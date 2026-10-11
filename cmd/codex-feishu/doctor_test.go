package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/compat"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/store"
	"github.com/pelletier/go-toml/v2"
)

type doctorFixtureGateway struct {
	botErr, scopeErr error
	scopes           []string
	calls            int
}

func (g *doctorFixtureGateway) Bot(context.Context) (string, error) {
	g.calls++
	return "ou_fixture_bot_valid", g.botErr
}
func (g *doctorFixtureGateway) GrantedTenantScopes(context.Context) ([]string, error) {
	g.calls++
	return g.scopes, g.scopeErr
}

func doctorFixture(t *testing.T) (string, config.Config, doctorDependencies, *doctorFixtureGateway) {
	t.Helper()
	dir := t.TempDir()
	var cfg config.Config
	cfg.SchemaVersion, cfg.Host.ID = 1, "doctor-fixture"
	cfg.Codex.CLIPath, cfg.Codex.Endpoint = "/fixture/bin/codex", "auto"
	cfg.Feishu.AppID, cfg.Feishu.AppSecretFile = "cli_doctor_fixture", filepath.Join(dir, "secret")
	cfg.Access.OwnerOpenID = "ou_fixture_owner_valid"
	cfg.Access.OperatorOpenIDs = []string{cfg.Access.OwnerOpenID}
	cfg.Discovery.Enabled, cfg.Discovery.FirstRun, cfg.Discovery.AllowedRoots = true, "baseline", []string{dir}
	cfg.Notifications.Timezone = "Asia/Shanghai"
	cfg.Storage.DatabaseFile = filepath.Join(dir, "bridge.db")
	if err := os.WriteFile(cfg.Feishu.AppSecretFile, []byte("doctor-secret-never-output"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	gateway := &doctorFixtureGateway{scopes: compat.FeishuManifest().RecommendedTenantScopes}
	deps := doctorDependencies{
		discover: func(_ context.Context, cli string) (codexclient.Daemon, error) {
			if cli != cfg.Codex.CLIPath {
				t.Fatalf("ignored configured launcher: %q", cli)
			}
			return codexclient.Daemon{Status: "running", SocketPath: "/fixture/shared.sock", AppServerVersion: "0.162.0"}, nil
		},
		health: func(_ context.Context, actual string, got config.Config) (control.Snapshot, error) {
			if actual != path || got.Host.ID != cfg.Host.ID {
				t.Fatal("incorrect management identity")
			}
			return control.Snapshot{Running: true, NativeConnected: true, FeishuConnected: true, Persistent: store.PersistentHealth{Schema: store.SchemaVersion, BaselineComplete: true}}, nil
		},
		gateway: func(got config.Config, secret string) doctorGateway {
			if got.Feishu.AppID != cfg.Feishu.AppID || secret != "doctor-secret-never-output" {
				t.Fatal("incorrect authentication input")
			}
			return gateway
		},
		snapshot: func(context.Context, config.Config) (store.PersistentHealth, error) {
			t.Fatal("live managed doctor opened independent database snapshot")
			return store.PersistentHealth{}, nil
		},
		nativeProbe: func(context.Context, codexclient.Daemon) (int, error) {
			t.Fatal("configured doctor opened another native client")
			return 0, nil
		},
	}
	return path, cfg, deps, gateway
}

func TestDoctorManagedHealthDoesNotOpenAnotherNativeConnectionOrDatabase(t *testing.T) {
	path, cfg, deps, gateway := doctorFixture(t)
	o, code := inspectDoctor(context.Background(), path, false, true, deps)
	if code != 0 || !o.OK || o.State != "diagnostics-passed-ui-unverified" || gateway.calls != 2 {
		t.Fatal("managed diagnostic failed", o, code)
	}
	b, _ := json.Marshal(o)
	for _, forbidden := range []string{"doctor-secret-never-output", cfg.Access.OwnerOpenID, "ou_fixture_bot_valid"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatal("diagnostic leaked authentication or user identity")
		}
	}
	if _, err := os.Stat(cfg.Storage.DatabaseFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("diagnostic initialized a database", err)
	}
	if !strings.Contains(string(b), `"ui_verified":false`) || !strings.Contains(string(b), `"id":"verification.live_ui","status":"pending","required":false`) {
		t.Fatal("readiness inferred real UI verification", string(b))
	}
}

func TestDoctorBeforeInstallAndRequireRunningDifferExplicitly(t *testing.T) {
	path, _, deps, _ := doctorFixture(t)
	deps.health = func(context.Context, string, config.Config) (control.Snapshot, error) {
		return control.Snapshot{}, control.ErrUnavailable
	}
	deps.snapshot = func(context.Context, config.Config) (store.PersistentHealth, error) {
		return store.PersistentHealth{}, errDoctorNoDatabase
	}
	for _, require := range []bool{false, true} {
		o, code := inspectDoctor(context.Background(), path, false, require, deps)
		if !require && (code != 0 || !o.OK || o.State != "prerequisites-ready-bridge-not-observed") || require && (code != 5 || o.OK || o.State != "diagnostics-pending") {
			t.Fatal("bridge prerequisite and runtime proof conflated", require, o, code)
		}
	}
	deps.snapshot = func(context.Context, config.Config) (store.PersistentHealth, error) {
		return store.PersistentHealth{}, errors.New("unreadable or wrong deployment")
	}
	if o, code := inspectDoctor(context.Background(), path, false, false, deps); code != 3 || o.OK {
		t.Fatal("invalid existing database treated as first install", o, code)
	}
}

func TestDoctorRequiredPermissionAndAmbiguousMetadata(t *testing.T) {
	path, _, deps, gateway := doctorFixture(t)
	for _, test := range []struct {
		name   string
		mutate func()
		code   int
	}{
		{"denied-reaction", func() {
			gateway.scopes = []string{"im:chat", "im:message:send_as_bot", "im:message.p2p_msg:readonly", "im:message.group_at_msg:readonly"}
		}, 4},
		{"ambiguous-grants", func() { gateway.scopeErr = errors.New("private remote response") }, 5},
		{"bad-bot", func() { gateway.botErr = errors.New("doctor-secret-never-output") }, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway.scopes, gateway.botErr, gateway.scopeErr = compat.FeishuManifest().RecommendedTenantScopes, nil, nil
			test.mutate()
			o, code := inspectDoctor(context.Background(), path, false, false, deps)
			b, _ := json.Marshal(o)
			if code != test.code || o.OK || strings.Contains(string(b), "private remote response") || strings.Contains(string(b), "doctor-secret-never-output") {
				t.Fatal("unverified permission or unsafe payload", o, code)
			}
		})
	}
}

