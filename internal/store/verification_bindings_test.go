package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func verificationBindingFixture(t *testing.T, completeBaseline bool) (*Store, VerificationNative, VerificationGroup, string, int64) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "verification-binding.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	host, app, owner, bot := "host", "app", "owner", "bot"
	if _, err := s.BeginBaseline(ctx, host, app, 1); err != nil {
		t.Fatal(err)
	}
	if completeBaseline {
		if err := s.CompleteBaseline(ctx, host, nil); err != nil {
			t.Fatal(err)
		}
	}
	id := ID()
	connection := strings.Repeat("a", 32)
	n := VerificationNative{RequestID: id, HostID: host, AppID: app, OwnerID: owner, BridgeCommit: "build", BinarySHA256: strings.Repeat("b", 64), ConnectionID: connection, CWD: "/project", ModelURL: "http://127.0.0.1:1234/v1", RequestJSON: `{"model":"fixture-model"}`, CreatedAtMS: 100_000, ExpiresAtMS: 1_000_000}
	if _, err := s.PrepareVerificationNative(ctx, n); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimVerificationNative(ctx, n, 7, connection, 200_000); err != nil || !ok {
		t.Fatal("native claim", ok, err)
	}
	n.Generation, n.State = 7, "dispatching"
	n.ConnectionID = connection
	if err := s.FinishVerificationNative(ctx, n, "confirmed", "thread-native"); err != nil {
		t.Fatal(err)
	}
	n, err = s.VerificationNative(ctx, id, host, app, owner)
	if err != nil {
		t.Fatal(err)
	}
	g := VerificationGroup{RequestID: id, HostID: host, AppID: app, OwnerID: owner, BridgeCommit: n.BridgeCommit, BinarySHA256: n.BinarySHA256, UUID: "group-uuid", Name: "verification group", MembersJSON: `[` + `"owner"` + `]`, CreatedAtMS: 100_000, ExpiresAtMS: 1_000_000}
	if _, err := s.PrepareVerificationGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimVerificationGroup(ctx, g, 200_000); err != nil || !ok {
		t.Fatal("group claim", ok, err)
	}
	g.State = "dispatching"
	if err := s.FinishVerificationGroup(ctx, g, "confirmed", "chat-verified"); err != nil {
		t.Fatal(err)
	}
	g, err = s.VerificationGroup(ctx, id, host, app, owner)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ObserveVerificationGroupMembership(ctx, g, 220_000); err != nil || !ok {
		t.Fatal("membership observation", ok, err)
	}
	if ok, err := s.ObserveVerificationGroupMessage(ctx, host, app, owner, id, "message-verified", g.ChatID, 220_000); err != nil || !ok {
		t.Fatal("group message observation", ok, err)
	}
	g, err = s.VerificationGroup(ctx, id, host, app, owner)
	if err != nil {
		t.Fatal(err)
	}
	return s, n, g, bot, 300_000
}

func verificationBindingCounts(t *testing.T, s *Store) [4]int {
	t.Helper()
	var counts [4]int
	for i, table := range []string{"thread_observations", "create_intents", "bindings", "verification_bindings"} {
		if err := s.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&counts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func TestVerificationBindingMigrationFromTwentyPreservesJournals(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "verification-binding-migration.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	card := VerificationCard{RequestID: ID(), HostID: "host", AppID: "app", OwnerID: "owner", BridgeCommit: "build", BinarySHA256: strings.Repeat("a", 64), Token: ID(), UUID: ID(), Card: "frozen card", CreatedAtMS: 100, ExpiresAtMS: 1000}
	group := VerificationGroup{RequestID: ID(), HostID: "host", AppID: "app", OwnerID: "owner", BridgeCommit: "build", BinarySHA256: strings.Repeat("a", 64), UUID: ID(), Name: "frozen group", MembersJSON: `["owner"]`, CreatedAtMS: 100, ExpiresAtMS: 1000}
	native := VerificationNative{RequestID: ID(), HostID: "host", AppID: "app", OwnerID: "owner", BridgeCommit: "build", BinarySHA256: strings.Repeat("a", 64), CWD: "/project", ModelURL: "http://127.0.0.1:1234/v1", RequestJSON: `{}`, CreatedAtMS: 100, ExpiresAtMS: 1000}
	if _, err := s.PrepareVerificationCard(ctx, card); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareVerificationGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareVerificationNative(ctx, native); err != nil {
		t.Fatal(err)
	}
	if fresh, err := s.Begin(ctx, "m0-retained", "send"); err != nil || !fresh {
		t.Fatal("m0 journal", fresh, err)
	}
	if err := s.Finish(ctx, "m0-retained", "confirmed", "om_m0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DROP TABLE verification_bindings; ALTER TABLE mirrors DROP COLUMN progress_observed_at_json; ALTER TABLE create_intents DROP COLUMN description; PRAGMA user_version=20`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.VerificationCard(ctx, card.RequestID, card.HostID, card.AppID, card.OwnerID); err != nil || got.Card != card.Card {
		t.Fatal("card evidence lost", got, err)
	}
	if got, err := s.VerificationGroup(ctx, group.RequestID, group.HostID, group.AppID, group.OwnerID); err != nil || got.Name != group.Name {
		t.Fatal("group evidence lost", got, err)
	}
	if got, err := s.VerificationNative(ctx, native.RequestID, native.HostID, native.AppID, native.OwnerID); err != nil || got.CWD != native.CWD {
		t.Fatal("native evidence lost", got, err)
	}
	if state, remote, err := s.Operation(ctx, "m0-retained"); err != nil || state != "confirmed" || remote != "om_m0" {
		t.Fatal("M0 evidence lost", state, remote, err)
	}
	var version int
	if err := s.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal("schema version", version, err)
	}
	var tables int
	if err := s.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='verification_bindings'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatal("binding table missing", tables, err)
	}
}

