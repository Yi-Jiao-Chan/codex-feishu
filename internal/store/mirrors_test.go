package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func mirrorDoc(key, kind, body string) Mirror {
	m := Mirror{Key: key, TurnID: "turn", Kind: kind, Card: `{"body":"` + body + `"}`}
	if kind != "human" {
		m.ParentKey = "human"
	}
	return m
}

func stage(t *testing.T, s *Store, at int64, docs ...Mirror) {
	t.Helper()
	if err := s.StageMirrors(context.Background(), "host", "thread", docs, Watermark{TurnID: "turn", ItemID: "last", ObservedAtMS: at}); err != nil {
		t.Fatal(err)
	}
}

func bindMirrorThread(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	i, err := s.PrepareGroup(ctx, "host", "thread", "owner", "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimGroupDispatch(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmGroup(ctx, i.ID, "chat"); err != nil {
		t.Fatal(err)
	}
	if err = s.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
}

func claimMirror(t *testing.T, s *Store, key string, at int64) MirrorAttempt {
	t.Helper()
	ctx := context.Background()
	m, err := s.Mirror(ctx, "host", "thread", key)
	if err != nil {
		t.Fatal(err)
	}
	a, claimed, err := s.ClaimMirror(ctx, "host", "thread", key, m.Revision, at)
	if err != nil || !claimed {
		t.Fatalf("claim %s: %v %v", key, claimed, err)
	}
	return a
}

func TestMirrorsBeforeBindingAreDurableAndReplyWaitsForParent(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	human, progress := mirrorDoc("human", "human", "input"), mirrorDoc("progress", "progress", "working")
	stage(t, s, 100, human, progress)
	if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", "human", 1, 100); err != nil || claimed {
		t.Fatalf("unbound write: %v %v", claimed, err)
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bindMirrorThread(t, s)
	if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", "progress", 1, 101); err != nil || claimed {
		t.Fatalf("parent missing but reply claimed: %v %v", claimed, err)
	}
	a := claimMirror(t, s, "human", 102)
	if a.Operation != "send" || a.Card != human.Card || a.ChatID != "chat" || a.ParentMessageID != "" {
		t.Fatalf("incorrect initial payload: %+v", a)
	}
	if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "human-message"); err != nil {
		t.Fatal(err)
	}
	p := claimMirror(t, s, "progress", 103)
	if p.Operation != "reply" || p.ParentMessageID != "human-message" {
		t.Fatalf("missing native parent relation: %+v", p)
	}
	if err := s.FinishMirrorAttempt(ctx, p.ID, "confirmed", "progress-message"); err != nil {
		t.Fatal(err)
	}
	stage(t, s, 104, human, progress)
	if ready, err := s.ReadyMirrors(ctx, "host", "thread"); err != nil || len(ready) != 0 {
		t.Fatalf("replay sends duplicates: %d %v", len(ready), err)
	}
}

func TestMirrorSnapshotRollbackDoesNotAdvanceWatermark(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	stage(t, s, 100, mirrorDoc("human", "human", "original"))
	bad := mirrorDoc("final", "final", "answer")
	bad.ParentKey = ""
	if err := s.StageMirrors(ctx, "host", "thread", []Mirror{mirrorDoc("human", "human", "changed"), bad}, Watermark{ObservedAtMS: 200}); err == nil {
		t.Fatal("missing reply parent accepted")
	}
	m, _ := s.Mirror(ctx, "host", "thread", "human")
	w, err := s.Watermark(ctx, "host", "thread")
	if err != nil || w.ObservedAtMS != 100 || m.Revision != 1 || m.Card != mirrorDoc("human", "human", "original").Card {
		t.Fatalf("partial snapshot committed: %+v %+v %v", m, w, err)
	}
	if _, err := s.Mirror(ctx, "host", "thread", "final"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("partial final persisted: %v", err)
	}
	if err := s.StageMirrors(ctx, "host", "thread", []Mirror{mirrorDoc("human", "human", "stale")}, Watermark{ObservedAtMS: 99}); !errors.Is(err, ErrConflict) {
		t.Fatalf("older snapshot accepted: %v", err)
	}
}

