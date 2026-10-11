package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

	"codex-feishu/internal/rules"
)

func retryFixture(t *testing.T) (*Store, string, MirrorAttempt) {
	t.Helper()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	stage(t, s, 100, mirrorDoc("human", "human", "original"))
	return s, path, claimMirror(t, s, "human", 1000)
}

func retryState(t *testing.T, s *Store) (id string, due, count int64) {
	t.Helper()
	if err := s.DB.QueryRow(`SELECT last_attempt_id,next_retry_at_ms,rejection_count FROM outbound_throttles WHERE host_id='host' AND thread_id='thread' AND mirror_key='human'`).Scan(&id, &due, &count); err != nil {
		t.Fatal(err)
	}
	return
}

func TestReadyMirrorsAtExcludesOnlyFutureDeferrals(t *testing.T) {
	ctx := context.Background()
	s, _, first := retryFixture(t)
	stage(t, s, 101, mirrorDoc("second", "human", "independent"))
	if err := s.DeferMirrorAttempt(ctx, first.ID, 1000, 0); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.ReadyMirrorsAt(ctx, "host", "thread", 1999); err != nil || len(ready) != 1 || ready[0].Key != "second" {
		t.Fatalf("selection before deadline: %+v %v", ready, err)
	}
	if ready, err := s.ReadyMirrorsAt(ctx, "host", "thread", 2000); err != nil || len(ready) != 2 || ready[0].Key != "human" || ready[1].Key != "second" {
		t.Fatalf("selection at deadline lost deterministic order: %+v %v", ready, err)
	}
	if ready, err := s.ReadyMirrors(ctx, "host", "thread"); err != nil || len(ready) != 2 {
		t.Fatalf("legacy selection changed: %+v %v", ready, err)
	}
	for _, now := range []int64{0, -1} {
		if _, err := s.ReadyMirrorsAt(ctx, "host", "thread", now); err == nil {
			t.Fatalf("invalid selection time accepted: %d", now)
		}
	}
}

