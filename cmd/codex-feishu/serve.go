package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sync"
	"time"

	"codex-feishu/compat"
	"codex-feishu/internal/app"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/pairing"
	"codex-feishu/internal/setup"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

func runServe(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	path := flags.String("config", "", "absolute TOML config path")
	candidate := flags.Bool("candidate-test", false, "candidate runtime: refuse database migration")
	_ = flags.Bool("json", false, "JSON events (default)")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || *path == "" {
		emit(output{Command: "serve", State: "config-required"})
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		emit(output{Command: "serve", State: "config-invalid", Checks: []check{{"config", "fail", true, err.Error()}}})
		return 2
	}
	beforeIdentity, err := control.Identity(*path, cfg)
	if err != nil {
		emit(output{Command: "serve", State: "config-identity-invalid"})
		return 6
	}
	// The application lock is independent of database choice. Acquire both
	// before opening/migrating the database or starting a Feishu ws receiver.
	appLock, err := store.AcquireApplication(cfg.Feishu.AppID)
	if err != nil {
		emit(output{Command: "serve", State: "application-lock-conflict"})
		return 6
	}
	defer appLock.Close()
	dbLock, err := store.AcquireListener(cfg.Storage.DatabaseFile)
	if err != nil {
		emit(output{Command: "serve", State: "listener-lock-conflict"})
		return 6
	}
	defer dbLock.Close()
	current, err := validateLockedServeConfig(*path, beforeIdentity)
	if err != nil {
		state, code := "configuration-changed-before-start", 6
		if errors.Is(err, setup.ErrApplyPending) {
			state, code = "setup-recovery-pending", 5
		}
		if errors.Is(err, setup.ErrMigrationPending) {
			state, code = "migration-recovery-pending", 5
		}
		emit(output{Command: "serve", State: state, NextSteps: []string{"complete or recover the original setup transaction before starting the bridge; no connections or migration started"}})
		return code
	}
	cfg = current
	if *candidate {
		if err := setup.CheckCandidateDatabase(ctx, cfg.Storage.DatabaseFile); err != nil {
			emit(output{Command: "serve", State: "candidate-database-invalid", Checks: []check{{"candidate.database", "fail", true, err.Error()}}})
			return 3
		}
	}
	var preview *previewHTTP
	if cfg.FilePreview.Enabled {
		preview, err = startFilePreview(cfg)
		if err != nil {
			emit(output{Command: "serve", State: "file-preview-unavailable", Checks: []check{{"file_preview", "fail", true, err.Error()}}})
			return 3
		}
		defer preview.Close()
	}
	secret, err := cfg.Secret()
	if err != nil {
		emit(output{Command: "serve", State: "secret-unavailable"})
		return 4
	}
	gateway := feishu.New(cfg.Feishu.AppID, secret)
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	bot, err := gateway.Bot(checkCtx)
	cancel()
	if err != nil {
		emit(output{Command: "serve", State: "feishu-unavailable", Checks: []check{{"feishu.bot", "fail", true, safeFeishu(err)}}})
		return 4
	}
	var db *store.Store
	if *candidate {
		// Existing candidate state is opened without invoking migrations, even
		// if its schema changed after the initial locked preflight.
		if _, statErr := os.Lstat(cfg.Storage.DatabaseFile); errors.Is(statErr, os.ErrNotExist) {
			db, err = store.Open(cfg.Storage.DatabaseFile)
		} else {
			db, err = store.OpenExisting(ctx, cfg.Storage.DatabaseFile, store.SchemaVersion)
		}
	} else {
		db, err = store.Open(cfg.Storage.DatabaseFile)
	}
	if err != nil {
		emit(output{Command: "serve", State: "storage-unavailable"})
		return 7
	}
	defer db.Close()
	pairManager, err := pairing.NewWithOptions(cfg.Feishu.AppID, cfg.Access.OwnerOpenID, pairing.Options{BotOpenID: bot})
	if err != nil {
		emit(output{Command: "serve", State: "pairing-unavailable"})
		return 7
	}
	defer pairManager.Close()
	service := &app.Service{Config: cfg, Store: db, Gateway: gateway, BotID: bot, Pairing: pairManager, Connect: serveNativeConnect(cfg.Codex.CLIPath, codexclient.Discover, codexclient.Connect, cfg.Creation.Enabled)}
	if preview != nil {
		service.PreparePublic = func(ctx context.Context, _ string, body string) (string, error) {
			return preview.preview.Rewrite(ctx, body)
		}
	}
	var noticeMu sync.Mutex
	last := map[string]time.Time{}
	service.Notice = func(code string) {
		noticeMu.Lock()
		defer noticeMu.Unlock()
		now := time.Now()
		if now.Sub(last[code]) < 30*time.Second {
			return
		}
		last[code] = now
		emit(output{Command: "serve", State: code})
	}
	identity, err := control.Identity(*path, cfg)
	if err != nil {
		emit(output{Command: "serve", State: "control-identity-unavailable"})
		return 6
	}
	binaryHash, err := verification.RunningBinarySHA256()
	if err != nil {
		emit(output{Command: "serve", State: "verification-artifact-unavailable"})
		return 3
	}
	verificationManager := &verification.Manager{Store: db, Gateway: gateway, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: cfg.Access.OwnerOpenID, Commit: commit, BinarySHA256: binaryHash, Ready: func() bool { return gateway.Ready() && service.HealthReadyForVerification() }}
	service.Verification = verificationManager
	verificationGroups := &verification.GroupManager{Store: db, Gateway: gateway, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: cfg.Access.OwnerOpenID, BotID: bot, Commit: commit, BinarySHA256: binaryHash, Ready: func() bool { return gateway.Ready() && service.HealthReadyForVerification() }}
	service.VerificationGroups = verificationGroups
	// This dedicated root is local service state, not a model selected by
	// management requests. No model/listener/workspace starts until Begin.
	physicalDatabase, err := filepath.EvalSymlinks(cfg.Storage.DatabaseFile)
	if err != nil {
		emit(output{Command: "serve", State: "verification-resources-unavailable"})
		return 7
	}
	verificationRoot := filepath.Join(filepath.Dir(physicalDatabase), "verification-native")
	if err := os.Mkdir(verificationRoot, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		emit(output{Command: "serve", State: "verification-resources-unavailable"})
		return 7
	}
	nativeVerification := &verification.NativeManager{Store: db, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: cfg.Access.OwnerOpenID, Commit: commit, BinarySHA256: binaryHash, Current: service.VerificationCreator}
	verificationThreads, err := verification.NewNativeSessions(ctx, nativeVerification, verificationRoot)
	if err != nil {
		emit(output{Command: "serve", State: "verification-resources-unavailable"})
		return 7
	}
	defer verificationThreads.Close()
	service.VerificationThreads = verificationThreads
	verificationBindings := &verification.BindingManager{Sessions: verificationThreads, Groups: verificationGroups, Activate: service.ActivateVerificationBinding, ActorStarted: service.VerificationActorStarted}
	service.VerificationBindings = verificationBindings
	endpoint, err := control.Path(cfg.Feishu.AppID)
	if err != nil {
		emit(output{Command: "serve", State: "control-endpoint-unavailable"})
		return 6
	}
	server, err := control.Listen(ctx, endpoint, identity, service.Health, pairManager)
	if err != nil {
		emit(output{Command: "serve", State: "control-endpoint-conflict"})
		return 6
	}
	defer server.Close()
	server.Verification = verificationManager
	server.VerificationGroups = verificationGroups
	server.VerificationThreads = verificationThreads
	server.VerificationBindings = verificationBindings
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var workers sync.WaitGroup
	results := make(chan error, 3)
	workers.Add(2)
	go func() { defer workers.Done(); results <- service.Run(runCtx) }()
	go func() { defer workers.Done(); results <- server.Serve(runCtx) }()
	if preview != nil {
		workers.Add(1)
		go func() { defer workers.Done(); results <- preview.Serve(runCtx) }()
	}
	emit(output{Command: "serve", State: "starting", Data: map[string]any{"host_id": cfg.Host.ID, "store_schema": store.SchemaVersion, "phase1_released": phase1Released, "candidate_test_deployment": *candidate, "control_endpoint": endpoint}})
	err = <-results
	stop()
	server.Close()
	workers.Wait()
	if ctx.Err() != nil {
		emit(output{Command: "serve", OK: true, State: "stopped"})
		return 0
	}
	if err != nil {
		emit(output{Command: "serve", State: "stopped-with-error", NextSteps: []string{"run doctor; native daemon and Feishu configuration remain unchanged"}})
		return 7
	}
	return 0
}

