package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"
	"time"

	"codex-feishu/compat"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type doctorGateway interface {
	Bot(context.Context) (string, error)
	GrantedTenantScopes(context.Context) ([]string, error)
}

var errDoctorNoDatabase = errors.New("diagnostic database not initialized")

type doctorDependencies struct {
	// Local verifier captures the exact validated snapshot used by these GETs.
	// Credentials remain in memory and are never included in diagnostic output.
	validatedConfig func(config.Config, string)
	discover        func(context.Context, string) (codexclient.Daemon, error)
	health          func(context.Context, string, config.Config) (control.Snapshot, error)
	gateway         func(config.Config, string) doctorGateway
	snapshot        func(context.Context, config.Config) (store.PersistentHealth, error)
	// Used only by the explicitly requested standalone --codex-only probe.
	nativeProbe func(context.Context, codexclient.Daemon) (int, error)
}

func defaultDoctorDependencies() doctorDependencies {
	return doctorDependencies{
		discover: codexclient.Discover,
		health: func(ctx context.Context, path string, cfg config.Config) (control.Snapshot, error) {
			id, err := control.Identity(path, cfg)
			if err != nil {
				return control.Snapshot{}, control.ErrIdentity
			}
			endpoint, err := control.Path(cfg.Feishu.AppID)
			if err != nil {
				return control.Snapshot{}, control.ErrUnsafe
			}
			return control.Query(ctx, endpoint, id)
		},
		gateway: func(cfg config.Config, secret string) doctorGateway { return feishu.New(cfg.Feishu.AppID, secret) },
		snapshot: func(ctx context.Context, cfg config.Config) (store.PersistentHealth, error) {
			if _, err := os.Stat(cfg.Storage.DatabaseFile); errors.Is(err, os.ErrNotExist) {
				return store.PersistentHealth{}, errDoctorNoDatabase
			}
			db, schema, err := store.OpenReadOnly(ctx, cfg.Storage.DatabaseFile)
			if err != nil {
				return store.PersistentHealth{}, err
			}
			defer db.Close()
			return db.Health(ctx, schema, cfg.Host.ID, cfg.Feishu.AppID)
		},
		nativeProbe: func(ctx context.Context, daemon codexclient.Daemon) (int, error) {
			client, err := codexclient.Connect(ctx, daemon.SocketPath)
			if err != nil {
				return 0, err
			}
			defer client.Close()
			threads, err := client.List(ctx)
			return len(threads), err
		},
	}
}

