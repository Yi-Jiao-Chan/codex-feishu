package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

type verifyOptions struct {
	Card, Group, Thread bool
	RequestID           string
}
type verifyGateway interface {
	SendCard(context.Context, string, string, string, string) (string, error)
	CreateGroup(context.Context, string, string, []string, string) (string, error)
}
type verifyDependencies struct {
	doctor       doctorDependencies
	appLock      func(string) (io.Closer, error)
	dbLock       func(string) (io.Closer, error)
	open         func(context.Context, string, int) (*store.Store, error)
	gateway      func(config.Config, string) verifyGateway
	emit         func(output)
	managed      func(context.Context, config.Config, string, string, bool) (verification.Result, error)
	managedGroup func(context.Context, config.Config, string, string, bool) (verification.GroupResult, error)
}

func defaultVerifyDependencies() verifyDependencies {
	return verifyDependencies{
		doctor:  defaultDoctorDependencies(),
		appLock: func(app string) (io.Closer, error) { return store.AcquireApplication(app) },
		dbLock:  func(path string) (io.Closer, error) { return store.AcquireListener(path) },
		open:    store.OpenExisting,
		gateway: func(cfg config.Config, secret string) verifyGateway { return feishu.New(cfg.Feishu.AppID, secret) },
		emit:    emit,
		managed: func(ctx context.Context, cfg config.Config, identity, id string, begin bool) (verification.Result, error) {
			path, err := control.Path(cfg.Feishu.AppID)
			if err != nil {
				return verification.Result{}, err
			}
			return control.VerifyCard(ctx, path, identity, id, begin)
		},
		managedGroup: func(ctx context.Context, cfg config.Config, identity, id string, begin bool) (verification.GroupResult, error) {
			path, err := control.Path(cfg.Feishu.AppID)
			if err != nil {
				return verification.GroupResult{}, err
			}
			return control.VerifyGroup(ctx, path, identity, id, begin)
		},
	}
}

