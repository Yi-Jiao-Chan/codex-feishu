package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/pairing"
	"codex-feishu/internal/setup"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type pairFixtureLock struct{ closed atomic.Int32 }

func (l *pairFixtureLock) Close() error { l.closed.Add(1); return nil }

type pairLostBegin struct {
	*pairing.Manager
	requestID string
}

func (s *pairLostBegin) Begin(id string, ttl time.Duration) (pairing.Challenge, error) {
	s.requestID = id
	if _, err := s.Manager.Begin(id, ttl); err != nil {
		return pairing.Challenge{}, err
	}
	return pairing.Challenge{}, control.ErrUnavailable
}

func TestPairingLostBeginResponseCancelsKnownRequestWithoutNewChallenge(t *testing.T) {
	manager, _ := pairing.New("cli_pair_lost", "")
	defer manager.Close()
	session := &pairLostBegin{Manager: manager}
	var events []output
	if code := waitPair(context.Background(), session, "", 300*time.Second, "managed", func(o output) { events = append(events, o) }); code != 5 || len(events) != 1 || events[0].State != "pairing-state-unavailable" {
		t.Fatal("ambiguous start was replayed or misreported", code, events)
	}
	result, err := manager.State(session.requestID)
	if err != nil || result.State != pairing.StateCanceled || result.OwnerOpenID != "" {
		t.Fatal("unknown start retained active pairing", result, err)
	}
	challenge, err := manager.Begin(session.requestID, time.Second)
	if err != nil || challenge.Code != "" {
		t.Fatal("unknown-start cleanup retained the code", challenge, err)
	}
}

type pairFixtureGateway struct {
	ready              atomic.Bool
	listens, bots      atomic.Int32
	listening, stopped chan struct{}
	message            func(context.Context, *im.P2MessageReceiveV1) error
	failure            chan struct{}
}

func (g *pairFixtureGateway) Bot(context.Context) (string, error) {
	g.bots.Add(1)
	return "ou_fixture_bot_valid", nil
}
func (g *pairFixtureGateway) Ready() bool { return g.ready.Load() }
func (g *pairFixtureGateway) Listen(ctx context.Context, msg func(context.Context, *im.P2MessageReceiveV1) error, _ func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error)) error {
	g.listens.Add(1)
	g.message = msg
	g.ready.Store(true)
	close(g.listening)
	defer func() { g.ready.Store(false); close(g.stopped) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.failure:
		return errors.New("secret-response-must-not-output")
	}
}

func pairEvent(t *testing.T, app, body, kind, owner string) *im.P2MessageReceiveV1 {
	t.Helper()
	content, _ := json.Marshal(map[string]string{"text": body})
	b, _ := json.Marshal(map[string]any{"schema": "2.0", "header": map[string]string{"app_id": app, "event_type": "im.message.receive_v1"}, "event": map[string]any{"sender": map[string]any{"sender_type": "user", "sender_id": map[string]string{"open_id": owner}}, "message": map[string]string{"message_id": "om_fixture_pair", "chat_id": "oc_fixture_private", "chat_type": kind, "message_type": "text", "content": string(content)}}})
	var event im.P2MessageReceiveV1
	if err := json.Unmarshal(b, &event); err != nil {
		t.Fatal(err)
	}
	return &event
}

func readPairOutput(t *testing.T, ch <-chan output) output {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(3 * time.Second):
		t.Fatal("pair event missing")
		return output{}
	}
}

