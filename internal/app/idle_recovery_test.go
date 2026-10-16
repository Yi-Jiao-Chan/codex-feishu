package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type idleReadbackGateway struct {
	*runtimeGateway
	message             feishu.CardMessage
	listCalls, getCalls atomic.Int32
}

func (g *idleReadbackGateway) ReadCardMessage(context.Context, string) (feishu.CardMessage, error) {
	g.getCalls.Add(1)
	return g.message, nil
}

func (g *idleReadbackGateway) ListCardMessages(_ context.Context, kind, id, cursor string) (feishu.CardMessagePage, error) {
	if kind != "chat" || id != g.message.ChatID || cursor != "" {
		return feishu.CardMessagePage{}, errors.New("unexpected idle readback scope")
	}
	g.listCalls.Add(1)
	return feishu.CardMessagePage{Items: []feishu.CardMessage{g.message}}, nil
}

func waitIdleMirrorsDelivered(t *testing.T, ctx context.Context, db *store.Store, thread string) {
	t.Helper()
	runtimeWait(t, func() bool {
		mirrors, err := db.Mirrors(ctx, "host", thread)
		if err != nil || len(mirrors) == 0 {
			return false
		}
		for _, mirror := range mirrors {
			if mirror.Revision != mirror.DeliveredRevision {
				return false
			}
		}
		return true
	})
}