func TestMirrorDeferralRejectsTimeBeforeClaimButReplayIsNoOp(t *testing.T) {
	ctx := context.Background()
	s, _, first := retryFixture(t)
	if err := s.DeferMirrorAttempt(ctx, first.ID, first.CreatedAtMS-1, 0); err == nil {
		t.Fatal("fresh deferral predates dispatch")
	}
	if a, err := s.MirrorAttempt(ctx, first.ID); err != nil || a.State != "dispatching" {
		t.Fatalf("past deferral changed dispatch: %+v %v", a, err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_throttles`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("past deferral created retry proof: %d %v", count, err)
	}
	if err := s.DeferMirrorAttempt(ctx, first.ID, first.CreatedAtMS, 0); err != nil {
		t.Fatal(err)
	}
	second := claimMirror(t, s, "human", 2000)
	if err := s.DeferMirrorAttempt(ctx, second.ID, second.CreatedAtMS-1, 0); err == nil {
		t.Fatal("subsequent deferral predates latest dispatch")
	}
	if err := s.DeferMirrorAttempt(ctx, first.ID, first.CreatedAtMS-1, 60000); err != nil {
		t.Fatalf("durable replay rejected past timestamp: %v", err)
	}
	if id, due, count := retryState(t, s); id != first.ID || due != 2000 || count != 1 {
		t.Fatalf("past fresh/replay time altered retry proof: %s %d %d", id, due, count)
	}
}

func TestRetiredMirrorTailAfterDeferralDoesNotCreateObsoleteCard(t *testing.T) {
	ctx := context.Background()
	for _, outcome := range []string{"deferred", "unknown", "retry_unknown", "dispatching", "confirmed"} {
		t.Run(outcome, func(t *testing.T) {
			s, _ := discoveryStore(t)
			eligible(t, s, "thread")
			bindMirrorThread(t, s)
			tail := Mirror{Key: "tail", TurnID: "turn", Kind: "final", Card: `{"body":"old tail"}`, ParentMessageID: "input", Terminal: true}
			stage(t, s, 100, tail)
			first := claimMirror(t, s, "tail", 1000)
			expectedState, expectedThrottle := outcome, 0
			switch outcome {
			case "deferred", "retry_unknown":
				if err := s.DeferMirrorAttempt(ctx, first.ID, 1000, 0); err != nil {
					t.Fatal(err)
				}
				expectedState = "confirmed"
				if outcome == "retry_unknown" {
					claimMirror(t, s, "tail", 2000)
					if err := s.RecoverMirrorDispatches(ctx, "host"); err != nil {
						t.Fatal(err)
					}
					expectedState, expectedThrottle = "unknown", 1
				}
			case "unknown", "confirmed":
				remote := ""
				if outcome == "confirmed" {
					remote, expectedState = "accepted-tail", "ready"
				}
				if err := s.FinishMirrorAttempt(ctx, first.ID, outcome, remote); err != nil {
					t.Fatal(err)
				}
			}
			tail.Retired, tail.Card = true, `{"body":"obsolete segment"}`
			stage(t, s, 101, tail)
			m, err := s.Mirror(ctx, "host", "thread", "tail")
			if err != nil || !m.Retired || m.State != expectedState {
				t.Fatalf("retirement lost dispatch evidence: %+v %v", m, err)
			}
			var throttles int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_throttles`).Scan(&throttles); err != nil || throttles != expectedThrottle {
				t.Fatalf("retirement changed nonobsolete retry proof: %d %v", throttles, err)
			}
			a, claimed, err := s.ClaimMirror(ctx, "host", "thread", "tail", m.Revision, 90000)
			if outcome == "confirmed" {
				if err != nil || !claimed || a.Operation != "update" || a.TargetMessageID != "accepted-tail" {
					t.Fatalf("accepted tail no longer updated in place: %+v %t %v", a, claimed, err)
				}
			} else if err != nil || claimed {
				t.Fatalf("obsolete or unresolved tail dispatched: %+v %t %v", a, claimed, err)
			}
			frozen, err := s.MirrorAttempt(ctx, first.ID)
			if err != nil || frozen.Card != first.Card || frozen.Revision != first.Revision {
				t.Fatalf("retirement modified immutable old tail: %+v %v", frozen, err)
			}
			if outcome == "deferred" {
				var attempts int
				if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_attempts`).Scan(&attempts); err != nil || attempts != 1 {
					t.Fatalf("obsolete tail created another attempt: %d %v", attempts, err)
				}
				if ready, err := s.ReadyMirrorsAt(ctx, "host", "thread", 90000); err != nil || len(ready) != 0 {
					t.Fatalf("retired tail remained selected: %+v %v", ready, err)
				}
			}
		})
	}
}

func TestRetiredInFlightMirrorTailRateRejectionCompletesLocally(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	tail := Mirror{Key: "tail", TurnID: "turn", Kind: "final", Card: `{"body":"old tail"}`, ParentMessageID: "input", Terminal: true}
	stage(t, s, 100, tail)
	first := claimMirror(t, s, "tail", 1000)
	tail.Retired, tail.Card = true, `{"body":"obsolete segment"}`
	stage(t, s, 1001, tail)
	if m, err := s.Mirror(ctx, "host", "thread", "tail"); err != nil || !m.Retired || m.State != "dispatching" {
		t.Fatalf("inflight retirement discarded unresolved request: %+v %v", m, err)
	}
	if err := s.DeferMirrorAttempt(ctx, first.ID, 1002, 0); err != nil {
		t.Fatal(err)
	}
	// Identical snapshots take StageSnapshot's early-continue path. Deferral
	// itself must have completed retirement rather than relying on later staging.
	stage(t, s, 1003, tail)
	m, err := s.Mirror(ctx, "host", "thread", "tail")
	if err != nil || !m.Retired || m.State != "confirmed" || m.RemoteID != "" {
		t.Fatalf("rejection reopened retired tail: %+v %v", m, err)
	}
	if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", "tail", m.Revision, 90000); err != nil || claimed {
		t.Fatalf("retired inflight tail created another write: %t %v", claimed, err)
	}
	var attempts, throttles int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_attempts`).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("retired inflight tail created another attempt: %d %v", attempts, err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_throttles`).Scan(&throttles); err != nil || throttles != 0 {
		t.Fatalf("retired inflight tail kept obsolete throttle: %d %v", throttles, err)
	}
	if ready, err := s.ReadyMirrorsAt(ctx, "host", "thread", 90000); err != nil || len(ready) != 0 {
		t.Fatalf("retired inflight tail remained selected: %+v %v", ready, err)
	}
	frozen, err := s.MirrorAttempt(ctx, first.ID)
	if err != nil || frozen.State != "rejected" || frozen.RemoteID != "" || frozen.Card != first.Card || frozen.Revision != first.Revision {
		t.Fatalf("rejection changed immutable retired-tail payload: %+v %v", frozen, err)
	}
}

func TestMirrorDeferralPersistsAndCoalescesFreshAttempt(t *testing.T) {
	ctx := context.Background()
	s, path, first := retryFixture(t)
	if err := s.DeferMirrorAttempt(ctx, first.ID, 1000, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferMirrorAttempt(ctx, first.ID, 90000, 120000); err != nil {
		t.Fatal(err)
	}
	if id, due, count := retryState(t, s); id != first.ID || due != 2000 || count != 1 {
		t.Fatalf("replay changed deferral: %s %d %d", id, due, count)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverMirrorDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	latest := mirrorDoc("human", "human", "latest")
	stage(t, s, 101, latest)
	if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", "human", 2, 1999); err != nil || claimed {
		t.Fatalf("early retry: %t %v", claimed, err)
	}
	second := claimMirror(t, s, "human", 2000)
	if second.ID == first.ID || second.Card != latest.Card || second.Revision != 2 || second.Operation != "send" {
		t.Fatalf("stale retry payload: %+v", second)
	}
	if err := s.DeferMirrorAttempt(ctx, first.ID, 50000, 50000); err != nil {
		t.Fatal(err)
	}
	if _, due, count := retryState(t, s); due != 2000 || count != 1 {
		t.Fatal("inflight replay changed deadline/count")
	}
	frozen, err := s.MirrorAttempt(ctx, first.ID)
	if err != nil || frozen.State != "rejected" || frozen.RemoteID != "" || frozen.Card != first.Card || frozen.Revision != 1 {
		t.Fatalf("rejected journal changed: %+v %v", frozen, err)
	}
	if err := s.FinishMirrorAttempt(ctx, second.ID, "confirmed", "message"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_throttles`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("successful retry retained throttle: %d %v", count, err)
	}
	if err := s.DeferMirrorAttempt(ctx, first.ID, 50000, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("old rejection reopened confirmed work: %v", err)
	}
	latest.Card = `{"body":"after success"}`
	stage(t, s, 102, latest)
	update := claimMirror(t, s, "human", 2001)
	if update.Operation != "update" || update.TargetMessageID != "message" {
		t.Fatalf("update lost confirmed target: %+v", update)
	}
	if err := s.DeferMirrorAttempt(ctx, update.ID, 2001, 0); err != nil {
		t.Fatal(err)
	}
	if _, due, count := retryState(t, s); due != 3001 || count != 1 {
		t.Fatal("successful delivery did not reset backoff")
	}
}

func TestMirrorDeferralBackoffAndServerFloor(t *testing.T) {
	ctx := context.Background()
	s, _, a := retryFixture(t)
	now := int64(1000)
	for i, expected := range []int64{1000, 2000, 4000, 8000, 16000, 32000, 60000, 60000, 120000} {
		floor := int64(0)
		if i == 8 {
			floor = 120000
		}
		if err := s.DeferMirrorAttempt(ctx, a.ID, now, floor); err != nil {
			t.Fatal(err)
		}
		if id, due, count := retryState(t, s); id != a.ID || due != now+expected || count != int64(i+1) {
			t.Fatalf("backoff %d: %s %d %d", i, id, due, count)
		}
		now += expected
		a = claimMirror(t, s, "human", now)
	}
}

func TestMirrorDeferralRejectsAmbiguousFinalAndMismatchedWork(t *testing.T) {
	ctx := context.Background()
	for _, outcome := range []string{"unknown", "confirmed", "rejected", "recovered", "mismatched"} {
		t.Run(outcome, func(t *testing.T) {
			s, _, a := retryFixture(t)
			switch outcome {
			case "recovered":
				if err := s.RecoverMirrorDispatches(ctx, "host"); err != nil {
					t.Fatal(err)
				}
			case "mismatched":
				if _, err := s.DB.Exec(`UPDATE outbound SET state='unknown'`); err != nil {
					t.Fatal(err)
				}
			default:
				remote := ""
				if outcome == "confirmed" {
					remote = "message"
				}
				if err := s.FinishMirrorAttempt(ctx, a.ID, outcome, remote); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.DeferMirrorAttempt(ctx, a.ID, 2000, 0); !errors.Is(err, ErrConflict) {
				t.Fatalf("unsafe retry accepted: %v", err)
			}
			var count int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_throttles`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unsafe retry persisted: %d %v", count, err)
			}
		})
	}
}

func TestMirrorDeferralOverflowRollsBack(t *testing.T) {
	ctx := context.Background()
	s, _, a := retryFixture(t)
	for _, tc := range []struct{ now, floor int64 }{{0, 0}, {1000, -1}, {math.MaxInt64, 0}, {1000, math.MaxInt64}} {
		if err := s.DeferMirrorAttempt(ctx, a.ID, tc.now, tc.floor); err == nil {
			t.Fatalf("invalid deadline accepted: %+v", tc)
		}
	}
	if got, err := s.MirrorAttempt(ctx, a.ID); err != nil || got.State != "dispatching" {
		t.Fatalf("failed deferral changed attempt: %+v %v", got, err)
	}
	if err := s.DeferMirrorAttempt(ctx, a.ID, 1000, 0); err != nil {
		t.Fatal(err)
	}
	next := claimMirror(t, s, "human", 2000)
	if _, err := s.DB.Exec(`UPDATE outbound_throttles SET rejection_count=?`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferMirrorAttempt(ctx, next.ID, 2000, 0); err == nil {
		t.Fatal("rejection count overflow accepted")
	}
	if id, due, count := retryState(t, s); id != a.ID || due != 2000 || count != math.MaxInt64 {
		t.Fatal("overflow corrupted throttle")
	}
}

func TestMirrorRetryDispatchCrashStaysUnknown(t *testing.T) {
	ctx := context.Background()
	for _, outcome := range []string{"unknown", "rejected", "crash"} {
		t.Run(outcome, func(t *testing.T) {
			s, _, first := retryFixture(t)
			if err := s.DeferMirrorAttempt(ctx, first.ID, 1000, 0); err != nil {
				t.Fatal(err)
			}
			second := claimMirror(t, s, "human", 2000)
			if outcome == "crash" {
				if err := s.RecoverMirrorDispatches(ctx, "host"); err != nil {
					t.Fatal(err)
				}
			} else if err := s.FinishMirrorAttempt(ctx, second.ID, outcome, ""); err != nil {
				t.Fatal(err)
			}
			if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", "human", 1, 90000); err != nil || claimed {
				t.Fatalf("unresolved retry sent again: %t %v", claimed, err)
			}
			if err := s.DeferMirrorAttempt(ctx, second.ID, 90000, 0); !errors.Is(err, ErrConflict) {
				t.Fatalf("frozen retry reopened: %v", err)
			}
			if outcome != "crash" {
				var count int
				if err := s.DB.QueryRow(`SELECT COUNT(*) FROM outbound_throttles`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("finished retry retained obsolete throttle: %d %v", count, err)
				}
			}
		})
	}
}

func TestMirrorDeferralDoesNotBlockIndependentThread(t *testing.T) {
	ctx := context.Background()
	s, _, first := retryFixture(t)
	if err := s.DeferMirrorAttempt(ctx, first.ID, 1000, 120000); err != nil {
		t.Fatal(err)
	}
	eligible(t, s, "second")
	intent, err := s.PrepareGroup(ctx, "host", "second", "owner", "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimGroupDispatch(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmGroup(ctx, intent.ID, "second-chat"); err != nil {
		t.Fatal(err)
	}
	if err = s.BindConfirmedGroup(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.StageMirrors(ctx, "host", "second", []Mirror{mirrorDoc("human", "human", "independent")}, Watermark{ObservedAtMS: 100}); err != nil {
		t.Fatal(err)
	}
	if a, claimed, err := s.ClaimMirror(ctx, "host", "second", "human", 1, 1001); err != nil || !claimed || a.ChatID != "second-chat" {
		t.Fatalf("independent thread delayed: %+v %t %v", a, claimed, err)
	}
}

func TestMirrorDeferralReevaluatesOnlyRejectedDispatchMentions(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{"off", "unreachable", "quiet"} {
		t.Run(reason, func(t *testing.T) {
			event := time.Date(2026, 10, 12, 21, 59, 58, 0, time.UTC)
			policy := rules.Policy{Enabled: true, Timezone: "UTC", QuietStart: "22:00", QuietEnd: "08:00"}
			s, _, options, m := mentionFixture(t, policy, event)
			if err := s.ScheduleMentions(ctx, "host", "thread", options); err != nil {
				t.Fatal(err)
			}
			bindMirrorThread(t, s)
			first := mentionClaim(t, s, options, event.Add(time.Second))
			if !hasMention(first.Card) {
				t.Fatal("initial attempt lacks eligible mention")
			}
			for _, state := range []string{"suppressed", "unknown"} {
				if _, err := s.DB.Exec(`INSERT INTO mentions(host_id,thread_id,mirror_key,open_id,state,reason,policy_revision,attempt_id) VALUES('host','thread','final',?,?,?,0,?)`, "ou_"+state, state, "original", first.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.DeferMirrorAttempt(ctx, first.ID, first.CreatedAtMS, 2000); err != nil {
				t.Fatal(err)
			}
			decisions, err := s.MentionDecisions(ctx, "host", "thread", "final")
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range decisions {
				if d.OpenID == "ou_user" {
					if d.State != "eligible" || d.AttemptID != "" {
						t.Fatalf("definite rejected mention not released: %+v", d)
					}
				} else if d.State == "eligible" || d.AttemptID != first.ID || d.Reason != "original" {
					t.Fatalf("unrelated mention changed: %+v", d)
				}
			}
			retryAt := event.Add(3 * time.Second)
			switch reason {
			case "off":
				policy.Enabled, policy.Revision = false, 1
				if err := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, policy, event.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
			case "unreachable":
				options.MemberIDs = nil
			}
			second := mentionClaim(t, s, options, retryAt)
			if second.ID == first.ID || hasMention(second.Card) || second.Card != expectedMarkedCard(t, s, second.ID, m.Card) {
				t.Fatalf("retry retained obsolete mention: %+v", second)
			}
			decisions, err = s.MentionDecisions(ctx, "host", "thread", "final")
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range decisions {
				if d.OpenID == "ou_user" && (d.State != "suppressed" || d.Reason != reason) {
					t.Fatalf("retry did not reevaluate %s: %+v", reason, d)
				}
			}
			frozen, err := s.MirrorAttempt(ctx, first.ID)
			if err != nil || frozen.Card != first.Card {
				t.Fatal("retry changed immutable mention payload", err)
			}
		})
	}
}

func TestVersionNineMigrationPreservesMirrorAndM0Journals(t *testing.T) {
	ctx := context.Background()
	s, path, first := retryFixture(t)
	if _, err := s.Begin(ctx, "native-add", "queue.add"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishMirrorAttempt(ctx, first.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DROP TABLE legacy_adoptions; DROP TABLE outbound_throttles; PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	frozen, err := s.MirrorAttempt(ctx, first.ID)
	if err != nil || frozen.State != "unknown" || frozen.Card != first.Card {
		t.Fatalf("migration changed outbound journal: %+v %v", frozen, err)
	}
	if state, _, err := s.Operation(ctx, "native-add"); err != nil || state != "sending" {
		t.Fatalf("migration changed native journal: %s %v", state, err)
	}
	var version int
	if err := s.DB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("migration version: %d %v", version, err)
	}
	if _, err := s.DB.Exec(`INSERT INTO outbound_throttles(host_id,thread_id,mirror_key,last_attempt_id,next_retry_at_ms,rejection_count) VALUES('host','thread','human','missing',1000,1)`); err == nil {
		t.Fatal("throttle accepts nonexistent attempt")
	}
	if err := s.DB.QueryRow(`SELECT last_attempt_id FROM outbound_throttles`).Scan(new(string)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("migration seeded retry without rejection proof: %v", err)
	}
}