func TestProgressCoalescesInflightContentAndTerminalCannotRegress(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	p := mirrorDoc("progress", "progress", "starting")
	p.ParentKey, p.ParentMessageID = "", "feishu-input"
	stage(t, s, 100, p)
	first := claimMirror(t, s, "progress", 1000)
	p.Card = `{"body":"new summary"}`
	stage(t, s, 101, p)
	p.Card = `{"body":"latest summary"}`
	stage(t, s, 102, p)
	frozen, err := s.MirrorAttempt(ctx, first.ID)
	if err != nil || frozen.Card != first.Card || frozen.Revision != 1 {
		t.Fatalf("inflight payload changed: %+v %v", frozen, err)
	}
	if err := s.FinishMirrorAttempt(ctx, first.ID, "confirmed", "process-card"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", "progress", 3, 2999); err != nil || claimed {
		t.Fatalf("progress throttle bypassed: %v %v", claimed, err)
	}
	next := claimMirror(t, s, "progress", 3000)
	if next.Operation != "update" || next.TargetMessageID != "process-card" || next.ParentMessageID != "feishu-input" || next.Card != p.Card || next.Revision != 3 {
		t.Fatalf("coalescing/target lost: %+v", next)
	}
	p.Terminal, p.Card = true, `{"body":"done"}`
	stage(t, s, 103, p)
	if err := s.FinishMirrorAttempt(ctx, next.ID, "confirmed", "process-card"); err != nil {
		t.Fatal(err)
	}
	final := claimMirror(t, s, "progress", 3001)
	if final.Revision != 4 || final.Card != p.Card {
		t.Fatalf("terminal card delayed or lost: %+v", final)
	}
	if err := s.FinishMirrorAttempt(ctx, final.ID, "confirmed", "different-card"); !errors.Is(err, ErrConflict) {
		t.Fatalf("update changed remote ID: %v", err)
	}
	if err := s.FinishMirrorAttempt(ctx, final.ID, "confirmed", "process-card"); err != nil {
		t.Fatal(err)
	}
	p.Terminal, p.Card = false, `{"body":"late generation"}`
	stage(t, s, 104, p)
	m, _ := s.Mirror(ctx, "host", "thread", "progress")
	if !m.Terminal || m.Revision != 4 || m.DeliveredRevision != 4 || m.State != "confirmed" {
		t.Fatalf("late progress overwrote completion: %+v", m)
	}
	// A duplicated old response must not rewind the latest delivered revision.
	if err := s.FinishMirrorAttempt(ctx, first.ID, "confirmed", "process-card"); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Mirror(ctx, "host", "thread", "progress")
	if m.DeliveredRevision != 4 {
		t.Fatalf("duplicate response rewound progress: %+v", m)
	}
}

func TestMirrorCrashFreezesUnknownAndDoesNotBlockIndependentFinal(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	p := mirrorDoc("progress", "progress", "working")
	p.ParentKey, p.ParentMessageID = "", "original-input"
	stage(t, s, 100, p)
	a := claimMirror(t, s, "progress", 1000)
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverMirrorDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	p.Terminal, p.Card = true, `{"body":"done"}`
	f := mirrorDoc("final", "final", "final answer")
	f.ParentKey, f.ParentMessageID, f.Terminal = "", "original-input", true
	stage(t, s, 101, p, f)
	if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", "progress", 2, 4000); err != nil || claimed {
		t.Fatalf("unknown request redispatched: %v %v", claimed, err)
	}
	frozen, err := s.MirrorAttempt(ctx, a.ID)
	if err != nil || frozen.State != "unknown" || frozen.ID != a.ID || frozen.Card != a.Card {
		t.Fatalf("crash journal changed: %+v %v", frozen, err)
	}
	final := claimMirror(t, s, "final", 4001)
	if final.Operation != "reply" || final.ParentMessageID != "original-input" {
		t.Fatalf("independent final blocked or sent without relation: %+v", final)
	}
	if err := s.FinishMirrorAttempt(ctx, final.ID, "confirmed", "final-message"); err != nil {
		t.Fatal(err)
	}
	// Readback of the original unknown request allows updating the same card.
	if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "recovered-process"); err != nil {
		t.Fatal(err)
	}
	update := claimMirror(t, s, "progress", 4002)
	if update.Operation != "update" || update.TargetMessageID != "recovered-process" {
		t.Fatalf("readback created a second process card: %+v", update)
	}
}

