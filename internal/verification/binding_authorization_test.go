package verification

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func authorizedBindingFixture(t *testing.T) (*BindingManager, *bindingCreator, *groupGateway, string) {
	t.Helper()
	m, n, g, id := bindingFixture(t)
	if _, err := m.Begin(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return m, n, g, id
}

func TestVerificationOperationOrdinaryThreadMakesNoFixtureCalls(t *testing.T) {
	m, n, g, _ := authorizedBindingFixture(t)
	joins, reads := n.joins.Load(), g.reads.Load()
	parent := context.Background()
	ctx, release, handled, err := m.BeginOperation(parent, "ordinary-thread")
	if handled || err != nil || ctx != parent || release == nil {
		t.Fatal(handled, err)
	}
	release()
	if n.joins.Load() != joins || g.reads.Load() != reads {
		t.Fatal("ordinary route used fixture API")
	}
}

func TestVerificationOperationLeaseRechecksWithoutRenewal(t *testing.T) {
	m, n, g, id := authorizedBindingFixture(t)
	before, err := m.State(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	joins, reads := n.joins.Load(), g.reads.Load()
	for range 3 {
		ctx, release, handled, err := m.BeginOperation(context.Background(), "owned-thread")
		if err != nil || !handled || release == nil || ctx == nil || ctx.Err() != nil {
			t.Fatal("valid lease denied", handled, err)
		}
		release()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("lease outlived release")
		}
	}
	after, err := m.State(context.Background(), id)
	if err != nil || after.BoundAtMS != before.BoundAtMS || after.ExpiresAtMS != before.ExpiresAtMS || n.joins.Load() != joins+3 || g.reads.Load() <= reads || n.calls.Load() != 1 || g.calls.Load() != 1 {
		t.Fatal("lease renewed/recreated or skipped current checks", err)
	}
}

func TestVerificationOperationInvalidMarkerNeverFallsBack(t *testing.T) {
	for _, reason := range []string{"foreign-app", "foreign-owner", "old-build", "expired", "future-roster-proof", "missing-group-ping", "changed-connection", "resource-stopped", "extra-member", "bot-absent", "route-deleted", "intent-changed", "policy-unproven", "model-stops-in-join", "route-deleted-in-join", "intent-changed-in-join", "frozen-request-changed"} {
		t.Run(reason, func(t *testing.T) {
			m, n, g, id := authorizedBindingFixture(t)
			db := m.Sessions.native.Store.DB
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := db.Exec(q, args...); err != nil {
					t.Fatal(err)
				}
			}
			switch reason {
			case "foreign-app":
				exec(`UPDATE verification_bindings SET app_id='other'`)
			case "foreign-owner":
				exec(`UPDATE verification_bindings SET owner_id='other'`)
			case "old-build":
				m.Sessions.native.Commit = "changed"
				m.Groups.Commit = "changed"
			case "expired":
				m.Sessions.native.Now = func() time.Time { return time.UnixMilli(2_000_000) }
			case "future-roster-proof":
				exec(`UPDATE verification_groups SET membership_verified_at_ms=2000000`)
			case "missing-group-ping":
				exec(`UPDATE verification_groups SET group_message_id=''`)
			case "changed-connection":
				n.connection = store.ID()
			case "resource-stopped":
				if err := m.Sessions.active[id].model.Close(); err != nil {
					t.Fatal(err)
				}
			case "extra-member":
				g.members = []string{"owner", "uninvited"}
			case "bot-absent":
				g.bot = false
			case "route-deleted":
				exec(`DELETE FROM bindings`)
			case "intent-changed":
				exec(`UPDATE create_intents SET name='other'`)
			case "policy-unproven":
				n.join = func(context.Context, string, codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
					return codexclient.Thread{}, errors.New("PRIVATE fixture error")
				}
			case "model-stops-in-join", "route-deleted-in-join", "intent-changed-in-join":
				n.join = func(_ context.Context, _ string, s codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
					switch reason {
					case "model-stops-in-join":
						if err := m.Sessions.active[id].model.Close(); err != nil {
							t.Error(err)
						}
					case "route-deleted-in-join":
						exec(`DELETE FROM bindings`)
					case "intent-changed-in-join":
						exec(`UPDATE create_intents SET state='unknown'`)
					}
					return nativeThread(s), nil
				}
			case "frozen-request-changed":
				exec(`UPDATE verification_native SET request_json='{}'`)
			}
			ctx, release, handled, err := m.BeginOperation(context.Background(), "owned-thread")
			if release != nil {
				release()
			}
			if !handled || err == nil || ctx != nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("invalid fixture fell back/returned writable scope", handled, err)
			}
			if n.calls.Load() != 1 || g.calls.Load() != 1 {
				t.Fatal("guard replaced components")
			}
		})
	}
}

