package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
)

func TestIdleThreadRequiresOldConfirmedIdleMetadataAndTerminalHistory(t *testing.T) {
	now := time.Unix(2000000000, 0)
	old := codexclient.Thread{UpdatedAt: now.Add(-48 * time.Hour).Unix()}
	old.Status.Type = "idle"
	completed := []codexclient.Turn{{Status: "completed"}}
	if !idleThread(old, completed, now) {
		t.Fatal("old idle thread stayed hot")
	}
	for _, tc := range []struct {
		name   string
		update int64
		status string
		turn   string
	}{
		{"unknown activity", 0, "idle", "completed"},
		{"at cutoff", now.Add(-24 * time.Hour).Unix(), "idle", "completed"},
		{"recent activity", now.Unix(), "idle", "completed"},
		{"active", old.UpdatedAt, "active", "completed"},
		{"unknown status", old.UpdatedAt, "", "completed"},
		{"unfinished history", old.UpdatedAt, "idle", "inProgress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := old
			meta.UpdatedAt, meta.Status.Type = tc.update, tc.status
			if idleThread(meta, []codexclient.Turn{{Status: tc.turn}}, now) {
				t.Fatal("unconfirmed idle state reduced polling")
			}
		})
	}
}

func TestIdleActorSkipsNormalHistoryPollAndWakesImmediately(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	n := &deliveryRetryNative{runtimeNative: base}
	r.Native, r.DiscoveryEnabled = n, false
	r.IdlePollInterval = 50 * time.Millisecond
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
	t.Cleanup(func() { cancel(); <-done })
	runtimeWait(t, func() bool {
		mirrors, err := db.Mirrors(ctx, r.HostID, "thread")
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
	reads := n.reads.Load()
	clock.Add((16 * time.Second).Milliseconds())
	time.Sleep(120 * time.Millisecond)
	if n.reads.Load() != reads {
		t.Fatal("idle actor re-read history at the normal 15-second deadline")
	}
	clock.Add((5 * time.Minute).Milliseconds())
	runtimeWait(t, func() bool { return n.reads.Load() >= reads+2 })
	reads = n.reads.Load()
	// An event wakes the cold actor and restores normal polling for new activity.
	base.mu.Lock()
	meta := base.threads["thread"]
	meta.UpdatedAt, meta.Status.Type = clock.Load()/1000, "active"
	base.threads["thread"] = meta
	base.mu.Unlock()
	r.Wake("thread")
	runtimeWait(t, func() bool { return n.reads.Load() >= reads+2 })
	reads = n.reads.Load()
	clock.Add((16 * time.Second).Milliseconds())
	runtimeWait(t, func() bool { return n.reads.Load() >= reads+2 })
}
