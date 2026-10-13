package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
)

type runtimeGroupRateFixture struct {
	*runtimeGateway
	calls atomic.Int32
}

func (g *runtimeGroupRateFixture) CreateGroupWithDescription(ctx context.Context, name, owner string, members []string, uuid, description string) (string, error) {
	if g.calls.Add(1) == 1 {
		return "", &feishu.RateLimited{RetryAfter: 10 * time.Second}
	}
	return g.runtimeGateway.CreateGroupWithDescription(ctx, name, owner, members, uuid, description)
}

func TestRuntimeGroupDeadlinePreservesFinalAndWakesWithoutNewInput(t *testing.T) {
	db, n, g, r := runtimeFixture(t)
	limited := &runtimeGroupRateFixture{runtimeGateway: g}
	r.Groups = limited
	r.ScanInterval = time.Hour // Retry must come from the actor tick, not a fresh scan.
	var at atomic.Int64
	at.Store(time.Now().UnixMilli())
	r.Now = func() time.Time { return time.UnixMilli(at.Load()) }
	staged := make(chan int64, 64)
	r.HistoryStaged = func(context.Context, string, []codexclient.Turn) error {
		select {
		case staged <- at.Load():
		default:
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	runtimeWait(t, func() bool { _, complete, err := db.BaselineFence(ctx, "host"); return err == nil && complete })
	n.set("new", r.AllowedRoots[0], "inProgress")
	meta, _ := n.Metadata(ctx, "new")
	body, _ := json.Marshal(map[string]any{"thread": meta})
	n.events <- codexclient.Event{Method: "thread/started", Params: body}
	var intent string
	var due int64
	runtimeWait(t, func() bool {
		err := db.DB.QueryRow("SELECT intent_id,next_retry_at_ms FROM group_throttles").Scan(&intent, &due)
		return err == nil
	})
	n.set("new", r.AllowedRoots[0], "completed")
	r.Wake("new")
	runtimeWait(t, func() bool {
		ms, err := db.Mirrors(ctx, "host", "new")
		if err != nil {
			return false
		}
		for _, m := range ms {
			if m.Kind == "final" && m.Terminal {
				return true
			}
		}
		return false
	})
	if _, err := db.Binding(ctx, "host", "new"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("binding before group confirmation", err)
	}
	at.Store(due - 1)
	r.Wake("new")
	runtimeWait(t, func() bool {
		for {
			select {
			case stamp := <-staged:
				if stamp == due-1 {
					return true
				}
			default:
				return false
			}
		}
	})
	if limited.calls.Load() != 1 {
		t.Fatal("created before rate deadline")
	}
	g.mu.Lock()
	writes := len(g.writes)
	g.mu.Unlock()
	if writes != 0 {
		t.Fatal("sent into unconfirmed target")
	}
	at.Store(due) // No App event or Feishu message: the existing tick wakes retry.
	runtimeWait(t, func() bool {
		ms, err := db.Mirrors(ctx, "host", "new")
		if err != nil {
			return false
		}
		for _, m := range ms {
			if m.Kind == "final" && m.State == "confirmed" {
				return true
			}
		}
		return false
	})
	if limited.calls.Load() != 2 {
		t.Fatal("retry did not create exactly one group", limited.calls.Load())
	}
	var rejected, confirmed int
	if err := db.DB.QueryRow("SELECT count(*) FROM group_attempts WHERE state='rejected'").Scan(&rejected); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow("SELECT count(*) FROM group_attempts WHERE state='confirmed'").Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if rejected != 1 || confirmed != 1 {
		t.Fatal("attempt evidence lost", rejected, confirmed)
	}
	g.mu.Lock()
	accepted := g.groups
	g.mu.Unlock()
	if accepted != 1 {
		t.Fatal("duplicate groups", accepted)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not join workers")
	}
}