func TestVerificationBindingRegistrationIsAtomicIdempotentAndScoped(t *testing.T) {
	ctx := context.Background()
	s, n, g, bot, now := verificationBindingFixture(t, true)
	b, err := s.RegisterVerificationBinding(ctx, n, g, bot, now)
	if err != nil || b.RequestID != n.RequestID || b.ThreadID != n.ThreadID || b.ChatID != g.ChatID || b.IntentID == "" || b.BoundAtMS != now || b.MembershipVerifiedAtMS != now || b.ExpiresAtMS != g.ExpiresAtMS {
		t.Fatal("registration", b, err)
	}
	if encoded, err := json.Marshal(b); err != nil || string(encoded) != "{}" {
		t.Fatal("binding escaped JSON", string(encoded), err)
	}
	counts := verificationBindingCounts(t, s)
	if counts != [4]int{1, 1, 1, 1} {
		t.Fatal("unexpected local registration rows", counts)
	}
	var observationCreatedAt int64
	if err := s.DB.QueryRow(`SELECT created_at FROM thread_observations WHERE host_id=? AND thread_id=?`, n.HostID, n.ThreadID).Scan(&observationCreatedAt); err != nil {
		t.Fatal("observation timestamp", err)
	}
	if observationCreatedAt != n.CreatedAtMS/1000 {
		t.Fatalf("observation timestamp unit: got %d, want %d", observationCreatedAt, n.CreatedAtMS/1000)
	}
	threadBinding, err := s.VerificationBindingForThread(ctx, n.HostID, n.ThreadID)
	if err != nil || threadBinding.IntentID != b.IntentID {
		t.Fatal("thread marker read", threadBinding, err)
	}
	var firstBound, firstExpiry int64
	if err := s.DB.QueryRow(`SELECT bound_at_ms,expires_at_ms FROM verification_bindings WHERE request_id=?`, n.RequestID).Scan(&firstBound, &firstExpiry); err != nil {
		t.Fatal(err)
	}
	const attempts = 16
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.RegisterVerificationBinding(ctx, n, g, bot, now+1)
			if err == nil && (got.BoundAtMS != firstBound || got.ExpiresAtMS != firstExpiry) {
				err = errors.New("replay changed binding time")
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("concurrent replay", err)
		}
	}
	if got := verificationBindingCounts(t, s); got != counts {
		t.Fatal("replay changed row count", got)
	}
	var attemptsCount int
	if err := s.DB.QueryRow(`SELECT count(*) FROM group_attempts`).Scan(&attemptsCount); err != nil || attemptsCount != 0 {
		t.Fatal("binding created group attempts", attemptsCount, err)
	}
	if _, err := s.VerificationBinding(ctx, n.RequestID, "other-host", n.AppID, n.OwnerID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign scoped binding read", err)
	}
}

func TestVerificationBindingReplayRequiresDerivedRows(t *testing.T) {
	ctx := context.Background()
	s, n, g, bot, now := verificationBindingFixture(t, true)
	if _, err := s.RegisterVerificationBinding(ctx, n, g, bot, now); err != nil {
		t.Fatal("initial registration", err)
	}
	if _, err := s.DB.Exec(`DELETE FROM bindings WHERE host_id=? AND thread_id=?`, n.HostID, n.ThreadID); err != nil {
		t.Fatal("delete route", err)
	}
	if _, err := s.RegisterVerificationBinding(ctx, n, g, bot, now+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay repaired missing route: %v", err)
	}
	if _, err := s.VerificationBinding(ctx, n.RequestID, n.HostID, n.AppID, n.OwnerID); err != nil {
		t.Fatal("verification marker changed", err)
	}
	if _, err := s.Binding(ctx, n.HostID, n.ThreadID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing route was repaired: %v", err)
	}
}

