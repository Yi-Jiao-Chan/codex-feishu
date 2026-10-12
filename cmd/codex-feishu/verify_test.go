package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
	"github.com/pelletier/go-toml/v2"
)

type verificationLock struct{ closes int }

func (l *verificationLock) Close() error { l.closes++; return nil }

type verificationGateway struct {
	cards, groups int
	err           error
	db            *store.Store
}

func TestLegacyVerifyRejectsChangedDiagnosticSnapshotAfterLocks(t *testing.T) {
	for _, when := range []string{"diagnostic", "application-lock", "database-lock", "secret"} {
		t.Run(when, func(t *testing.T) {
			path, cfg, d, g := doctorFixture(t)
			createLegacyVerificationDB(t, cfg.Storage.DatabaseFile)
			d.health = func(context.Context, string, config.Config) (control.Snapshot, error) {
				return control.Snapshot{}, control.ErrUnavailable
			}
			d.snapshot = defaultDoctorDependencies().snapshot
			change := func() {
				if when == "secret" {
					if err := os.WriteFile(cfg.Feishu.AppSecretFile, []byte("changed-private-secret"), 0600); err != nil {
						t.Fatal(err)
					}
					return
				}
				cfg.Access.OwnerOpenID = "ou_changed_owner_valid"
				b, err := toml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if when == "diagnostic" {
				discover := d.discover
				d.discover = func(ctx context.Context, cli string) (codexclient.Daemon, error) { change(); return discover(ctx, cli) }
			}
			deps := readonlyVerifyDependencies(t, d)
			appLock, dbLock := &verificationLock{}, &verificationLock{}
			deps.appLock = func(string) (io.Closer, error) {
				if when == "application-lock" {
					change()
				}
				return appLock, nil
			}
			deps.dbLock = func(string) (io.Closer, error) {
				if when == "database-lock" || when == "secret" {
					change()
				}
				return dbLock, nil
			}
			o, code := inspectVerify(context.Background(), path, verifyOptions{Card: true, Group: true}, deps)
			if code != 6 || o.State != "verification-config-conflict" || o.OK || appLock.closes != 1 || dbLock.closes != 1 || g.calls != 2 {
				t.Fatal(o, code)
			}
			b, _ := json.Marshal(o)
			if strings.Contains(string(b), "changed-private-secret") || strings.Contains(string(b), "ou_changed_owner_valid") {
				t.Fatal("conflict exposed changed private data")
			}
		})
	}
}

func (g *verificationGateway) SendCard(_ context.Context, kind, target, card, uuid string) (string, error) {
	if kind != "open_id" || target != "ou_fixture_owner_valid" || uuid == "" || card == "" {
		panic("unexpected card target")
	}
	if g.db != nil {
		status, saved, err := g.db.Operation(context.Background(), "m0-test-card-uuid")
		if err != nil || status != "confirmed" || saved != uuid {
			panic("card UUID was not committed before send")
		}
	}
	g.cards++
	return "om_owner_probe", g.err
}
func (g *verificationGateway) CreateGroup(_ context.Context, name, owner string, members []string, uuid string) (string, error) {
	if owner != "ou_fixture_owner_valid" || len(members) != 1 || members[0] != owner || uuid == "" || name == "" {
		panic("unexpected group members")
	}
	if g.db != nil {
		status, saved, err := g.db.Operation(context.Background(), "m0-test-group-uuid")
		if err != nil || status != "confirmed" || saved != uuid {
			panic("group UUID was not committed before send")
		}
	}
	g.groups++
	return "oc_owner_probe", g.err
}
func readonlyVerifyDependencies(t *testing.T, d doctorDependencies) verifyDependencies {
	return verifyDependencies{doctor: d,
		appLock: func(string) (io.Closer, error) { t.Fatal("unexpected application lock"); return nil, nil },
		dbLock:  func(string) (io.Closer, error) { t.Fatal("unexpected database lock"); return nil, nil },
		open: func(context.Context, string, int) (*store.Store, error) {
			t.Fatal("unexpected writable open")
			return nil, nil
		},
		gateway: func(config.Config, string) verifyGateway { t.Fatal("unexpected mutation gateway"); return nil },
		emit:    func(output) {},
	}
}
func createLegacyVerificationDB(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE m0_operations(key TEXT PRIMARY KEY,kind TEXT NOT NULL,state TEXT NOT NULL,remote_id TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL);
CREATE TABLE m0_bindings(thread_id TEXT PRIMARY KEY,chat_id TEXT UNIQUE NOT NULL);
CREATE TABLE m0_receipts(id TEXT PRIMARY KEY,app_id TEXT NOT NULL,state TEXT NOT NULL);
CREATE TABLE m0_reactions(receipt_id TEXT PRIMARY KEY,state TEXT NOT NULL);
PRAGMA user_version=1;`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDefaultUsesManagedFactsWithoutMutationsAndCannotPassUI(t *testing.T) {
	path, cfg, d, g := doctorFixture(t)
	deps := readonlyVerifyDependencies(t, d)
	o, code := inspectVerify(context.Background(), path, verifyOptions{}, deps)
	if code != 5 || o.OK || o.State != "verification-pending" || g.calls != 2 {
		t.Fatal(o, code)
	}
	pending := 0
	for _, c := range o.Checks {
		if strings.HasPrefix(c.ID, "acceptance.P1-") {
			if c.Status != "pending" || !c.Required {
				t.Fatal(c)
			}
			pending++
		}
	}
	if pending != 12 {
		t.Fatal("matrix omitted", pending)
	}
	if _, err := os.Stat(cfg.Storage.DatabaseFile); !os.IsNotExist(err) {
		t.Fatal("database created", err)
	}
	b, _ := json.Marshal(o)
	if strings.Contains(string(b), "doctor-secret-never-output") {
		t.Fatal("credential leaked")
	}
}

func TestVerifyLegacyDefaultPreservesBytesModeAndSchemaWhileLocked(t *testing.T) {
	path, cfg, d, _ := doctorFixture(t)
	createLegacyVerificationDB(t, cfg.Storage.DatabaseFile)
	if err := os.Chmod(cfg.Storage.DatabaseFile, 0640); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.Storage.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.AcquireListener(cfg.Storage.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	d.health = func(context.Context, string, config.Config) (control.Snapshot, error) {
		return control.Snapshot{}, control.ErrUnavailable
	}
	d.snapshot = defaultDoctorDependencies().snapshot
	o, code := inspectVerify(context.Background(), path, verifyOptions{}, readonlyVerifyDependencies(t, d))
	if code != 5 || o.OK {
		t.Fatal(o, code)
	}
	after, _ := os.ReadFile(cfg.Storage.DatabaseFile)
	if string(before) != string(after) {
		t.Fatal("default verification changed database bytes")
	}
	info, _ := os.Stat(cfg.Storage.DatabaseFile)
	if info.Mode().Perm() != 0640 {
		t.Fatal("default verification chmodded database")
	}
	db, version, err := store.OpenReadOnly(context.Background(), cfg.Storage.DatabaseFile)
	if err != nil || version != 1 {
		t.Fatal("legacy migrated", version, err)
	}
	db.Close()
}

func TestVerifyInvalidArgumentsPerformNoDependencies(t *testing.T) {
	for _, args := range [][]string{{}, {"--config", "/unused", "--target", "all"}, {"--config", "/unused", "--unknown"}, {"--config", "/unused", "trailing"}} {
		var got output
		deps := verifyDependencies{emit: func(o output) { got = o }}
		if code := runVerifyWithDependencies(context.Background(), args, deps); code != 2 || got.State != "invalid-arguments" {
			t.Fatal(got, code)
		}
	}
}

func TestVerifyUnsupportedThreadAndManagedTestsNeverPartiallyMutate(t *testing.T) {
	path, _, d, _ := doctorFixture(t)
	for _, opts := range []verifyOptions{{Card: true, Group: true}, {Card: true, Group: true, Thread: true}, {Thread: true}} {
		o, code := inspectVerify(context.Background(), path, opts, readonlyVerifyDependencies(t, d))
		if code != 5 || o.OK {
			t.Fatal(o, code)
		}
	}
}

func TestLegacyVerificationLocksBeforeWritableOpenAndRetainsPending(t *testing.T) {
	ctx := context.Background()
	for _, conflict := range []string{"app", "db", "schema", "none"} {
		t.Run(conflict, func(t *testing.T) {
			path, cfg, d, _ := doctorFixture(t)
			createLegacyVerificationDB(t, cfg.Storage.DatabaseFile)
			d.health = func(context.Context, string, config.Config) (control.Snapshot, error) {
				return control.Snapshot{}, control.ErrUnavailable
			}
			d.snapshot = defaultDoctorDependencies().snapshot
			appLock, dbLock := &verificationLock{}, &verificationLock{}
			g := &verificationGateway{}
			deps := readonlyVerifyDependencies(t, d)
			deps.appLock = func(string) (io.Closer, error) {
				if conflict == "app" {
					return nil, errors.New("private error")
				}
				return appLock, nil
			}
			deps.dbLock = func(string) (io.Closer, error) {
				if conflict == "db" {
					return nil, errors.New("private error")
				}
				return dbLock, nil
			}
			opens := 0
			deps.open = func(ctx context.Context, path string, schema int) (*store.Store, error) {
				opens++
				if schema != 1 {
					t.Fatal("unexpected schema")
				}
				if conflict == "schema" {
					return nil, errors.New("private error")
				}
				db, err := store.OpenExisting(ctx, path, schema)
				g.db = db
				return db, err
			}
			deps.gateway = func(config.Config, string) verifyGateway { return g }
			o, code := inspectVerify(ctx, path, verifyOptions{Card: true, Group: true}, deps)
			want := 5
			if conflict == "app" || conflict == "db" {
				want = 6
			}
			if conflict == "schema" {
				want = 3
			}
			if code != want || o.OK {
				t.Fatal(o, code)
			}
			if conflict == "app" || conflict == "db" {
				if opens != 0 || g.cards+g.groups != 0 {
					t.Fatal("mutation before lock")
				}
			}
			if conflict != "app" && appLock.closes != 1 {
				t.Fatal("app lock leaked")
			}
			if conflict == "none" || conflict == "schema" {
				if dbLock.closes != 1 {
					t.Fatal("db lock leaked")
				}
			}
			if conflict == "none" {
				if g.cards != 1 || g.groups != 1 {
					t.Fatal("probes not prepared")
				}
				_, code = inspectVerify(ctx, path, verifyOptions{Card: true, Group: true}, deps)
				if code != 5 || g.cards != 1 || g.groups != 1 {
					t.Fatal("replay duplicated external writes")
				}
				db, v, err := store.OpenReadOnly(ctx, cfg.Storage.DatabaseFile)
				if err != nil || v != 1 {
					t.Fatal("probe migrated", v, err)
				}
				db.Close()
			}
		})
	}
}

func TestLegacyUnknownMutationDoesNotRetryAndDefiniteFailureCannotPass(t *testing.T) {
	for _, mutationErr := range []error{&feishu.Unknown{Operation: "send"}, &feishu.Rejected{Code: 999, HTTPStatus: 403}} {
		t.Run(mutationErr.Error(), func(t *testing.T) {
			path, cfg, d, _ := doctorFixture(t)
			createLegacyVerificationDB(t, cfg.Storage.DatabaseFile)
			d.health = func(context.Context, string, config.Config) (control.Snapshot, error) {
				return control.Snapshot{}, control.ErrUnavailable
			}
			d.snapshot = defaultDoctorDependencies().snapshot
			g := &verificationGateway{err: mutationErr}
			deps := readonlyVerifyDependencies(t, d)
			deps.appLock = func(string) (io.Closer, error) { return &verificationLock{}, nil }
			deps.dbLock = deps.appLock
			deps.open = func(ctx context.Context, path string, schema int) (*store.Store, error) {
				db, err := store.OpenExisting(ctx, path, schema)
				g.db = db
				return db, err
			}
			deps.gateway = func(config.Config, string) verifyGateway { return g }
			o, code := inspectVerify(context.Background(), path, verifyOptions{Card: true}, deps)
			want := 5
			var rejected *feishu.Rejected
			if errors.As(mutationErr, &rejected) {
				want = 4
			}
			if o.OK || code != want {
				t.Fatal("incorrect mutation outcome", o, code)
			}
			_, code = inspectVerify(context.Background(), path, verifyOptions{Card: true}, deps)
			if code == 0 || g.cards != 1 {
				t.Fatal("uncertain/rejected probe was resent", code, g.cards)
			}
		})
	}
}

func TestManagedCardRequiresExplicitIDAndRetainsFullAcceptancePending(t *testing.T) {
	path, cfg, d, _ := doctorFixture(t)
	originalHealth := d.health
	d.health = func(ctx context.Context, path string, c config.Config) (control.Snapshot, error) {
		s, err := originalHealth(ctx, path, c)
		s.VerificationCardSupported = true
		return s, err
	}
	deps := readonlyVerifyDependencies(t, d)
	calls := 0
	var starts []bool
	requestID := store.ID()
	deps.managed = func(_ context.Context, c config.Config, identity, id string, begin bool) (verification.Result, error) {
		calls++
		starts = append(starts, begin)
		loaded, loadErr := config.Load(path)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		expected, err := control.Identity(path, loaded)
		if err != nil || c.Host.ID != cfg.Host.ID || identity != expected || id != requestID {
			t.Fatal("wrong managed probe scope")
		}
		return verification.Result{RequestID: id, BridgeCommit: "running-build", BinarySHA256: strings.Repeat("b", 64), State: "confirmed", CardStatus: "pass", CallbackStatus: "pass", PrivateMessageStatus: "pass", MessageID: "om_probe", ExpiresAtMS: 100}, nil
	}
	o, code := inspectVerify(context.Background(), path, verifyOptions{Card: true}, deps)
	if code != 2 || o.State != "verification-request-id-required" || calls != 0 {
		t.Fatal(o, code, calls)
	}
	for _, begin := range []bool{true, false} {
		o, code = inspectVerify(context.Background(), path, verifyOptions{Card: begin, RequestID: requestID}, deps)
		if code != 5 || o.OK || o.State != "verification-managed-card-observed" {
			t.Fatal(o, code)
		}
		data := o.Data.(map[string]any)
		if data["read_only"] != !begin || data["request_id"] != requestID || data["verification_scope"] != "managed-owner-card-components" {
			t.Fatal(data)
		}
		count := 0
		for _, check := range o.Checks {
			if strings.HasPrefix(check.ID, "acceptance.P1-") {
				count++
				if check.Status != "pending" || !check.Required {
					t.Fatal("component evidence promoted P1", check)
				}
			}
		}
		if count != 12 {
			t.Fatal("incomplete P1 matrix")
		}
	}
	if calls != 2 || len(starts) != 2 || !starts[0] || starts[1] {
		t.Fatal(calls, starts)
	}
	for _, opts := range []verifyOptions{{Card: true, Group: true, RequestID: requestID}, {Card: true, Thread: true, RequestID: requestID}} {
		inspectVerify(context.Background(), path, opts, deps)
	}
	if calls != 2 {
		t.Fatal("unsupported combined probe partially sent")
	}
	if _, err := os.Stat(cfg.Storage.DatabaseFile); !os.IsNotExist(err) {
		t.Fatal("CLI opened/created separate journal", err)
	}
}

func TestManagedProbeUnknownQueriesAndUnsupportedBuildsStayPending(t *testing.T) {
	path, _, d, _ := doctorFixture(t)
	original := d.health
	d.health = func(ctx context.Context, p string, c config.Config) (control.Snapshot, error) {
		s, err := original(ctx, p, c)
		s.VerificationCardSupported = true
		return s, err
	}
	deps := readonlyVerifyDependencies(t, d)
	id := store.ID()
	calls := 0
	deps.managed = func(_ context.Context, _ config.Config, _ string, got string, begin bool) (verification.Result, error) {
		calls++
		if got != id || begin {
			t.Fatal("state query tried begin")
		}
		return verification.Result{}, errors.New("PRIVATE WIRE BODY")
	}
	o, code := inspectVerify(context.Background(), path, verifyOptions{RequestID: id}, deps)
	if code != 5 || o.OK || calls != 1 || o.State != "verification-managed-card-pending" {
		t.Fatal(o, code, calls)
	}
	b, _ := json.Marshal(o)
	if strings.Contains(string(b), "PRIVATE") {
		t.Fatal("raw managed error leaked")
	}
	d.health = original
	deps.doctor = d
	o, code = inspectVerify(context.Background(), path, verifyOptions{Card: true, RequestID: id}, deps)
	if code != 5 || calls != 1 || o.State != "verification-managed-tests-pending" {
		t.Fatal("unsupported build received mutation", o, code, calls)
	}
	deps = verifyDependencies{emit: func(o output) {
		if o.State != "invalid-arguments" {
			t.Fatal(o)
		}
	}}
	if runVerifyWithDependencies(context.Background(), []string{"--config", path, "--request-id", "invalid"}, deps) != 2 {
		t.Fatal("bad probe ID reached dependencies")
	}
}

func TestManagedReadOnlyProbeRemainsAvailableDuringNativeDegradation(t *testing.T) {
	path, _, d, _ := doctorFixture(t)
	original := d.health
	d.health = func(ctx context.Context, p string, c config.Config) (control.Snapshot, error) {
		s, err := original(ctx, p, c)
		s.VerificationCardSupported = true
		s.NativeConnected = false
		return s, err
	}
	deps := readonlyVerifyDependencies(t, d)
	id := store.ID()
	reads := 0
	deps.managed = func(_ context.Context, _ config.Config, _ string, got string, begin bool) (verification.Result, error) {
		if begin || got != id {
			t.Fatal("degraded probe mutation")
		}
		reads++
		return verification.Result{RequestID: id, BridgeCommit: "prior-build", BinarySHA256: strings.Repeat("b", 64), BuildChanged: true, State: "confirmed", CardStatus: "pass", CallbackStatus: "pass", PrivateMessageStatus: "pass", MessageID: "om_card", ExpiresAtMS: 100}, nil
	}
	o, code := inspectVerify(context.Background(), path, verifyOptions{RequestID: id}, deps)
	if code != 5 || o.OK || reads != 1 || o.State != "verification-managed-card-other-build" || o.Data.(map[string]any)["read_only"] != true {
		t.Fatal(o, code, reads)
	}
	o, code = inspectVerify(context.Background(), path, verifyOptions{RequestID: id, Card: true}, deps)
	if code != 5 || reads != 1 {
		t.Fatal("degraded native allowed new probe", o, code, reads)
	}
}
