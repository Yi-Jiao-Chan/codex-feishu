package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/pairing"
	"codex-feishu/internal/setup"
	"codex-feishu/internal/store"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type pairSession interface {
	Begin(string, time.Duration) (pairing.Challenge, error)
	State(string) (pairing.Result, error)
	Cancel(string) (pairing.Result, error)
}
type pairGateway interface {
	Bot(context.Context) (string, error)
	Ready() bool
	Listen(context.Context, func(context.Context, *im.P2MessageReceiveV1) error, func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error)) error
}
type pairDependencies struct {
	managed func(context.Context, setup.PairPreparation) (pairSession, error)
	appLock func(string) (io.Closer, error)
	dbLock  func(string) (io.Closer, error)
	gateway func(string, string) pairGateway
	emit    func(output)
}

var errPairNotReady = errors.New("existing receiver is not ready for pairing")

type managedPair struct {
	ctx            context.Context
	path, identity string
}

func (p managedPair) Begin(id string, ttl time.Duration) (pairing.Challenge, error) {
	return control.PairBegin(p.ctx, p.path, p.identity, id, ttl)
}
func (p managedPair) State(id string) (pairing.Result, error) {
	return control.PairState(p.ctx, p.path, p.identity, id)
}
func (p managedPair) Cancel(id string) (pairing.Result, error) {
	// Best-effort local cleanup also works after the CLI's context is canceled.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return control.PairCancel(ctx, p.path, p.identity, id)
}

func defaultPairDependencies() pairDependencies {
	return pairDependencies{
		managed: func(ctx context.Context, p setup.PairPreparation) (pairSession, error) {
			if p.Existing == nil {
				return nil, control.ErrUnavailable
			}
			identity, err := control.Identity(p.ConfigPath, *p.Existing)
			if err != nil {
				return nil, control.ErrIdentity
			}
			endpoint, err := control.Path(p.AppID)
			if err != nil {
				return nil, control.ErrUnsafe
			}
			health, err := control.Query(ctx, endpoint, identity)
			if err != nil {
				return nil, err
			}
			if !health.PairingSupported {
				return nil, control.ErrUnsupported
			}
			if !health.Running || !health.FeishuConnected {
				return nil, errPairNotReady
			}
			return managedPair{ctx, endpoint, identity}, nil
		},
		appLock: func(app string) (io.Closer, error) { return store.AcquireApplication(app) },
		dbLock:  func(path string) (io.Closer, error) { return store.AcquireListener(path) },
		gateway: func(app, secret string) pairGateway { return feishu.New(app, secret) },
		emit:    emit,
	}
}

func runPair(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("setup.pair", flag.ContinueOnError)
	input := flags.String("input", "", "absolute partial setup JSON path")
	configPath := flags.String("config", "", "optional absolute existing config path")
	timeout := flags.Int("timeout", 300, "one-time code lifetime, 1–300 seconds")
	_ = flags.Bool("json", false, "JSON line events (default)")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || *input == "" || *timeout < 1 || *timeout > 300 {
		emit(output{Command: "setup.pair", State: "invalid-arguments"})
		return 2
	}
	in, err := setup.LoadInput(*input)
	if err != nil {
		emit(output{Command: "setup.pair", State: "invalid-input"})
		return 2
	}
	if *configPath == "" {
		paths, err := setup.DefaultPaths()
		if err != nil {
			emit(output{Command: "setup.pair", State: "paths-unavailable"})
			return 3
		}
		*configPath = paths.ConfigFile
	}
	prepared, err := setup.PreparePair(in, *configPath)
	if err != nil {
		code, state := 2, "pair-input-invalid"
		if errors.Is(err, setup.ErrPairIdentityConflict) {
			code, state = 6, "pair-identity-conflict"
		}
		emit(output{Command: "setup.pair", State: state, Checks: []check{{"pair.input", "fail", true, "partial input, private secret or existing identity/ref configuration could not be verified"}}})
		return code
	}
	return executePair(ctx, prepared, time.Duration(*timeout)*time.Second, defaultPairDependencies())
}