func TestVerificationBindingInvalidInputsWriteNothing(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*Store, *VerificationNative, *VerificationGroup, *int64)
	}{
		{name: "native state", prepare: func(_ *Store, n *VerificationNative, _ *VerificationGroup, _ *int64) { n.State = "unknown" }},
		{name: "group state", prepare: func(_ *Store, _ *VerificationNative, g *VerificationGroup, _ *int64) { g.State = "unknown" }},
		{name: "empty group message", prepare: func(_ *Store, _ *VerificationNative, g *VerificationGroup, _ *int64) { g.GroupMessageID = "" }},
		{name: "wrong members", prepare: func(_ *Store, _ *VerificationNative, g *VerificationGroup, _ *int64) {
			g.MembersJSON = `["owner","other"]`
		}},
		{name: "foreign scope", prepare: func(_ *Store, n *VerificationNative, g *VerificationGroup, _ *int64) {
			n.OwnerID = "foreign"
			g.OwnerID = "foreign"
		}},
		{name: "changed build", prepare: func(_ *Store, n *VerificationNative, g *VerificationGroup, _ *int64) {
			n.BridgeCommit = "other"
			g.BridgeCommit = "other"
		}},
		{name: "changed nonce", prepare: func(_ *Store, n *VerificationNative, _ *VerificationGroup, _ *int64) {
			n.ConnectionID = strings.Repeat("c", 32)
		}},
		{name: "before window", prepare: func(_ *Store, _ *VerificationNative, _ *VerificationGroup, now *int64) { *now = 99_999 }},
		{name: "after window", prepare: func(_ *Store, _ *VerificationNative, _ *VerificationGroup, now *int64) { *now = 1_000_000 }},
		{name: "bot owner", prepare: func(_ *Store, n *VerificationNative, _ *VerificationGroup, _ *int64) { n.OwnerID = "bot" }},
		{name: "invalid nonce", prepare: func(_ *Store, n *VerificationNative, _ *VerificationGroup, _ *int64) { n.ConnectionID = "bad" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, n, g, bot, now := verificationBindingFixture(t, true)
			test.prepare(s, &n, &g, &now)
			before := verificationBindingCounts(t, s)
			if _, err := s.RegisterVerificationBinding(context.Background(), n, g, bot, now); err == nil {
				t.Fatal("invalid binding accepted")
			}
			if after := verificationBindingCounts(t, s); after != before {
				t.Fatal("invalid binding wrote rows", before, after)
			}
		})
	}

	t.Run("baseline incomplete", func(t *testing.T) {
		s, n, g, bot, now := verificationBindingFixture(t, false)
		before := verificationBindingCounts(t, s)
		if _, err := s.RegisterVerificationBinding(context.Background(), n, g, bot, now); err == nil {
			t.Fatal("incomplete baseline accepted")
		}
		if after := verificationBindingCounts(t, s); after != before {
			t.Fatal("incomplete baseline wrote rows", before, after)
		}
	})

	t.Run("changed database snapshot", func(t *testing.T) {
		s, n, g, bot, now := verificationBindingFixture(t, true)
		if _, err := s.DB.Exec(`UPDATE verification_native SET request_json='{"changed":true}' WHERE request_id=?`, n.RequestID); err != nil {
			t.Fatal(err)
		}
		before := verificationBindingCounts(t, s)
		if _, err := s.RegisterVerificationBinding(context.Background(), n, g, bot, now); err == nil {
			t.Fatal("changed database snapshot accepted")
		}
		if after := verificationBindingCounts(t, s); after != before {
			t.Fatal("changed snapshot wrote rows", before, after)
		}
	})
}

func TestVerificationBindingOrdinaryRouteConflictRollsBack(t *testing.T) {
	ctx := context.Background()
	s, n, g, bot, now := verificationBindingFixture(t, true)
	if err := s.ObserveNew(ctx, n.HostID, n.ThreadID, n.CWD, n.CreatedAtMS/1000); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEligible(ctx, n.HostID, n.ThreadID, "first-input", n.CWD, n.CWD); err != nil {
		t.Fatal(err)
	}
	ordinary, err := s.PrepareGroup(ctx, n.HostID, n.ThreadID, n.OwnerID, "ordinary route", []string{n.OwnerID})
	if err != nil {
		t.Fatal(err)
	}
	attempt, claimed, err := s.ClaimGroupAttempt(ctx, ordinary.ID, now)
	if err != nil || !claimed {
		t.Fatal("ordinary claim", claimed, err)
	}
	if err := s.FinishGroupDispatch(ctx, attempt, "confirmed", "ordinary-chat"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindConfirmedGroup(ctx, ordinary.ID); err != nil {
		t.Fatal(err)
	}
	beforeObservation, err := s.Observation(ctx, n.HostID, n.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterVerificationBinding(ctx, n, g, bot, now); !errors.Is(err, ErrConflict) {
		t.Fatal("ordinary route was taken over", err)
	}
	afterObservation, err := s.Observation(ctx, n.HostID, n.ThreadID)
	if err != nil || afterObservation.State != beforeObservation.State || afterObservation.ExclusionReason != beforeObservation.ExclusionReason {
		t.Fatal("ordinary observation changed after conflict", beforeObservation, afterObservation, err)
	}
	if got, err := s.Binding(ctx, n.HostID, n.ThreadID); err != nil || got.IntentID != ordinary.ID || got.ChatID != "ordinary-chat" {
		t.Fatal("ordinary binding changed", got, err)
	}
	if got := verificationBindingCounts(t, s); got[3] != 0 {
		t.Fatal("verification binding persisted after conflict", got)
	}
}
