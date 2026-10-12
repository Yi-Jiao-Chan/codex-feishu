package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func assertHydrationScope(t *testing.T, s *Store, host, app, owner, bot string, want []Binding, private int) {
	t.Helper()
	var before, after int64
	if err := s.DB.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	got, count, err := s.BindingHydrationScope(context.Background(), host, app, owner, bot)
	if err != nil || count != private || !reflect.DeepEqual(got, want) {
		t.Fatalf("hydration scope: %+v private=%d err=%v; want %+v private=%d", got, count, err, want, private)
	}
	if err := s.DB.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil || after != before {
		t.Fatalf("classification wrote: %d -> %d, %v", before, after, err)
	}
}

func assertHydrationConflict(t *testing.T, s *Store, host, app, owner, bot string) {
	t.Helper()
	var before, after int64
	if err := s.DB.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	ordinary, count, err := s.BindingHydrationScope(context.Background(), host, app, owner, bot)
	if !errors.Is(err, ErrConflict) || len(ordinary) != 0 || count != 0 {
		t.Fatalf("mismatched marker yielded usable scope: %+v private=%d err=%v", ordinary, count, err)
	}
	if err := s.DB.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil || before != after {
		t.Fatal("conflict check wrote", before, after, err)
	}
}

func TestBindingHydrationScopeRetainsHistoricalFixtureAndOrdinaryRoutes(t *testing.T) {
	s, n, g, bot, now := verificationBindingFixture(t, true)
	ctx := context.Background()
	b, err := s.RegisterVerificationBinding(ctx, n, g, bot, now)
	if err != nil {
		t.Fatal(err)
	}
	if b.ExpiresAtMS >= time.Now().UnixMilli() {
		t.Fatal("fixture must already be expired")
	}
	// The fixture's historical build/connection are never compared with a
	// current process; this query classifies retained proof, not permission.
	assertHydrationScope(t, s, n.HostID, n.AppID, n.OwnerID, bot, nil, 1)
	// A later positive roster observation does not change the original route.
	if _, err := s.DB.Exec(`UPDATE verification_groups SET membership_verified_at_ms=?`, now+100); err != nil {
		t.Fatal(err)
	}
	assertHydrationScope(t, s, n.HostID, n.AppID, n.OwnerID, bot, nil, 1)
	if err := s.ObserveNew(ctx, n.HostID, "thread-ordinary", "/project", 500); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEligible(ctx, n.HostID, "thread-ordinary", "ordinary-user-item", "/project", "/project"); err != nil {
		t.Fatal(err)
	}
	i, err := s.PrepareGroup(ctx, n.HostID, "thread-ordinary", n.OwnerID, "ordinary group", []string{n.OwnerID})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimGroupDispatch(ctx, i.ID); err != nil || !ok {
		t.Fatal("claim ordinary group", ok, err)
	}
	if err := s.ConfirmGroup(ctx, i.ID, "chat-ordinary"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	ordinary := Binding{HostID: n.HostID, ThreadID: "thread-ordinary", IntentID: i.ID, ChatID: "chat-ordinary"}
	assertHydrationScope(t, s, n.HostID, n.AppID, n.OwnerID, bot, []Binding{ordinary}, 1)
	for _, scope := range [][4]string{{n.HostID, "other-app", n.OwnerID, bot}, {n.HostID, n.AppID, "other-owner", bot}, {n.HostID, n.AppID, n.OwnerID, "other-bot"}} {
		assertHydrationConflict(t, s, scope[0], scope[1], scope[2], scope[3])
	}
	assertHydrationScope(t, s, "other-host", n.AppID, n.OwnerID, bot, nil, 0)
}

func TestBindingHydrationScopeIncompleteOrChangedProofCannotHideReadiness(t *testing.T) {
	for _, c := range []struct{ name, query string }{
		{"marker-missing", `DELETE FROM verification_bindings`},
		{"marker-host", `UPDATE verification_bindings SET host_id='other-host'`},
		{"marker-app", `UPDATE verification_bindings SET app_id='other-app'`},
		{"marker-owner", `UPDATE verification_bindings SET owner_id='other-owner'`},
		{"marker-bot", `UPDATE verification_bindings SET bot_id='other-bot'`},
		{"marker-thread", `UPDATE verification_bindings SET thread_id='other-thread'`},
		{"marker-chat", `UPDATE verification_bindings SET chat_id='other-chat'`},
		{"marker-only-chat-associated", `UPDATE verification_bindings SET thread_id='other-thread',intent_id='other-intent'`},
		{"marker-only-intent-associated", `UPDATE verification_bindings SET thread_id='other-thread',chat_id='other-chat'`},
		{"marker-only-thread-associated", `UPDATE verification_bindings SET chat_id='other-chat',intent_id='other-intent'`},
		{"marker-build", `UPDATE verification_bindings SET bridge_commit='other-build'`},
		{"marker-cwd", `UPDATE verification_bindings SET cwd='/other'`},
		{"marker-expiry", `UPDATE verification_bindings SET expires_at_ms=expires_at_ms+1`},
		{"route-chat", `UPDATE bindings SET chat_id='other-chat'`},
		{"route-intent", `UPDATE bindings SET intent_id='other-intent'`},
		{"intent-missing", `DELETE FROM create_intents`},
		{"intent-owner", `UPDATE create_intents SET owner_id='other-owner'`},
		{"intent-host", `UPDATE create_intents SET host_id='other-host'`},
		{"intent-thread", `UPDATE create_intents SET thread_id='other-thread'`},
		{"intent-chat", `UPDATE create_intents SET chat_id='other-chat'`},
		{"intent-state", `UPDATE create_intents SET state='unknown'`},
		{"intent-name", `UPDATE create_intents SET name='other-name'`},
		{"intent-derived-id", `UPDATE create_intents SET intent_id='other-intent'; UPDATE bindings SET intent_id='other-intent'; UPDATE verification_bindings SET intent_id='other-intent'`},
		{"native-missing", `DELETE FROM verification_native`},
		{"native-app", `UPDATE verification_native SET app_id='other-app'`},
		{"native-owner", `UPDATE verification_native SET owner_id='other-owner'`},
		{"native-hash", `UPDATE verification_native SET binary_sha256=replace(binary_sha256,'b','a')`},
		{"native-thread", `UPDATE verification_native SET thread_id='other-thread'`},
		{"native-state", `UPDATE verification_native SET state='unknown'`},
		{"group-missing", `DELETE FROM verification_groups`},
		{"group-app", `UPDATE verification_groups SET app_id='other-app'`},
		{"group-owner", `UPDATE verification_groups SET owner_id='other-owner'`},
		{"group-build", `UPDATE verification_groups SET bridge_commit='other-build'`},
		{"group-chat", `UPDATE verification_groups SET chat_id='other-chat'`},
		{"group-state", `UPDATE verification_groups SET state='unknown'`},
		{"group-extra-members", `UPDATE verification_groups SET members_json='["owner","extra"]'; UPDATE create_intents SET members_json='["owner","extra"]'`},
		{"group-invalid-members", `UPDATE verification_groups SET members_json='broken'; UPDATE create_intents SET members_json='broken'`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, n, g, bot, now := verificationBindingFixture(t, true)
			ctx := context.Background()
			if _, err := s.RegisterVerificationBinding(ctx, n, g, bot, now); err != nil {
				t.Fatal(err)
			}
			// Corrupt proof deliberately, without exercising a real mutation API.
			if _, err := s.DB.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.Exec(c.query); err != nil {
				t.Fatal(err)
			}
			all, err := s.Bindings(ctx, n.HostID)
			if err != nil || len(all) != 1 {
				t.Fatal("actual route lost", all, err)
			}
			if c.name == "marker-missing" {
				assertHydrationScope(t, s, n.HostID, n.AppID, n.OwnerID, bot, all, 0)
			} else {
				assertHydrationConflict(t, s, n.HostID, n.AppID, n.OwnerID, bot)
			}
		})
	}
}

func TestBindingHydrationScopeReadFailuresDoNotHideRoutes(t *testing.T) {
	s, n, g, bot, now := verificationBindingFixture(t, true)
	ctx := context.Background()
	if _, err := s.RegisterVerificationBinding(ctx, n, g, bot, now); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.BindingHydrationScope(canceled, n.HostID, n.AppID, n.OwnerID, bot); err == nil {
		t.Fatal("canceled classification succeeded")
	}
	if _, err := s.DB.Exec(`ALTER TABLE verification_groups RENAME TO unreadable_groups`); err != nil {
		t.Fatal(err)
	}
	if routes, count, err := s.BindingHydrationScope(ctx, n.HostID, n.AppID, n.OwnerID, bot); err == nil || len(routes) != 0 || count != 0 {
		t.Fatal("read failure yielded usable partial scope", routes, count, err)
	}
}