func executePair(ctx context.Context, p setup.PairPreparation, ttl time.Duration, deps pairDependencies) int {
	failure := func(state string, code int) int { deps.emit(output{Command: "setup.pair", State: state}); return code }
	session, err := deps.managed(ctx, p)
	if err == nil {
		return waitPair(ctx, session, p.ExpectedOwner, ttl, "managed", deps.emit)
	}
	if !errors.Is(err, control.ErrUnavailable) {
		if errors.Is(err, control.ErrIdentity) || errors.Is(err, control.ErrUnsafe) {
			return failure("pair-instance-conflict", 6)
		}
		if errors.Is(err, control.ErrUnsupported) {
			return failure("pair-instance-upgrade-required", 3)
		}
		return failure("pair-receiver-not-ready", 3)
	}
	appLock, err := deps.appLock(p.AppID)
	if err != nil {
		return failure("pair-receiver-conflict", 6)
	}
	defer appLock.Close()
	if p.Existing != nil {
		// Older M0 listeners predate the application lock. Honor their DB lock
		// before opening a temporary connection for an existing deployment.
		lock, err := deps.dbLock(p.Existing.Storage.DatabaseFile)
		if err != nil {
			return failure("pair-listener-conflict", 6)
		}
		defer lock.Close()
	}
	gateway := deps.gateway(p.AppID, p.Secret)
	preflight, cancel := context.WithTimeout(ctx, 30*time.Second)
	bot, err := gateway.Bot(preflight)
	cancel()
	if err != nil || !config.ValidOpenID(bot) {
		return failure("pair-feishu-unavailable", 4)
	}
	manager, err := pairing.NewWithOptions(p.AppID, p.ExpectedOwner, pairing.Options{BotOpenID: bot})
	if err != nil {
		return failure("pair-input-invalid", 2)
	}
	defer manager.Close()
	listenCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- gateway.Listen(listenCtx, func(_ context.Context, e *im.P2MessageReceiveV1) error { manager.Handle(e); return nil }, func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "info", Content: "当前仅进行本机配对"}}, nil
		})
	}()
	joined := false
	defer func() {
		stop()
		if !joined {
			<-done
		}
	}()
	readyTimer := time.NewTimer(30 * time.Second)
	defer readyTimer.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for !gateway.Ready() {
		select {
		case <-ctx.Done():
			return failure("pairing-interrupted", 5)
		case <-readyTimer.C:
			return failure("pair-receiver-not-ready", 4)
		case <-done:
			joined = true
			return failure("pair-feishu-unavailable", 4)
		case <-tick.C:
		}
	}
	// A stopped temporary listener must abort pairing instead of leaving a
	// printed code silently waiting until expiration. Join it on every exit.
	opCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		select {
		case <-done:
			joined = true
			cancelOperation()
		case <-opCtx.Done():
		}
	}()
	code := waitPair(opCtx, manager, p.ExpectedOwner, ttl, "temporary", deps.emit)
	cancelOperation()
	<-monitorDone
	return code
}

func waitPair(ctx context.Context, session pairSession, expectedOwner string, ttl time.Duration, transport string, write func(output)) int {
	id, err := pairing.GenerateRequestID()
	if err != nil {
		write(output{Command: "setup.pair", State: "pairing-unavailable"})
		return 7
	}
	// Begin can commit locally even if the Unix response is lost. Cleanup the
	// known random request ID on every exit, including an ambiguous Begin.
	defer session.Cancel(id)
	challenge, err := session.Begin(id, ttl)
	if err != nil {
		code, state := 5, "pairing-state-unavailable"
		if errors.Is(err, pairing.ErrBusy) {
			code, state = 6, "pairing-already-active"
		} else if errors.Is(err, pairing.ErrInvalid) {
			code, state = 2, "invalid-pairing-request"
		} else if errors.Is(err, pairing.ErrClosed) || errors.Is(err, control.ErrUnsupported) {
			code, state = 3, "pairing-unavailable"
		}
		write(output{Command: "setup.pair", State: state})
		return code
	}
	if challenge.RequestID != id || challenge.State != "waiting" || challenge.Code == "" {
		write(output{Command: "setup.pair", State: "pairing-state-unavailable"})
		return 5
	}
	// This is the only code-bearing CLI event. Neither the result nor health
	// carries the ephemeral token, and no configuration/identity is written.
	write(output{Command: "setup.pair", State: "pairing-waiting", Data: map[string]any{"request_id": id, "pair_command": "/pair " + challenge.Code, "expires_at_ms": challenge.ExpiresAtMS, "transport": transport}, NextSteps: []string{"send this one-time command in the bot's private chat; do not save the code in logs or configuration"}})
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			write(output{Command: "setup.pair", State: "pairing-interrupted"})
			return 5
		case <-tick.C:
			result, err := session.State(id)
			if err != nil || result.RequestID != id {
				write(output{Command: "setup.pair", State: "pairing-state-unavailable"})
				return 5
			}
			switch result.State {
			case "waiting":
				if time.Now().UnixMilli() >= challenge.ExpiresAtMS {
					write(output{Command: "setup.pair", State: "pairing-expired"})
					return 5
				}
			case "confirmed":
				if !config.ValidOpenID(result.OwnerOpenID) || expectedOwner != "" && result.OwnerOpenID != expectedOwner {
					write(output{Command: "setup.pair", State: "pair-identity-conflict"})
					return 6
				}
				write(output{Command: "setup.pair", OK: true, State: "paired", Data: result, NextSteps: []string{"copy owner_open_id into the reviewed setup input; pairing did not write or replace configuration"}})
				return 0
			case "expired", "canceled":
				write(output{Command: "setup.pair", State: "pairing-" + result.State})
				return 5
			default:
				write(output{Command: "setup.pair", State: "pairing-state-unavailable"})
				return 5
			}
		}
	}
}