func TestUnknownParentBlocksReplyWithoutPlainSendFallback(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	stage(t, s, 100, mirrorDoc("human", "human", "question"), mirrorDoc("final", "final", "answer"))
	a := claimMirror(t, s, "human", 1000)
	if err := s.FinishMirrorAttempt(ctx, a.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", "final", 1, 4000); err != nil || claimed {
		t.Fatalf("reply bypassed unknown parent: %v %v", claimed, err)
	}
}

func TestMirrorClaimsSerializePerThreadAndFreezeRejectedWork(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	m := mirrorDoc("human", "human", "question")
	stage(t, s, 100, m, mirrorDoc("second", "human", "next question"))
	var claims atomic.Int32
	var attempt MirrorAttempt
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "human"
			if i%2 == 0 {
				key = "second"
			}
			a, claimed, err := s.ClaimMirror(ctx, "host", "thread", key, 1, 1000)
			if err != nil {
				t.Error(err)
			}
			if claimed {
				claims.Add(1)
				mu.Lock()
				attempt = a
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("concurrent remote writes: %d", claims.Load())
	}
	if err := s.FinishMirrorAttempt(ctx, attempt.ID, "rejected", ""); err != nil {
		t.Fatal(err)
	}
	m = mirrorDoc(attempt.Key, "human", "changed after rejection")
	stage(t, s, 101, m)
	if _, claimed, err := s.ClaimMirror(ctx, "host", "thread", attempt.Key, 2, 5000); err != nil || claimed {
		t.Fatalf("rejected request automatically retried: %v %v", claimed, err)
	}
}

func TestMirrorReplayAndReplyTargetAreImmutable(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	m := mirrorDoc("human", "human", "live")
	stage(t, s, 100, m)
	m.Card, m.Replay = `{"body":"newer content"}`, true
	stage(t, s, 101, m)
	got, _ := s.Mirror(ctx, "host", "thread", m.Key)
	if got.Replay {
		t.Fatal("reconnection replaced live provenance")
	}
	p := mirrorDoc("progress", "progress", "working")
	stage(t, s, 102, p)
	p.ParentKey, p.ParentMessageID = "", "different-original"
	if err := s.StageMirrors(ctx, "host", "thread", []Mirror{p}, Watermark{ObservedAtMS: 103}); !errors.Is(err, ErrConflict) {
		t.Fatalf("reply target changed: %v", err)
	}
}

func TestVersionTwoMigrationPreservesDiscoveryAndM0Journals(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	if _, err := s.Begin(ctx, "lost-native-add", "queue.add"); err != nil {
		t.Fatal(err)
	}
	// Model a real v2 database: no v3 tables, retained discovery and M0 state.
	if _, err := s.DB.Exec(`DROP TABLE notification_rules; DROP TABLE input_history_links; DROP TABLE outbound_attempts; DROP TABLE outbound; DROP TABLE mirrors; DROP TABLE history_watermarks; PRAGMA user_version=2;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	obs, err := reopened.Observation(ctx, "host", "thread")
	if err != nil || obs.State != "eligible" {
		t.Fatalf("discovery state lost in migration: %+v %v", obs, err)
	}
	state, _, err := reopened.Operation(ctx, "lost-native-add")
	if err != nil || state != "sending" {
		t.Fatalf("native journal lost in migration: %s %v", state, err)
	}
	stage(t, reopened, 200, mirrorDoc("human", "human", "after migration"))
	var version int
	if err := reopened.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("wrong migration version: %d %v", version, err)
	}
}