func TestIdleColdTicksReconcileCallbacksWithoutNativeHistory(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	n := &deliveryRetryNative{runtimeNative: base}
	r.Native, r.DiscoveryEnabled = n, false
	r.IdlePollInterval = 20 * time.Millisecond
	r.IdleHistoryInterval = 5 * time.Minute
	var clock atomic.Int64
	clock.Store(time.Now().UnixMilli())
	r.Now = func() time.Time { return time.UnixMilli(clock.Load()) }
	base.set("thread", r.AllowedRoots[0], "completed")
	pinMeta(base, "thread", "", "Task")
	runtimeBind(t, db, r, "thread", "chat")
	var afterCalls, waitingCalls atomic.Int32
	r.AfterDelivery = func(context.Context, string, []codexclient.Turn) error {
		afterCalls.Add(1)
		return nil
	}
	r.WaitingReminder = func(context.Context, string) (bool, error) {
		waitingCalls.Add(1)
		return false, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitIdleMirrorsDelivered(t, ctx, db, "thread")
	runtimeWait(t, func() bool { return afterCalls.Load() > 0 && waitingCalls.Load() > 0 })
	due, err := db.HasDueMirrors(ctx, r.HostID, "thread", clock.Load(), r.MinProgressIntervalMS)
	if err != nil || due {
		t.Fatalf("idle fixture retained ready mirrors: due=%v err=%v", due, err)
	}
	// Let the actor finish its first transition to cold before establishing the
	// native-read baseline. The fake clock stays fixed during this settling time.
	time.Sleep(3 * r.PollInterval)
	reads := n.reads.Load()
	initialAfter, initialWaiting := afterCalls.Load(), waitingCalls.Load()
	clock.Add((16 * time.Second).Milliseconds())
	runtimeWait(t, func() bool {
		return afterCalls.Load() >= initialAfter+2 && waitingCalls.Load() >= initialWaiting+2
	})
	if got := n.reads.Load(); got != reads {
		t.Fatalf("cold callback reconciliation reread native history: reads=%d want=%d", got, reads)
	}
}

func TestIdleColdTickReadbackConfirmsUnknownWithoutResendOrNativeRead(t *testing.T) {
	db, base, gateway, r := runtimeFixture(t)
	reader := &idleReadbackGateway{runtimeGateway: gateway}
	n := &deliveryRetryNative{runtimeNative: base}
	r.Native, r.Messages, r.DiscoveryEnabled = n, reader, false
	r.IdlePollInterval = 20 * time.Millisecond
	r.IdleHistoryInterval = 5 * time.Minute
	var clock atomic.Int64
	clock.Store(time.Now().UnixMilli())
	r.Now = func() time.Time { return time.UnixMilli(clock.Load()) }
	base.set("thread", r.AllowedRoots[0], "completed")
	pinMeta(base, "thread", "", "Task")
	runtimeBind(t, db, r, "thread", "chat")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitIdleMirrorsDelivered(t, ctx, db, "thread")
	time.Sleep(3 * r.PollInterval)
	reads := n.reads.Load()
	gateway.mu.Lock()
	writes := len(gateway.writes)
	gateway.mu.Unlock()
	var attemptID string
	if err := db.DB.QueryRowContext(ctx, `SELECT attempt_id FROM outbound_attempts WHERE host_id=? AND thread_id=? ORDER BY created_at_ms,rowid LIMIT 1`, r.HostID, "thread").Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	attempt, err := db.MirrorAttempt(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	deleted := false
	parent := attempt.ParentMessageID
	reader.message = feishu.CardMessage{ID: "readback-recovered", ChatID: attempt.ChatID, Type: "interactive", SenderID: r.AppID, SenderIDType: "app_id", SenderType: "app", ParentID: nil, Deleted: &deleted, Content: attempt.Card}
	if parent != "" {
		reader.message.ParentID = &parent
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE outbound_attempts SET state='unknown',remote_id='' WHERE attempt_id=?`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE outbound SET state='unknown' WHERE host_id=? AND thread_id=? AND mirror_key=?`, attempt.HostID, attempt.ThreadID, attempt.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE mirrors SET remote_id='' WHERE host_id=? AND thread_id=? AND mirror_key=?`, attempt.HostID, attempt.ThreadID, attempt.Key); err != nil {
		t.Fatal(err)
	}
	due, err := db.HasDueMirrors(ctx, r.HostID, "thread", clock.Load(), r.MinProgressIntervalMS)
	if err != nil || due {
		t.Fatalf("unknown-only outbox became due work: due=%v err=%v", due, err)
	}
	runtimeWait(t, func() bool {
		current, e := db.MirrorAttempt(ctx, attempt.ID)
		return e == nil && current.State == "confirmed" && reader.listCalls.Load() > 0 && reader.getCalls.Load() > 0
	})
	if reader.listCalls.Load() != 1 || reader.getCalls.Load() != 1 {
		t.Fatalf("unexpected readback calls: list=%d get=%d", reader.listCalls.Load(), reader.getCalls.Load())
	}
	gateway.mu.Lock()
	finalWrites := len(gateway.writes)
	gateway.mu.Unlock()
	if finalWrites != writes {
		t.Fatalf("unknown readback resent a card: writes=%d want=%d", finalWrites, writes)
	}
	if got := n.reads.Load(); got != reads {
		t.Fatalf("cold readback reread native history: reads=%d want=%d", got, reads)
	}
}

func TestIdleSubmissionBlockedKeepsOldActorHot(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	n := &deliveryRetryNative{runtimeNative: base}
	r.Native, r.DiscoveryEnabled = n, false
	r.IdlePollInterval = 20 * time.Millisecond
	r.IdleHistoryInterval = 5 * time.Minute
	var clock atomic.Int64
	clock.Store(time.Now().UnixMilli())
	r.Now = func() time.Time { return time.UnixMilli(clock.Load()) }
	base.set("thread", r.AllowedRoots[0], "completed")
	pinMeta(base, "thread", "", "Task")
	runtimeBind(t, db, r, "thread", "chat")
	ctx := context.Background()
	receipt, claimed, err := db.Claim(ctx, store.Receipt{AppID: r.AppID, ThreadID: "thread", ChatID: "chat", MessageID: "native-input", Body: "fixture input"})
	if err != nil || !claimed {
		t.Fatalf("seed receipt: claimed=%v err=%v", claimed, err)
	}
	scope := store.SubmissionScope{HostID: r.HostID, AppID: r.AppID, ThreadID: "thread"}
	if err := db.StageSubmissions(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE submission_inputs SET state='unknown' WHERE host_id=? AND app_id=? AND thread_id=? AND receipt_id=?`, r.HostID, r.AppID, "thread", receipt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `UPDATE m0_receipts SET state='unknown' WHERE id=? AND app_id=? AND thread_id=?`, receipt.ID, r.AppID, "thread"); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitIdleMirrorsDelivered(t, runCtx, db, "thread")
	due, err := db.HasDueMirrors(runCtx, r.HostID, "thread", clock.Load(), r.MinProgressIntervalMS)
	if err != nil || due {
		t.Fatalf("submission fixture retained ready mirrors: due=%v err=%v", due, err)
	}
	reads := n.reads.Load()
	clock.Add((16 * time.Second).Milliseconds())
	runtimeWait(t, func() bool { return n.reads.Load() >= reads+2 })
	if n.reads.Load() < reads+2 {
		t.Fatal("unknown native submission incorrectly allowed cold polling")
	}
}