// The caller holds both receiver locks. Never connect or migrate using a
// snapshot read while setup still owned those locks, or an unfinished journal.
func validateLockedServeConfig(path string, identity string) (config.Config, error) {
	current, err := config.Load(path)
	if err != nil {
		return config.Config{}, control.ErrIdentity
	}
	currentIdentity, err := control.Identity(path, current)
	if err != nil || currentIdentity != identity {
		return config.Config{}, control.ErrIdentity
	}
	if err := setup.CheckApplyReady(path); err != nil {
		return config.Config{}, err
	}
	return current, nil
}

// Each generation rechecks the existing daemon before opening its native
// connection. The injected calls are the same production discovery and dial
// operations; tests can exercise this boundary without a shared daemon.
type discoveredNative struct {
	*codexclient.Client
	serverVersion string
}

// AppServerVersion belongs to this discovered connection generation. Neither
// the CLI version nor a caller's environment authorizes filesystem protocols.
func (n *discoveredNative) AppServerVersion() string { return n.serverVersion }

func (n *discoveredNative) Retry(ctx context.Context, thread, attempt string) (codexclient.Turn, error) {
	if n.serverVersion != codexclient.ManualRetrySchemaVersion {
		return codexclient.Turn{}, &codexclient.RPCError{Code: -32601}
	}
	return n.Client.Retry(ctx, thread, attempt)
}

func serveNativeConnect(cli string, discover func(context.Context, string) (codexclient.Daemon, error), connect func(context.Context, string) (*codexclient.Client, error), creationEnabled ...bool) func(context.Context) (app.ServiceNative, error) {
	return func(ctx context.Context) (app.ServiceNative, error) {
		inspectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		daemon, err := discover(inspectCtx, cli)
		if err != nil {
			return nil, errors.New("existing daemon unavailable")
		}
		if !compat.VerifiedCodexVersion(daemon.AppServerVersion) {
			return nil, errors.New("native version is not in the verified compatibility list")
		}
		if len(creationEnabled) > 0 && creationEnabled[0] && !compat.VerifiedCreationCodexVersion(daemon.AppServerVersion) {
			return nil, errors.New("native creation protocol version is not verified")
		}
		client, err := connect(ctx, daemon.SocketPath)
		if err != nil {
			return nil, err
		}
		return &discoveredNative{Client: client, serverVersion: daemon.AppServerVersion}, nil
	}
}
