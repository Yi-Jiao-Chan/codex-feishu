package store

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDuplicateInboundAndRestartDoNotResubmit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bridge.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	var claims atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, fresh, e := s.Claim(ctx, Receipt{AppID: "app", MessageID: "m", ThreadID: "t", ChatID: "c", Body: "hello"})
			if e != nil {
				t.Error(e)
			}
			if fresh {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatal("duplicate receipt accepted")
	}
	if fresh, e := s.Begin(ctx, "add.r", "codex.queue.add"); e != nil || !fresh {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if fresh, e := s.Begin(ctx, "add.r", "codex.queue.add"); e != nil || fresh {
		t.Fatal("crashed sending request retried")
	}
	state, _, e := s.Operation(ctx, "add.r")
	if e != nil || state != "sending" {
		t.Fatal("lost journal")
	}
	rs, e := s.Receipts(ctx, "t")
	if e != nil || len(rs) != 1 || rs[0].ClientID == "" {
		t.Fatal("lost input correlation")
	}
}
func TestBindingCannotSilentlyMove(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "bridge.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	if e = s.Bind(ctx, "t", "c1"); e != nil {
		t.Fatal(e)
	}
	if e = s.Bind(ctx, "t", "c2"); e == nil {
		t.Fatal("binding moved")
	}
}
func TestDeploymentLockIsExclusiveAndReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	one, e := AcquireListener(path)
	if e != nil {
		t.Fatal(e)
	}
	if two, e := AcquireListener(path); e == nil {
		two.Close()
		one.Close()
		t.Fatal("second listener obtained deployment lock")
	}
	one.Close()
	next, e := AcquireListener(path)
	if e != nil {
		t.Fatal("lock not released")
	}
	next.Close()
}