func TestVerificationOperationCancelsOnConnectionModelAndWindow(t *testing.T) {
	for _, reason := range []string{"connection", "model", "window", "parent", "close"} {
		t.Run(reason, func(t *testing.T) {
			m, n, _, id := authorizedBindingFixture(t)
			if reason == "window" {
				for _, table := range []string{"verification_native", "verification_groups", "verification_bindings"} {
					if _, err := m.Sessions.native.Store.DB.Exec(`UPDATE ` + table + ` SET expires_at_ms=1000250`); err != nil {
						t.Fatal(err)
					}
				}
			}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, release, handled, err := m.BeginOperation(parent, "owned-thread")
			if err != nil || !handled {
				t.Fatal(handled, err)
			}
			var closed chan error
			switch reason {
			case "connection":
				close(n.done)
			case "model":
				if err := m.Sessions.active[id].model.Close(); err != nil {
					t.Fatal(err)
				}
			case "parent":
				cancel()
			case "close":
				closed = make(chan error, 1)
				go func() { closed <- m.Sessions.Close() }()
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Error("invalidated operation did not cancel", reason)
			}
			release()
			if closed != nil {
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("close did not join released operation")
				}
			}
		})
	}
}

func TestVerificationRuntimeAuthorizationPreservesClassification(t *testing.T) {
	m, n, g, id := authorizedBindingFixture(t)
	s := m.Sessions.active[id].spec
	joins, reads := n.joins.Load(), g.reads.Load()
	test := nativeThread(s)
	if handled, err := m.Authorize(context.Background(), test, true); !handled || !errors.Is(err, store.ErrIneligible) || n.joins.Load() != joins || g.reads.Load() != reads {
		t.Fatal("automatic validation was granted or used API", handled, err)
	}
	if handled, err := m.Authorize(context.Background(), test, false); !handled || err != nil {
		t.Fatal("explicit runtime proof denied", handled, err)
	}
	test.ThreadSource = nil
	if handled, err := m.Authorize(context.Background(), test, false); !handled || err == nil {
		t.Fatal("marker with ordinary source fell back", handled, err)
	}
}

func TestVerificationOperationReadDelayCannotExtendWindow(t *testing.T) {
	m, _, _, _ := authorizedBindingFixture(t)
	ctx := context.Background()
	db := m.Sessions.native.Store.DB
	base := time.Now().UnixMilli()
	expires := base + 2000
	for _, table := range []string{"verification_native", "verification_groups"} {
		if _, err := db.Exec(`UPDATE `+table+` SET created_at_ms=?,expires_at_ms=?`, base, expires); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE verification_groups SET membership_verified_at_ms=?`, base); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE verification_bindings SET bound_at_ms=?,membership_verified_at_ms=?,expires_at_ms=?`, base, base, expires); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	var once sync.Once
	m.Sessions.native.Now = func() time.Time {
		sampled := time.Now()
		once.Do(func() {
			connection, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Hold the sole connection after time sampling but before native /
			// group / intent reads. The returned scope must use the old anchor.
			time.AfterFunc(80*time.Millisecond, func() { _ = connection.Close() })
		})
		return sampled
	}
	opctx, release, handled, err := m.BeginOperation(ctx, "owned-thread")
	if err != nil || !handled {
		t.Fatal(handled, err)
	}
	defer release()
	deadline, ok := opctx.Deadline()
	if !ok || deadline.After(time.UnixMilli(expires).Add(5*time.Millisecond)) {
		t.Fatal("read latency extended original expiry", deadline)
	}
}