func TestTemporaryPairingOnlyListensAndJoinsBeforeReleasingLocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	appLock, dbLock := &pairFixtureLock{}, &pairFixtureLock{}
	g := &pairFixtureGateway{listening: make(chan struct{}), stopped: make(chan struct{}), failure: make(chan struct{})}
	events := make(chan output, 8)
	var cfg config.Config
	cfg.Storage.DatabaseFile = "/fixture/legacy.db"
	p := setup.PairPreparation{AppID: "cli_pair_fixture", Secret: "long-secret-never-output", Existing: &cfg, ExpectedOwner: "ou_fixture_owner_valid"}
	deps := pairDependencies{
		managed: func(context.Context, setup.PairPreparation) (pairSession, error) { return nil, control.ErrUnavailable },
		appLock: func(string) (io.Closer, error) { return appLock, nil },
		dbLock:  func(string) (io.Closer, error) { return dbLock, nil },
		gateway: func(app, secret string) pairGateway {
			if app != p.AppID || secret != p.Secret {
				t.Error("incorrect gateway configuration")
			}
			return g
		},
		emit: func(o output) { events <- o },
	}
	done := make(chan int, 1)
	go func() { done <- executePair(ctx, p, 10*time.Second, deps) }()
	waiting := readPairOutput(t, events)
	if waiting.State != "pairing-waiting" || waiting.OK {
		t.Fatal("challenge missing", waiting)
	}
	command := waiting.Data.(map[string]any)["pair_command"].(string)
	for _, event := range []*im.P2MessageReceiveV1{
		pairEvent(t, p.AppID, "ordinary user task", "p2p", p.ExpectedOwner),
		pairEvent(t, p.AppID, command, "group", p.ExpectedOwner),
		pairEvent(t, p.AppID, command, "p2p", "ou_other_fixture_user"),
	} {
		if err := g.message(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if len(events) != 0 {
		t.Fatal("unrecognized message produced pairing success")
	}
	if err := g.message(ctx, pairEvent(t, p.AppID, command, "p2p", p.ExpectedOwner)); err != nil {
		t.Fatal(err)
	}
	result := readPairOutput(t, events)
	if result.State != "paired" || !result.OK || result.Data.(pairing.Result).OwnerOpenID != p.ExpectedOwner {
		t.Fatal("matching owner not paired", result)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal("pair command failed", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("temporary listener not joined")
	}
	select {
	case <-g.stopped:
	default:
		t.Fatal("receiver survived command exit")
	}
	if appLock.closed.Load() != 1 || dbLock.closed.Load() != 1 || g.listens.Load() != 1 || g.bots.Load() != 1 {
		t.Fatal("locks or single receiver lifecycle incorrect")
	}
	b, _ := json.Marshal(result)
	if strings.Contains(string(b), command) || strings.Contains(string(b), p.Secret) || len(events) != 0 {
		t.Fatal("secret/code leaked or repeated events")
	}
}

func TestManagedPairingDoesNotAcquireLocksOrCreateGateway(t *testing.T) {
	m, _ := pairing.New("cli_pair_managed", "ou_fixture_owner_valid")
	defer m.Close()
	events := make(chan output, 4)
	deps := pairDependencies{
		managed: func(context.Context, setup.PairPreparation) (pairSession, error) { return m, nil },
		appLock: func(string) (io.Closer, error) {
			t.Error("managed pair opened another receiver lock")
			return nil, errors.New("unexpected")
		},
		dbLock: func(string) (io.Closer, error) {
			t.Error("managed pair opened another database lock")
			return nil, errors.New("unexpected")
		},
		gateway: func(string, string) pairGateway { t.Error("managed pair created another gateway"); return nil },
		emit:    func(o output) { events <- o },
	}
	done := make(chan int, 1)
	go func() {
		done <- executePair(context.Background(), setup.PairPreparation{AppID: "cli_pair_managed", ExpectedOwner: "ou_fixture_owner_valid"}, time.Second, deps)
	}()
	waiting := readPairOutput(t, events)
	data := waiting.Data.(map[string]any)
	if data["transport"] != "managed" {
		t.Fatal("wrong transport", data)
	}
	m.Handle(pairEvent(t, "cli_pair_managed", data["pair_command"].(string), "p2p", "ou_fixture_owner_valid"))
	if result := readPairOutput(t, events); result.State != "paired" || !result.OK {
		t.Fatal(result)
	}
	if code := <-done; code != 0 {
		t.Fatal("managed pair failed", code)
	}
}

func TestPairingRejectsReceiverConflictsBeforeOpeningConnection(t *testing.T) {
	for _, reason := range []string{"identity", "unsafe", "unsupported", "not-ready", "app-lock", "legacy-lock"} {
		t.Run(reason, func(t *testing.T) {
			calls := 0
			gatewayCalls := 0
			app := &pairFixtureLock{}
			var got output
			p := setup.PairPreparation{AppID: "cli_pair_conflict", Existing: &config.Config{}}
			deps := pairDependencies{
				managed: func(context.Context, setup.PairPreparation) (pairSession, error) {
					switch reason {
					case "identity":
						return nil, control.ErrIdentity
					case "unsafe":
						return nil, control.ErrUnsafe
					case "unsupported":
						return nil, control.ErrUnsupported
					case "not-ready":
						return nil, errPairNotReady
					}
					return nil, control.ErrUnavailable
				},
				appLock: func(string) (io.Closer, error) {
					calls++
					if reason == "app-lock" {
						return nil, errors.New("held")
					}
					return app, nil
				},
				dbLock:  func(string) (io.Closer, error) { return nil, errors.New("legacy held") },
				gateway: func(string, string) pairGateway { gatewayCalls++; return nil },
				emit:    func(o output) { got = o },
			}
			code := executePair(context.Background(), p, time.Second, deps)
			want := 6
			if reason == "unsupported" || reason == "not-ready" {
				want = 3
			}
			if code != want || got.OK || gatewayCalls != 0 {
				t.Fatal("conflict opened transport", got, code, gatewayCalls)
			}
			if reason != "app-lock" && reason != "legacy-lock" && calls != 0 {
				t.Fatal("managed error fell back to temporary receiver")
			}
			if reason == "legacy-lock" && app.closed.Load() != 1 {
				t.Fatal("application lock leaked")
			}
		})
	}
}

func TestPairingExpiryAndCLIInterruptCancelPendingChallenge(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "expire", true: "interrupt"}[interrupt], func(t *testing.T) {
			m, _ := pairing.New("cli_pair_lifecycle", "")
			defer m.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			events := make(chan output, 4)
			done := make(chan int, 1)
			go func() { done <- waitPair(ctx, m, "", time.Second, "managed", func(o output) { events <- o }) }()
			waiting := readPairOutput(t, events)
			id := waiting.Data.(map[string]any)["request_id"].(string)
			if interrupt {
				cancel()
			}
			result := readPairOutput(t, events)
			if result.OK || result.State != "pairing-expired" && !interrupt || interrupt && result.State != "pairing-interrupted" {
				t.Fatal("wrong terminal pairing event", result)
			}
			if code := <-done; code != 5 {
				t.Fatal("expiry/interruption succeeded", code)
			}
			state, err := m.State(id)
			if err != nil || state.State == pairing.StateWaiting || state.OwnerOpenID != "" {
				t.Fatal("challenge survived command exit", state, err)
			}
		})
	}
}