func TestDoctorUnreconciledInstanceAndBaselineCannotPass(t *testing.T) {
	path, _, deps, _ := doctorFixture(t)
	for _, reason := range []string{"native", "feishu", "hydration", "baseline", "unknown"} {
		t.Run(reason, func(t *testing.T) {
			deps.health = func(context.Context, string, config.Config) (control.Snapshot, error) {
				s := control.Snapshot{Running: true, NativeConnected: true, FeishuConnected: true, Persistent: store.PersistentHealth{Schema: store.SchemaVersion, BaselineComplete: true}}
				switch reason {
				case "native":
					s.NativeConnected = false
				case "feishu":
					s.FeishuConnected = false
				case "hydration":
					s.Persistent.Bindings = 1
				case "baseline":
					s.Persistent.BaselineComplete = false
				case "unknown":
					s.Persistent.GroupIntents = map[string]int64{"unknown": 1}
				}
				return s, nil
			}
			if o, code := inspectDoctor(context.Background(), path, false, false, deps); code != 5 || o.OK {
				t.Fatal("live but incomplete instance reported ready", o, code)
			}
		})
	}
}

func TestDoctorConfigurationAndEndpointConflictsFailBeforeAuth(t *testing.T) {
	path, cfg, deps, gateway := doctorFixture(t)
	for _, err := range []error{control.ErrIdentity, control.ErrUnsafe} {
		deps.health = func(context.Context, string, config.Config) (control.Snapshot, error) { return control.Snapshot{}, err }
		if o, code := inspectDoctor(context.Background(), path, false, false, deps); code != 6 || o.OK || gateway.calls != 0 {
			t.Fatal("management conflict ignored", o, code)
		}
	}
	if err := os.Chmod(cfg.Feishu.AppSecretFile, 0644); err != nil {
		t.Fatal(err)
	}
	if o, code := inspectDoctor(context.Background(), path, false, false, deps); code != 4 || o.OK || gateway.calls != 0 {
		t.Fatal("unsafe secret reached remote authentication", o, code)
	}
	if err := os.WriteFile(path, []byte("unexpected=\"doctor-secret-never-output\""), 0600); err != nil {
		t.Fatal(err)
	}
	o, code := inspectDoctor(context.Background(), path, false, false, deps)
	b, _ := json.Marshal(o)
	if code != 2 || o.OK || strings.Contains(string(b), "doctor-secret-never-output") {
		t.Fatal("invalid config not private", o, code)
	}
}

func TestDoctorConfiguredVersionBoundaryAndStandaloneCodexProbe(t *testing.T) {
	path, _, deps, _ := doctorFixture(t)
	for _, version := range []string{"0.162.0", "0.163.0"} {
		deps.discover = func(context.Context, string) (codexclient.Daemon, error) {
			return codexclient.Daemon{Status: "running", AppServerVersion: version}, nil
		}
		called := 0
		deps.nativeProbe = func(context.Context, codexclient.Daemon) (int, error) { called++; return 3, nil }
		if o, code := inspectDoctor(context.Background(), path, false, false, deps); (version == "0.162.0" && code != 0) || (version != "0.162.0" && code != 3) || called != 0 {
			t.Fatal("configured version check opened native probe", o, code)
		}
		o, code := inspectDoctor(context.Background(), "", true, false, deps)
		if version == "0.162.0" && (code != 0 || !o.OK || called != 1) || version != "0.162.0" && (code != 3 || o.OK || called != 0) {
			t.Fatal("unverified standalone native probe", o, code, called)
		}
	}
}
