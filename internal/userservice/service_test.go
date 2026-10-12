package userservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"github.com/pelletier/go-toml/v2"
)

type fakeRunner struct {
	t                                       *testing.T
	dir                                     string
	active, linger, fragment, dropIns       string
	enabled, unavailable, lingerUnavailable bool
	needsReload                             bool
	missingExit, reloadFailures             int
	failVerb                                string
	calls                                   [][]string
	beforeShow                              func()
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (CommandResult, error) {
	f.t.Helper()
	f.calls = append(f.calls, append([]string{name}, args...))
	if name == "loginctl" {
		if len(args) < 2 || args[0] != "show-user" || args[1] != strconv.Itoa(os.Getuid()) {
			f.t.Fatalf("unexpected loginctl arguments: %v", args)
		}
		if f.lingerUnavailable {
			return CommandResult{}, errors.New("SECRET_TOKEN=private")
		}
		return CommandResult{Stdout: f.linger + "\n"}, nil
	}
	if name != "systemctl" || len(args) < 2 || args[0] != "--user" {
		f.t.Fatalf("unexpected command: %s %v", name, args)
	}
	verb := args[1]
	if verb == "show" && args[2] == "--property=Version" {
		if f.beforeShow != nil {
			fn := f.beforeShow
			f.beforeShow = nil
			fn()
		}
		if f.unavailable {
			return CommandResult{}, errors.New("SECRET_TOKEN=private")
		}
		return CommandResult{Stdout: "252\n"}, nil
	}
	if verb != "daemon-reload" && (len(args) < 3 || args[2] != UnitName) {
		f.t.Fatalf("mutation not restricted to fixed unit: %v", args)
	}
	if verb == "show" {
		load, fragment, fileState, exit := "not-found", "", "", f.missingExit
		if _, err := os.Stat(filepath.Join(f.dir, UnitName)); err == nil {
			load, fragment, fileState, exit = "loaded", filepath.Join(f.dir, UnitName), "disabled", 0
		}
		if f.fragment != "" {
			load, fragment = "loaded", f.fragment
		}
		if f.enabled {
			fileState = "enabled"
		}
		reload := "no"
		if f.needsReload {
			reload = "yes"
		}
		return CommandResult{Stdout: "LoadState=" + load + "\nActiveState=" + f.active + "\nSubState=dead\nUnitFileState=" + fileState + "\nFragmentPath=" + fragment + "\nDropInPaths=" + f.dropIns + "\nNeedDaemonReload=" + reload + "\n", ExitCode: exit}, nil
	}
	if verb == f.failVerb {
		return CommandResult{ExitCode: 1, Stdout: "secret-private"}, nil
	}
	switch verb {
	case "daemon-reload":
		if f.reloadFailures > 0 {
			f.reloadFailures--
			return CommandResult{ExitCode: 1}, nil
		}
		f.needsReload = false
	case "enable":
		f.enabled = true
	case "disable":
		f.enabled = false
	case "start":
		f.active = "active"
	case "stop":
		f.active = "inactive"
	default:
		f.t.Fatalf("unexpected verb: %s", verb)
	}
	return CommandResult{}, nil
}

func fixture(t *testing.T) (*Manager, *fakeRunner, Request, config.Config) {
	t.Helper()
	root := t.TempDir()
	var c config.Config
	c.SchemaVersion = 1
	c.Host.ID = "persistent-host"
	c.Codex.CLIPath = "auto"
	c.Feishu.AppID = "cli_test_application"
	c.Feishu.AppSecretFile = filepath.Join(root, "secret")
	c.Access.OwnerOpenID = "ou_authorized_owner"
	c.Access.OperatorOpenIDs = []string{c.Access.OwnerOpenID}
	c.Discovery.FirstRun = "baseline"
	c.Discovery.AllowedRoots = []string{root}
	c.Notifications.Timezone = "Asia/Shanghai"
	c.Storage.DatabaseFile = filepath.Join(root, "bridge.db")
	data, err := toml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{BinaryPath: filepath.Join(root, `bridge % $ " \ executable`), ConfigPath: filepath.Join(root, `config % $ " \.toml`)}
	if err := os.WriteFile(req.ConfigPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(req.BinaryPath, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{t: t, dir: filepath.Join(root, "units"), active: "inactive", linger: "yes"}
	m, err := New(Options{Runner: f, UnitDir: f.dir})
	if err != nil {
		t.Fatal(err)
	}
	return m, f, req, c
}

func installFixture(t *testing.T) (*Manager, *fakeRunner, Request, config.Config) {
	t.Helper()
	m, f, req, c := fixture(t)
	if r := m.Install(context.Background(), req); !r.OK {
		t.Fatalf("fixture install: %+v", r)
	}
	f.calls = nil
	return m, f, req, c
}

func mutations(f *fakeRunner) []string {
	var verbs []string
	for _, call := range f.calls {
		if call[0] == "systemctl" && call[2] != "show" {
			verbs = append(verbs, call[2])
		}
	}
	return verbs
}

func TestInstallPrivateEscapingIdempotenceAndEnableRepair(t *testing.T) {
	m, f, req, _ := fixture(t)
	r := m.Install(context.Background(), req)
	if !r.OK || !r.Changed || !reflect.DeepEqual(mutations(f), []string{"daemon-reload", "enable"}) {
		t.Fatalf("install: %+v, mutations %v", r, mutations(f))
	}
	unit, err := os.ReadFile(r.Service.UnitPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(unit)
	if !strings.Contains(s, "%%") || !strings.Contains(s, "ExecStart=:") || !strings.Contains(s, "$") || strings.Contains(s, "$$") || !strings.Contains(s, `\"`) || !strings.Contains(s, `\\`) || strings.Contains(s, "/bin/sh") {
		t.Fatalf("unsafe rendering: %s", s)
	}
	for _, tt := range []struct {
		path string
		mode os.FileMode
	}{{m.dir, 0700}, {r.Service.UnitPath, 0600}} {
		info, err := os.Stat(tt.path)
		if err != nil || info.Mode().Perm() != tt.mode {
			t.Fatalf("permissions: %v %v", info, err)
		}
	}
	f.calls = nil
	r = m.Install(context.Background(), req)
	if !r.OK || r.Changed || len(mutations(f)) != 0 {
		t.Fatalf("repeat changed: %+v %v", r, mutations(f))
	}
	f.calls, f.enabled = nil, false
	r = m.Install(context.Background(), req)
	if !r.OK || !r.Changed || !reflect.DeepEqual(mutations(f), []string{"enable"}) {
		t.Fatalf("enable repair: %+v %v", r, mutations(f))
	}
	if err := os.Chmod(r.Service.UnitPath, 0644); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	r = m.Install(context.Background(), req)
	if !r.OK || !r.Changed {
		t.Fatalf("mode repair: %+v", r)
	}
	info, _ := os.Stat(r.Service.UnitPath)
	if info.Mode().Perm() != 0600 {
		t.Fatal("unit remained accessible")
	}
	f.calls, f.needsReload = nil, true
	r = m.Install(context.Background(), req)
	if !r.OK || !r.Changed || !reflect.DeepEqual(mutations(f), []string{"daemon-reload"}) {
		t.Fatalf("stale manager not repaired: %+v %v", r, mutations(f))
	}
}

func TestUnconfirmedEnableAndDisableExposeUnknownAutostart(t *testing.T) {
	m, f, req, _ := fixture(t)
	f.failVerb = "enable"
	r := m.Install(context.Background(), req)
	if r.OK || r.Code != 5 || !r.Service.Installed || r.Service.UnitFileState != "unknown" {
		t.Fatalf("enable uncertainty: %+v", r)
	}
	f.failVerb = ""
	if r := m.Install(context.Background(), req); !r.OK {
		t.Fatalf("enable recovery: %+v", r)
	}
	f.failVerb = "disable"
	r = m.Uninstall(context.Background(), req.ConfigPath)
	if r.OK || r.Code != 5 || r.Service.UnitFileState != "unknown" {
		t.Fatalf("disable uncertainty: %+v", r)
	}
	for _, c := range r.Checks {
		if c.ID == "boot_autostart" && c.Status == "pass" {
			t.Fatal("failed disable retained misleading enabled check")
		}
	}
	if _, err := os.Stat(r.Service.UnitPath); err != nil {
		t.Fatal("unconfirmed disable removed unit")
	}
}

func TestUninstallReloadFailureRestoresStoppedDisabledUnit(t *testing.T) {
	m, f, req, _ := installFixture(t)
	f.active, f.reloadFailures = "active", 1
	r := m.Uninstall(context.Background(), req.ConfigPath)
	if r.OK || r.State != "rolled_back" || r.Service.ActiveState != "inactive" || r.Service.UnitFileState != "disabled" {
		t.Fatalf("uninstall rollback: %+v", r)
	}
	if _, err := os.Stat(r.Service.UnitPath); err != nil {
		t.Fatal("unit not restored")
	}
	if f.enabled || f.active != "inactive" {
		t.Fatal("restored unit restarted or reenabled")
	}
}

func TestAbsentUnitWithForeignLoadedServiceIsConflict(t *testing.T) {
	m, f, req, _ := fixture(t)
	f.fragment = "/etc/systemd/user/codex-feishu.service"
	r := m.Install(context.Background(), req)
	if r.OK || r.Code != 6 || len(mutations(f)) != 0 {
		t.Fatalf("loaded foreign unit replaced: %+v %v", r, mutations(f))
	}
}

func TestReadOnlyStatusDistinguishesInactiveFromUnavailableAndLinger(t *testing.T) {
	for _, linger := range []string{"yes", "no", "unknown"} {
		t.Run(linger, func(t *testing.T) {
			m, f, req, _ := fixture(t)
			f.missingExit, f.linger = 4, linger
			f.lingerUnavailable = linger == "unknown"
			r := m.ReadStatus(context.Background(), req.ConfigPath)
			if !r.OK || !r.Service.ManagerAvailable || r.Service.ActiveState != "inactive" || r.State != "not_installed" {
				t.Fatalf("stopped treated as missing manager: %+v", r)
			}
			if (r.Service.LogoutPersistence == "pass") != (linger == "yes") {
				t.Fatalf("linger misreported: %+v", r)
			}
			if len(mutations(f)) != 0 {
				t.Fatal("read mutated system")
			}
			if _, err := os.Stat(m.dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("read created directory")
			}
		})
	}
	m, f, req, _ := fixture(t)
	f.unavailable = true
	r := m.ReadStatus(context.Background(), req.ConfigPath)
	if r.OK || r.Code != 3 || r.Service.ManagerAvailable {
		t.Fatalf("unavailable: %+v", r)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "SECRET_TOKEN") || strings.Contains(string(b), "private") {
		t.Fatalf("raw error leak: %s", b)
	}
}

func TestConflictRefusesManualForeignSymlinkAndDropInUnits(t *testing.T) {
	for _, kind := range []string{"manual", "foreign", "symlink", "dropin", "other-location"} {
		t.Run(kind, func(t *testing.T) {
			m, f, req, _ := installFixture(t)
			p := filepath.Join(m.dir, UnitName)
			switch kind {
			case "manual":
				if err := os.WriteFile(p, []byte("[Service]\nExecStart=/bin/true\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				b, _ := os.ReadFile(p)
				id, _ := identity(req.ConfigPath)
				b = []byte(strings.Replace(string(b), id, strings.Repeat("a", 64), 1))
				if err := os.WriteFile(p, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(req.ConfigPath, p); err != nil {
					t.Fatal(err)
				}
			case "dropin":
				f.dropIns = "/tmp/override.conf"
			case "other-location":
				f.fragment = "/etc/systemd/user/codex-feishu.service"
			}
			for _, r := range []Result{m.Install(context.Background(), req), m.Start(context.Background(), req.ConfigPath), m.Stop(context.Background(), req.ConfigPath), m.Uninstall(context.Background(), req.ConfigPath)} {
				if r.OK || r.Code != 6 {
					t.Fatalf("conflict accepted: %+v", r)
				}
			}
			if len(mutations(f)) != 0 {
				t.Fatalf("conflict mutated: %v", mutations(f))
			}
		})
	}
}

func TestReloadFailureRestoresPriorUnitOrReportsUnknown(t *testing.T) {
	for _, failures := range []int{1, 2} {
		t.Run(strconv.Itoa(failures), func(t *testing.T) {
			m, f, req, _ := installFixture(t)
			p := filepath.Join(m.dir, UnitName)
			before, _ := os.ReadFile(p)
			req.BinaryPath += "-new"
			if err := os.WriteFile(req.BinaryPath, []byte("fixture"), 0700); err != nil {
				t.Fatal(err)
			}
			f.reloadFailures = failures
			r := m.Install(context.Background(), req)
			after, _ := os.ReadFile(p)
			if r.OK || !reflect.DeepEqual(before, after) || r.BackupPath == "" {
				t.Fatalf("failed rollback: %+v", r)
			}
			want := "rolled_back"
			if failures == 2 {
				want = "unknown"
			}
			if r.State != want {
				t.Fatalf("wrong outcome: %+v", r)
			}
			if !reflect.DeepEqual(mutations(f), []string{"daemon-reload", "daemon-reload"}) {
				t.Fatalf("blind retry: %v", mutations(f))
			}
		})
	}
	m, f, req, _ := fixture(t)
	f.reloadFailures = 1
	r := m.Install(context.Background(), req)
	if r.State != "rolled_back" {
		t.Fatalf("new unit rollback: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(m.dir, UnitName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new unit survived rollback")
	}
}

func TestStartStopNoopAndUninstallPreservesData(t *testing.T) {
	m, f, req, c := installFixture(t)
	for _, p := range []string{c.Feishu.AppSecretFile, c.Storage.DatabaseFile} {
		if err := os.WriteFile(p, []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := m.Start(context.Background(), req.ConfigPath)
	if !r.OK || !r.Changed || r.State != "running" {
		t.Fatalf("start: %+v", r)
	}
	f.calls = nil
	r = m.Start(context.Background(), req.ConfigPath)
	if !r.OK || r.Changed || len(mutations(f)) != 0 {
		t.Fatalf("repeat start: %+v %v", r, mutations(f))
	}
	r = m.Stop(context.Background(), req.ConfigPath)
	if !r.OK || !r.Changed || r.State != "stopped" {
		t.Fatalf("stop: %+v", r)
	}
	f.calls = nil
	r = m.Stop(context.Background(), req.ConfigPath)
	if !r.OK || r.Changed || len(mutations(f)) != 0 {
		t.Fatalf("repeat stop: %+v %v", r, mutations(f))
	}
	f.active, f.calls = "active", nil
	r = m.Uninstall(context.Background(), req.ConfigPath)
	if !r.OK || !r.Changed || r.State != "uninstalled" || !reflect.DeepEqual(mutations(f), []string{"stop", "disable", "daemon-reload"}) {
		t.Fatalf("uninstall: %+v %v", r, mutations(f))
	}
	for _, p := range []string{req.ConfigPath, c.Feishu.AppSecretFile, c.Storage.DatabaseFile} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("data removed: %s", p)
		}
	}
	f.calls = nil
	r = m.Uninstall(context.Background(), "/missing/config.toml")
	if !r.OK || r.Changed || len(f.calls) != 0 {
		t.Fatalf("missing-unit uninstall: %+v %v", r, f.calls)
	}
}

func TestUninstallRetainsUnitWhenStopCannotBeConfirmed(t *testing.T) {
	m, f, req, _ := installFixture(t)
	f.active, f.failVerb = "active", "stop"
	r := m.Uninstall(context.Background(), req.ConfigPath)
	if r.OK || r.Code != 5 || !reflect.DeepEqual(mutations(f), []string{"stop"}) {
		t.Fatalf("unsafe uninstall: %+v %v", r, mutations(f))
	}
	if _, err := os.Stat(filepath.Join(m.dir, UnitName)); err != nil {
		t.Fatal("unit removed despite unconfirmed stop")
	}
}

func TestActiveInstallReportsPendingAndDoesNotRestart(t *testing.T) {
	m, f, req, _ := installFixture(t)
	f.active, f.linger = "active", "no"
	req.BinaryPath += "-updated"
	if err := os.WriteFile(req.BinaryPath, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	r := m.Install(context.Background(), req)
	if !r.OK || r.Service.LogoutPersistence != "pending" || !reflect.DeepEqual(mutations(f), []string{"daemon-reload"}) {
		t.Fatalf("active install: %+v %v", r, mutations(f))
	}
	var pending bool
	for _, c := range r.Checks {
		if c.ID == "running_configuration" && c.Status == "pending" {
			pending = true
		}
	}
	if !pending {
		t.Fatal("updated active process reported fully applied")
	}
}

func TestInvalidPathsAndServiceOperationLock(t *testing.T) {
	m, f, req, _ := fixture(t)
	for _, bad := range []string{"relative", "/tmp/evil\nExecStart=/bin/false"} {
		req.BinaryPath = bad
		r := m.Install(context.Background(), req)
		if r.OK || r.Code != 2 || len(f.calls) != 0 {
			t.Fatalf("unsafe path: %+v", r)
		}
	}
	m, f, req, _ = installFixture(t)
	lock, err := m.lock(false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(lock)
	r := m.Stop(context.Background(), req.ConfigPath)
	if r.OK || r.Code != 6 || len(f.calls) != 0 {
		t.Fatalf("concurrent operation not blocked: %+v", r)
	}
}

func candidateFixture(t *testing.T) (*Manager, *fakeRunner, Request, config.Config) {
	t.Helper()
	_, f, req, c := fixture(t)
	if err := os.Chmod(filepath.Dir(c.Storage.DatabaseFile), 0700); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{Runner: f, UnitDir: f.dir, CandidateTest: true, CandidateBinaryPath: req.BinaryPath})
	if err != nil {
		t.Fatal(err)
	}
	return m, f, req, c
}

func TestCandidateLifecycleBindsExecutableAndNormalCleanup(t *testing.T) {
	m, f, req, c := candidateFixture(t)
	ctx := context.Background()
	r := m.Install(ctx, req)
	if !r.OK || !r.CandidateTest || !r.Changed || !reflect.DeepEqual(mutations(f), []string{"daemon-reload", "enable"}) {
		t.Fatalf("candidate install: %+v %v", r, mutations(f))
	}
	b, err := os.ReadFile(r.Service.UnitPath)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte("fixture"))
	if !strings.Contains(string(b), "# Candidate binary SHA256: "+hex.EncodeToString(h[:])+"\n") || !strings.Contains(string(b), " serve --candidate-test --config ") {
		t.Fatalf("candidate identity absent: %s", b)
	}
	f.calls = nil
	if r := m.Install(ctx, req); !r.OK || r.Changed || len(mutations(f)) != 0 {
		t.Fatalf("repeat install: %+v %v", r, mutations(f))
	}
	if err := os.Chmod(filepath.Dir(c.Storage.DatabaseFile), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(c.Storage.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if r := m.Start(ctx, req.ConfigPath); !r.OK || !r.CandidateTest || !r.Changed || r.State != "running" {
		t.Fatalf("candidate start: %+v", r)
	}
	f.calls = nil
	if r := m.Start(ctx, req.ConfigPath); !r.OK || r.Changed || len(mutations(f)) != 0 {
		t.Fatalf("repeat start: %+v %v", r, mutations(f))
	}
	if r := m.Install(ctx, req); !r.OK || r.Changed || len(mutations(f)) != 0 {
		t.Fatalf("active exact install: %+v %v", r, mutations(f))
	}
	normal, err := New(Options{Runner: f, UnitDir: f.dir})
	if err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if r := normal.ReadStatus(ctx, req.ConfigPath); !r.OK || !r.CandidateTest || len(mutations(f)) != 0 {
		t.Fatalf("candidate status lost deployment label: %+v %v", r, mutations(f))
	}
	if r := normal.Stop(ctx, req.ConfigPath); !r.OK || r.State != "stopped" {
		t.Fatalf("normal cleanup stop: %+v", r)
	}
	if r := normal.Uninstall(ctx, req.ConfigPath); !r.OK || r.State != "uninstalled" {
		t.Fatalf("normal cleanup uninstall: %+v", r)
	}
	if _, err := os.Stat(req.ConfigPath); err != nil {
		t.Fatal("cleanup removed configuration")
	}
	if _, err := os.Stat(c.Storage.DatabaseFile); err != nil {
		t.Fatal("cleanup removed database")
	}
	id, err := identity(req.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	wantNormal := marker + "# Deployment identity: " + id + "\n[Unit]\nDescription=Codex Feishu bridge\n\n[Service]\nType=simple\nExecStart=:" + quoteArg(req.BinaryPath) + " serve --config " + quoteArg(req.ConfigPath) + "\nRestart=on-failure\nRestartSec=5s\nUMask=0077\n\n[Install]\nWantedBy=default.target\n"
	if string(render(req.BinaryPath, req.ConfigPath, id)) != wantNormal {
		t.Fatal("normal unit rendering changed")
	}
}

func TestCandidateStartRejectsChangedExecutablePathAndNormalUnit(t *testing.T) {
	for _, change := range []string{"bytes", "path", "normal-unit", "missing-selection"} {
		t.Run(change, func(t *testing.T) {
			m, f, req, _ := candidateFixture(t)
			ctx := context.Background()
			if r := m.Install(ctx, req); !r.OK {
				t.Fatal(r)
			}
			unitPath := filepath.Join(f.dir, UnitName)
			switch change {
			case "bytes":
				if err := os.WriteFile(req.BinaryPath, []byte("changed candidate"), 0700); err != nil {
					t.Fatal(err)
				}
			case "path":
				m.candidateBinaryPath = req.BinaryPath + "-other"
				if err := os.WriteFile(m.candidateBinaryPath, []byte("fixture"), 0700); err != nil {
					t.Fatal(err)
				}
			case "normal-unit":
				id, _ := identity(req.ConfigPath)
				if err := os.WriteFile(unitPath, render(req.BinaryPath, req.ConfigPath, id), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-selection":
				m.candidateBinaryPath = ""
			}
			before, _ := os.ReadFile(unitPath)
			f.calls = nil
			r := m.Start(ctx, req.ConfigPath)
			after, _ := os.ReadFile(unitPath)
			if r.OK || len(mutations(f)) != 0 || !bytes.Equal(before, after) {
				t.Fatalf("changed candidate started: %+v %v", r, mutations(f))
			}
		})
	}
}

func TestCandidateStartRejectsNonPrivateUnitWithoutMutation(t *testing.T) {
	m, f, req, _ := candidateFixture(t)
	ctx := context.Background()
	if r := m.Install(ctx, req); !r.OK {
		t.Fatal(r)
	}
	unitPath := filepath.Join(f.dir, UnitName)
	before, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unitPath, 0644); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	r := m.Start(ctx, req.ConfigPath)
	after, readErr := os.ReadFile(unitPath)
	info, statErr := os.Stat(unitPath)
	if r.OK || r.Code != 6 || len(mutations(f)) != 0 || readErr != nil || statErr != nil || info.Mode().Perm() != 0644 || !bytes.Equal(before, after) {
		t.Fatalf("non-private candidate started or mutated: %+v %v, read=%v stat=%v", r, mutations(f), readErr, statErr)
	}
}

func TestCandidateInstallCannotMutateActiveOrTransitioningUnit(t *testing.T) {
	for _, active := range []string{"active", "activating", "deactivating"} {
		for _, change := range []string{"bytes", "reload", "disabled", "permissions"} {
			t.Run(active+"/"+change, func(t *testing.T) {
				m, f, req, _ := candidateFixture(t)
				ctx := context.Background()
				if r := m.Install(ctx, req); !r.OK {
					t.Fatal(r)
				}
				unitPath := filepath.Join(f.dir, UnitName)
				before, _ := os.ReadFile(unitPath)
				f.active = active
				switch change {
				case "bytes":
					if err := os.WriteFile(req.BinaryPath, []byte("changed candidate"), 0700); err != nil {
						t.Fatal(err)
					}
				case "reload":
					f.needsReload = true
				case "disabled":
					f.enabled = false
				case "permissions":
					if err := os.Chmod(unitPath, 0644); err != nil {
						t.Fatal(err)
					}
				}
				f.calls = nil
				r := m.Install(ctx, req)
				after, _ := os.ReadFile(unitPath)
				if r.OK || r.Code != 6 || len(mutations(f)) != 0 || !bytes.Equal(before, after) || r.BackupPath != "" {
					t.Fatalf("active installation mutated: %+v %v", r, mutations(f))
				}
			})
		}
	}
}

func TestCandidateRejectsUnselectedAndForeignUnits(t *testing.T) {
	for _, conflict := range []string{"selected-binary", "manual", "foreign"} {
		t.Run(conflict, func(t *testing.T) {
			m, f, req, _ := candidateFixture(t)
			ctx := context.Background()
			if r := m.Install(ctx, req); !r.OK {
				t.Fatal(r)
			}
			p := filepath.Join(f.dir, UnitName)
			before, _ := os.ReadFile(p)
			switch conflict {
			case "selected-binary":
				req.BinaryPath += "-other"
				if err := os.WriteFile(req.BinaryPath, []byte("fixture"), 0700); err != nil {
					t.Fatal(err)
				}
			case "manual":
				before = []byte("[Service]\nExecStart=/bin/true\n")
			case "foreign":
				id, _ := identity(req.ConfigPath)
				before = bytes.Replace(before, []byte(id), []byte(strings.Repeat("a", 64)), 1)
			}
			if err := os.WriteFile(p, before, 0600); err != nil {
				t.Fatal(err)
			}
			f.calls = nil
			if r := m.Install(ctx, req); r.OK || r.Code != 6 || len(mutations(f)) != 0 {
				t.Fatalf("conflicting install: %+v %v", r, mutations(f))
			}
			if conflict != "selected-binary" {
				if r := m.Start(ctx, req.ConfigPath); r.OK || r.Code != 6 || len(mutations(f)) != 0 {
					t.Fatalf("conflicting start: %+v %v", r, mutations(f))
				}
			}
			after, _ := os.ReadFile(p)
			if !bytes.Equal(before, after) {
				t.Fatal("conflicting unit replaced")
			}
		})
	}
}

func TestCandidateRejectsLegacyDatabaseWithoutInstallingStartingOrMigrating(t *testing.T) {
	for _, action := range []string{"install", "start"} {
		t.Run(action, func(t *testing.T) {
			m, f, req, c := candidateFixture(t)
			ctx := context.Background()
			if action == "start" {
				if r := m.Install(ctx, req); !r.OK {
					t.Fatal(r)
				}
			}
			db, err := sql.Open("sqlite", c.Storage.DatabaseFile)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`PRAGMA user_version=1`); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(c.Storage.DatabaseFile, 0600); err != nil {
				t.Fatal(err)
			}
			f.calls = nil
			var r Result
			if action == "install" {
				r = m.Install(ctx, req)
			} else {
				r = m.Start(ctx, req.ConfigPath)
			}
			if r.OK || len(mutations(f)) != 0 {
				t.Fatalf("legacy mutation: %+v %v", r, mutations(f))
			}
			var version int
			if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
				t.Fatalf("legacy schema changed: %d %v", version, err)
			}
			if action == "install" {
				if _, err := os.Stat(f.dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("legacy preflight created installer directory")
				}
			}
		})
	}
}

func TestCandidateRepeatsDatabasePreflightUnderServiceLock(t *testing.T) {
	for _, action := range []string{"install", "start"} {
		t.Run(action, func(t *testing.T) {
			m, f, req, c := candidateFixture(t)
			ctx := context.Background()
			if action == "start" {
				if r := m.Install(ctx, req); !r.OK {
					t.Fatal(r)
				}
			}
			before, _ := os.ReadFile(filepath.Join(f.dir, UnitName))
			// Manager observation runs after initial preflight and service locking.
			f.beforeShow = func() {
				db, err := sql.Open("sqlite", c.Storage.DatabaseFile)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`PRAGMA user_version=1`); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(c.Storage.DatabaseFile, 0600); err != nil {
					t.Fatal(err)
				}
			}
			f.calls = nil
			var r Result
			if action == "install" {
				r = m.Install(ctx, req)
			} else {
				r = m.Start(ctx, req.ConfigPath)
			}
			after, _ := os.ReadFile(filepath.Join(f.dir, UnitName))
			if r.OK || len(mutations(f)) != 0 || !bytes.Equal(before, after) {
				t.Fatalf("changed database bypassed locked check: %+v %v", r, mutations(f))
			}
		})
	}
}

func TestCandidateRenderingPreservesExecutableContainingArgumentText(t *testing.T) {
	_, _, req, _ := candidateFixture(t)
	binary := filepath.Join(filepath.Dir(req.BinaryPath), "binary serve --config executable")
	if err := os.WriteFile(binary, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	unit, err := candidateUnit(context.Background(), binary, req.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(unit, []byte("ExecStart=:"+quoteArg(binary)+" serve --candidate-test --config "+quoteArg(req.ConfigPath))) {
		t.Fatal("candidate rendering altered the executable path")
	}
}