func runVerify(ctx context.Context, args []string) int {
	return runVerifyWithDependencies(ctx, args, defaultVerifyDependencies())
}
func runVerifyWithDependencies(ctx context.Context, args []string, deps verifyDependencies) int {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	path := flags.String("config", "", "absolute TOML config path")
	target := flags.String("target", "self", "verify supports only self")
	card := flags.Bool("allow-test-card", false, "authorize owner-only test card; managed probes require --request-id")
	group := flags.Bool("allow-test-group", false, "authorize owner-and-bot test group; managed probes require --request-id")
	thread := flags.Bool("allow-test-thread", false, "authorize isolated thread verification; managed implementation pending")
	requestID := flags.String("request-id", "", "32 lowercase hexadecimal characters; query existing managed card/group records without writes when no authorization flags are supplied")
	_ = flags.Bool("json", false, "JSON output (default)")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || *path == "" || *target != "self" || *requestID != "" && !verification.ValidRequestID(*requestID) {
		deps.emit(output{Command: "verify", State: "invalid-arguments"})
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	o, code := inspectVerify(ctx, *path, verifyOptions{Card: *card, Group: *group, Thread: *thread, RequestID: *requestID}, deps)
	deps.emit(o)
	return code
}

// No flags means GET-only diagnostics through the existing managed instance
// and read-only snapshots. Native connections and receivers are never opened.
func inspectVerify(ctx context.Context, path string, opts verifyOptions, deps verifyDependencies) (output, int) {
	writes := opts.Card || opts.Group || opts.Thread
	var cfg config.Config
	var secret, identity string
	var identityErr error
	doctor := deps.doctor
	doctor.validatedConfig = func(validated config.Config, credential string) {
		cfg, secret = validated, credential
		identity, identityErr = control.Identity(path, validated)
	}
	o, code := inspectDoctor(ctx, path, false, !writes, doctor)
	o.Command = "verify"
	o.OK = false
	data, ok := o.Data.(map[string]any)
	if !ok {
		data = map[string]any{}
		o.Data = data
	}
	data["read_only"] = true
	data["verification_scope"] = "phase1-prerequisites-and-pending-acceptance"
	// Readiness, granted permissions and historical M0 observations cannot
	// prove current deployment, true App UI or the complete acceptance matrix.
	for i := range o.Checks {
		if o.Checks[i].ID == "verification.live_ui" {
			o.Checks[i].Required = true
		}
	}
	for _, id := range []string{"P1-01", "P1-02", "P1-03", "P1-04", "P1-05", "P1-06", "P1-07", "P1-08", "P1-09", "P1-10", "P1-11", "P1-12"} {
		o.Checks = append(o.Checks, check{"acceptance." + id, "pending", true, "current-deployment scenario and real UI evidence not yet integrated into verify"})
	}
	o.NextSteps = append(o.NextSteps, "full phase1 verify is pending; do not infer acceptance or configuration completion from diagnostic readiness")
	if !writes {
		o.Checks = append(o.Checks, check{"verification.test_writes", "skipped", false, "no test mutations authorized; no database creation/migration or extra receiver/native connection"})
	}
	live, managedInstance := data["instance"].(control.Snapshot)
	// A local journal read stays available when another diagnostic is
	// degraded; preserve its exit code and never authorize a mutation there.
	readProbe := managedInstance && !writes && verification.ValidRequestID(opts.RequestID) && identityErr == nil && identity != ""
	pendingCode := code
	if pendingCode == 0 {
		pendingCode = 5
	}
	if code != 0 && !readProbe {
		return o, code
	}
	o.State = "verification-pending"
	if managedInstance && (opts.Card || opts.Group || opts.RequestID != "") && !opts.Thread {
		if identityErr != nil || identity == "" {
			o.State = "verification-config-conflict"
			return o, 6
		}
		return inspectManagedVerification(ctx, cfg, identity, opts, live, deps, o, pendingCode)
	}
	if opts.RequestID != "" {
		o.State = "verification-managed-probe-unavailable"
		o.Checks = append(o.Checks, check{"verification.managed_card", "pending", true, "a matching managed receiver is required; no standalone fallback or test mutation performed"})
		return o, 5
	}
	if !writes {
		return o, 5
	}
	// Do not partially run card/group mutations when an unsupported isolated
	// thread was requested. A future managed verifier will own the whole flow.
	if opts.Thread {
		o.State = "verification-thread-not-integrated"
		o.Checks = append(o.Checks, check{"verification.managed_tests", "pending", true, "isolated managed test flow unavailable; none of the requested test mutations performed"})
		return o, 5
	}
	if _, live := data["instance"]; live {
		o.State = "verification-managed-tests-pending"
		o.Checks = append(o.Checks, check{"verification.managed_tests", "pending", true, "running instance retained; legacy standalone preparation disabled and no second receiver/native connection opened"})
		return o, 5
	}
	if identityErr != nil || identity == "" {
		o.State = "verification-config-conflict"
		return o, 6
	}
	applicationLock, err := deps.appLock(cfg.Feishu.AppID)
	if err != nil {
		o.State = "verification-instance-conflict"
		return o, 6
	}
	defer applicationLock.Close()
	listenerLock, err := deps.dbLock(cfg.Storage.DatabaseFile)
	if err != nil {
		o.State = "verification-listener-conflict"
		return o, 6
	}
	defer listenerLock.Close()
	// setup can finish between diagnosis and locking. Never send a test using
	// owner, paths, preferences or credential bytes from that stale snapshot.
	current, err := config.Load(path)
	if err != nil {
		o.State = "verification-config-conflict"
		return o, 6
	}
	currentIdentity, err := control.Identity(path, current)
	currentSecret, secretErr := current.Secret()
	if err != nil || secretErr != nil || currentIdentity != identity || currentSecret != secret {
		o.State = "verification-config-conflict"
		return o, 6
	}
	db, err := deps.open(ctx, cfg.Storage.DatabaseFile, 1)
	if err != nil {
		o.State = "verification-legacy-journal-required"
		o.Checks = append(o.Checks, check{"verification.legacy_journal", "fail", true, "legacy preparation requires a private existing schema 1 database; nothing created, chmodded or migrated"})
		return o, 3
	}
	defer db.Close()
	data["read_only"] = false
	data["verification_scope"] = "legacy-m0-preparation-only"
	o.NextSteps = []string{"legacy M0 preparation is not phase1 acceptance; use the retained historical M0 binary after this command exits, because current development m0-listen migrates its database; retain all pending phase1 checks"}
	return prepareLegacyVerification(ctx, cfg, db, deps.gateway(cfg, secret), opts, o)
}

func prepareLegacyVerification(ctx context.Context, cfg config.Config, db *store.Store, g verifyGateway, opts verifyOptions, o output) (output, int) {
	callCtx := ctx

	if opts.Card {
		key := "m0-test-card"
		fresh, e := db.Begin(ctx, key, "feishu.send_card")
		if e != nil {
			return o, 7
		}
		if fresh {
			token := store.ID()
			if _, e = db.Begin(ctx, "m0-test-card-token", "action-token"); e != nil {
				return o, 7
			}
			if e = db.Finish(ctx, "m0-test-card-token", "confirmed", token); e != nil {
				return o, 7
			}
			uuid, e := legacyProbeUUID(ctx, db, key)
			if e != nil {
				return o, 7
			}
			id, e := g.SendCard(callCtx, "open_id", cfg.Access.OwnerOpenID, cards.Probe(token), uuid)
			if e != nil {
				if finishErr := db.Finish(ctx, key, state(e), ""); finishErr != nil {
					return o, 7
				}
				o.Checks = append(o.Checks, check{"feishu.test_card", opStatus(state(e)), true, safeFeishu(e)})
			} else {
				if e = db.Finish(ctx, key, "confirmed", id); e != nil {
					return o, 7
				}
				o.Checks = append(o.Checks, check{"feishu.test_card", "pass", true, map[string]string{"message_id": id}})
			}
		} else {
			st, id, e := db.Operation(ctx, key)
			if e != nil {
				return o, 7
			}
			o.Checks = append(o.Checks, check{"feishu.test_card", opStatus(st), true, map[string]string{"state": st, "message_id": id, "replayed": "true"}})
		}
	} else {
		o.Checks = append(o.Checks, check{"feishu.test_card", "skipped", false, nil})
	}
	if opts.Group {
		key := "m0-test-group"
		fresh, e := db.Begin(ctx, key, "feishu.create_group")
		if e != nil {
			return o, 7
		}
		if fresh {
			uuid, e := legacyProbeUUID(ctx, db, key)
			if e != nil {
				return o, 7
			}
			id, e := g.CreateGroup(callCtx, "Codex Feishu · M0 联调", cfg.Access.OwnerOpenID, []string{cfg.Access.OwnerOpenID}, uuid)
			if e != nil {
				if finishErr := db.Finish(ctx, key, state(e), ""); finishErr != nil {
					return o, 7
				}
				o.Checks = append(o.Checks, check{"feishu.test_group", opStatus(state(e)), true, safeFeishu(e)})
			} else {
				if e = db.Finish(ctx, key, "confirmed", id); e != nil {
					return o, 7
				}
				o.Checks = append(o.Checks, check{"feishu.test_group", "pass", true, map[string]string{"chat_id": id}})
			}
		} else {
			st, id, e := db.Operation(ctx, key)
			if e != nil {
				return o, 7
			}
			o.Checks = append(o.Checks, check{"feishu.test_group", opStatus(st), true, map[string]string{"state": st, "chat_id": id, "replayed": "true"}})
		}
	} else {
		o.Checks = append(o.Checks, check{"feishu.test_group", "skipped", false, nil})
	}
	o.Checks = append(o.Checks, check{"m0.app_ui", "pending", true, "full App-origin queue/steer and remaining UI gate require live observation"})
	callbackState, callbackID, callbackErr := db.Operation(ctx, "m0.card_callback")
	if callbackErr == nil && callbackState == "confirmed" {
		o.Checks = append(o.Checks, check{"m0.card_callback", "pass", true, map[string]string{"message_id": callbackID, "source": "persisted actual owner callback"}})
	} else {
		o.Checks = append(o.Checks, check{"m0.card_callback", "pending", true, "click test card while m0-listen is running"})
	}
	o.State = "m0-pending"
	o.NextSteps = append(o.NextSteps, "start the matching M0 listener, then confirm its owner-only test card")
	for _, c := range o.Checks {
		if c.Required && c.Status == "fail" {
			return o, 4
		}
	}
	return o, 5
}

// Freeze deduplication UUID before calling Feishu; an unknown operation remains
// frozen and retains the original UUID for future explicit reconciliation.
func legacyProbeUUID(ctx context.Context, db *store.Store, operation string) (string, error) {
	key := operation + "-uuid"
	fresh, err := db.Begin(ctx, key, "feishu.request_uuid")
	if err != nil {
		return "", err
	}
	if fresh {
		id := store.ID()
		if err := db.Finish(ctx, key, "confirmed", id); err != nil {
			return "", err
		}
		return id, nil
	}
	status, id, err := db.Operation(ctx, key)
	if err != nil {
		return "", err
	}
	if status != "confirmed" || len(id) != 32 {
		return "", fmt.Errorf("legacy UUID not confirmed")
	}
	return id, nil
}