func TestTemporaryPairingUnexpectedReceiverExitCannotClaimSuccess(t *testing.T) {
	g := &pairFixtureGateway{listening: make(chan struct{}), stopped: make(chan struct{}), failure: make(chan struct{})}
	events := make(chan output, 4)
	done := make(chan int, 1)
	lock := &pairFixtureLock{}
	deps := pairDependencies{managed: func(context.Context, setup.PairPreparation) (pairSession, error) { return nil, control.ErrUnavailable }, appLock: func(string) (io.Closer, error) { return lock, nil }, gateway: func(string, string) pairGateway { return g }, emit: func(o output) { events <- o }}
	go func() {
		done <- executePair(context.Background(), setup.PairPreparation{AppID: "cli_pair_exit"}, 10*time.Second, deps)
	}()
	if o := readPairOutput(t, events); o.State != "pairing-waiting" {
		t.Fatal(o)
	}
	close(g.failure)
	o := readPairOutput(t, events)
	if o.OK || o.State != "pairing-interrupted" {
		t.Fatal("dead listener retained active pairing", o)
	}
	if code := <-done; code != 5 || lock.closed.Load() != 1 {
		t.Fatal("listener exit not joined or lock leaked", code)
	}
	b, _ := json.Marshal(o)
	if strings.Contains(string(b), "secret-response") {
		t.Fatal("SDK error payload leaked")
	}
}