func runDoctor(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	path := flags.String("config", "", "absolute TOML config path")
	codexOnly := flags.Bool("codex-only", false, "standalone read-only Codex handshake and thread-list probe")
	requireRunning := flags.Bool("require-running", false, "also require a matching, healthy running bridge")
	_ = flags.Bool("json", false, "JSON output (default)")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || (*codexOnly && (*path != "" || *requireRunning)) || (!*codexOnly && *path == "") {
		emit(output{Command: "doctor", State: "invalid-arguments"})
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	o, code := inspectDoctor(ctx, *path, *codexOnly, *requireRunning, defaultDoctorDependencies())
	emit(o)
	return code
}

// Configuration diagnostics never connect another native client or Feishu
// receiver, migrate a database, or issue conversation/group/card writes.
// Before install, absence of a running bridge is informational. Once an
// instance exists, its unresolved state is required even without the flag.
func inspectDoctor(ctx context.Context, path string, codexOnly, requireRunning bool, deps doctorDependencies) (output, int) {
	o := output{Command: "doctor", State: "checking"}
	code := 0
	fail := func(exit int) {
		if code == 0 || code == 5 {
			code = exit
		}
	}
	data := map[string]any{"read_only": true, "ui_verified": false}
	var cfg config.Config
	var secret string
	var err error
	if !codexOnly {
		cfg, err = config.Load(path)
		if err != nil {
			o.State, o.Checks = "config-invalid", []check{{"config", "fail", true, "configuration schema, identity or paths are invalid"}}
			return o, 2
		}
		o.Checks = append(o.Checks, check{"config", "pass", true, "validated phase1 configuration and allowed directories"})
		data["interactions"] = map[string]any{"enabled": cfg.Interactions.Enabled, "enabled_types": cfg.Interactions.EnabledTypes, "ui_verified": false, "required_protocol": "verified shared app-server"}
		preview := cfg.FilePreviewLimits()
		data["file_preview"] = map[string]any{
			"enabled": preview.Enabled, "base_url": preview.BaseURL,
			"listen_address": preview.ListenAddress, "allowed_roots_count": len(preview.AllowedRoots),
			"max_file_bytes": preview.MaxFileBytes, "ui_verified": false,
		}
		if preview.Enabled {
			if _, err := cfg.FilePreviewSecret(); err != nil {
				o.State, o.Data = "file-preview-secret-unavailable", data
				o.Checks = append(o.Checks, check{"file_preview.secret", "fail", true, "preview access code requires an owner-only regular 0600 file; value omitted"})
				return o, 3
			}
			o.Checks = append(o.Checks, check{"file_preview.secret", "pass", true, "private preview access credential verified; value omitted"},
				check{"file_preview.browser", "pending", false, "IP/network reachability, browser sign-in and real Feishu clicks require separate client acceptance"})
		}
		secret, err = cfg.Secret()
		if err != nil {
			o.State, o.Checks = "secret-unavailable", append(o.Checks, check{"feishu.secret", "fail", true, "secret file must be a private regular file containing one value"})
			return o, 4
		}
		o.Checks = append(o.Checks, check{"feishu.secret", "pass", true, "private credential file read; value omitted"})
		if deps.validatedConfig != nil {
			deps.validatedConfig(cfg, secret)
		}
	}
	cli := "auto"
	if !codexOnly {
		cli = cfg.Codex.CLIPath
	}
	daemon, err := deps.discover(ctx, cli)
	compatible := err == nil && daemon.Status == "running" && compat.VerifiedCodexVersion(daemon.AppServerVersion)
	if err != nil {
		o.Checks = append(o.Checks, check{"codex.daemon", "fail", true, "existing shared daemon unavailable; no daemon started"})
		fail(3)
	} else {
		data["codex_daemon"] = daemon
		o.Checks = append(o.Checks, check{"codex.daemon", "pass", true, "inspected existing shared daemon using the configured CLI"})
		verifiedVersions := strings.Join(compat.VerifiedCodexVersions(), ", ")
		if !compatible {
			o.Checks = append(o.Checks, check{"codex.compatibility", "fail", true, "actual server version " + daemon.AppServerVersion + " is outside the verified compatibility versions: " + verifiedVersions})
			fail(3)
		} else {
			o.Checks = append(o.Checks, check{"codex.compatibility", "pass", true, "actual shared server version " + daemon.AppServerVersion + "; verified compatibility versions: " + verifiedVersions})
		}
	}
	if codexOnly {
		if compatible {
			count, err := deps.nativeProbe(ctx, daemon)
			if err != nil {
				o.Checks = append(o.Checks, check{"codex.handshake_and_list", "fail", true, "read-only native probe unavailable"})
				fail(3)
			} else {
				o.Checks = append(o.Checks, check{"codex.handshake_and_list", "pass", true, map[string]any{"count": count, "full_pagination": true}})
			}
		}
		o.State, o.OK, o.Data = "codex-readonly-ready", code == 0, data
		if code != 0 {
			o.State = "prerequisite-failed"
		}
		return o, code
	}
	data["host_id"] = cfg.Host.ID
	live, liveErr := deps.health(ctx, path, cfg)
	switch {
	case liveErr == nil:
		// Readiness follows the validated config even when an older management
		// response omits a recently added discovery field.
		live.DiscoveryEnabled = cfg.Discovery.Enabled
		liveOutput, liveCode := liveStatus(live)
		o.Checks = append(o.Checks, liveOutput.Checks...)
		data["instance"] = live
		if liveCode != 0 {
			fail(liveCode)
			o.NextSteps = append(o.NextSteps, liveOutput.NextSteps...)
		}
	case errors.Is(liveErr, control.ErrIdentity), errors.Is(liveErr, control.ErrUnsafe):
		o.State, o.Data = "instance-conflict", data
		o.Checks = append(o.Checks, check{"instance.live_health", "fail", true, "control identity or endpoint ownership differs; explicitly repair/restart the bridge"})
		return o, 6
	default:
		o.Checks = append(o.Checks, check{"instance.live_health", "pending", requireRunning, "managed instance unavailable; no extra receiver/native connection opened"})
		if requireRunning {
			fail(5)
		}
		if health, err := deps.snapshot(ctx, cfg); err == nil {
			data["durable"] = health
			o.Checks = append(o.Checks, check{"store.snapshot", "pass", false, "read-only aggregate snapshot; not proof of runtime readiness"})
		} else if errors.Is(err, errDoctorNoDatabase) {
			o.Checks = append(o.Checks, check{"store.snapshot", "pending", false, "database absent or unreadable; nothing created or migrated"})
		} else {
			o.Checks = append(o.Checks, check{"store.snapshot", "fail", true, "existing database is unreadable, unsupported or belongs to another deployment; nothing migrated"})
			fail(3)
		}
	}
	// GET-only authentication and scope inspection are independent of the
	// existing event receiver. Scoped permissions do not prove UI capability.
	gateway := deps.gateway(cfg, secret)
	if bot, err := gateway.Bot(ctx); err != nil || !config.ValidOpenID(bot) {
		o.Checks = append(o.Checks, check{"feishu.bot", "fail", true, "tenant authentication or published bot identity unavailable"})
		fail(4)
	} else {
		o.Checks = append(o.Checks, check{"feishu.bot", "pass", true, "tenant authentication and published bot identity observed; identity omitted"})
	}
	scopes, scopeErr := gateway.GrantedTenantScopes(ctx)
	if scopeErr != nil {
		o.Checks = append(o.Checks, check{"feishu.granted_scopes", "pending", true, "authorized tenant scope metadata unavailable or ambiguous; inspect application permissions and publish status"})
		fail(5)
	} else {
		o.Checks = append(o.Checks, check{"feishu.granted_scopes", "pass", true, "read current tenant grants; user grants and unapproved grants do not count"})
		capabilities := compat.EvaluatePermissions(scopes)
		data["feishu_capabilities"] = capabilities
		for _, capability := range capabilities {
			o.Checks = append(o.Checks, check{"feishu.permission." + capability.ID, capability.DocumentedPermissionSatisfied, capability.Required, capability})
			if capability.Required && capability.DocumentedPermissionSatisfied == "fail" {
				fail(4)
			} else if capability.Required && capability.DocumentedPermissionSatisfied != "pass" {
				fail(5)
			}
		}
	}
	o.Checks = append(o.Checks, check{"verification.live_ui", "pending", false, "subscription, card callbacks, group creation, message writes and App UI require separately authorized real verification; scope grants and transport readiness alone do not prove them"})
	data["phase1_released"] = phase1Released
	if !phase1Released {
		o.Checks = append(o.Checks, check{"release.acceptance", "pending", false, "development artifact; formal apply/install/start remain gated"})
	}
	o.OK, o.Data = code == 0, data
	switch {
	case code == 0 && liveErr == nil:
		o.State = "diagnostics-passed-ui-unverified"
	case code == 0:
		o.State = "prerequisites-ready-bridge-not-observed"
	case code == 5:
		o.State = "diagnostics-pending"
	default:
		o.State = "prerequisite-failed"
	}
	o.NextSteps = append(o.NextSteps, "doctor performs no test messages or tasks; complete real verification separately before claiming configuration complete")
	return o, code
}
