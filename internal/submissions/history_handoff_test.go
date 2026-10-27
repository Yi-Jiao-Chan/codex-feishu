package submissions

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/codexclient"
)

type historyCountingNative struct {
	*fakeNative
	reads int
}

func (n *historyCountingNative) Turns(ctx context.Context, thread string) ([]codexclient.Turn, error) {
	n.reads++
	return n.fakeNative.Turns(ctx, thread)
}

func TestTickWithHistoryHandsOffFreshSnapshotWithoutRetainingIt(t *testing.T) {
	c, native, _ := fixture(t, true)
	n := &historyCountingNative{fakeNative: native}
	c.Native = n
	ctx := context.Background()
	turns, err := c.TickWithHistory(ctx)
	if err != nil || len(turns) != 1 || n.reads != 1 {
		t.Fatalf("history handoff required extra reads: turns=%d reads=%d err=%v", len(turns), n.reads, err)
	}
	if c.historyHandoff != nil {
		t.Fatal("tick retained a raw history reference")
	}
	native.mu.Lock()
	native.end()
	native.start("external-app-input")
	native.mu.Unlock()
	turns, err = c.TickWithHistory(ctx)
	if err != nil || len(turns) != 2 || n.reads != 2 || turns[1].Items[0].ClientID == nil || *turns[1].Items[0].ClientID != "external-app-input" {
		t.Fatal("next tick reused stale native history", err)
	}
}

func TestTickWithHistoryUsesPostAddEvidenceAndDropsFailedHandoff(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "lost-reply"}[lostReply], func(t *testing.T) {
			c, native, _ := fixture(t, false)
			n := &historyCountingNative{fakeNative: native}
			c.Native = n
			r := claim(t, c, "new-input")
			if lostReply {
				native.addReplyErr = errors.New("fixture response lost")
			}
			turns, err := c.TickWithHistory(context.Background())
			if lostReply {
				if err == nil || turns != nil {
					t.Fatal("failed tick exposed its pre-write history")
				}
				turns, err = c.TickWithHistory(context.Background())
			}
			if err != nil || len(turns) != 1 || native.addCalls != 1 {
				t.Fatal("post-add evidence missing or write retried", err)
			}
			if turns[0].Items[0].ClientID == nil || *turns[0].Items[0].ClientID != r.ClientID {
				t.Fatal("handoff predates the accepted input")
			}
			if c.historyHandoff != nil {
				t.Fatal("tick retained native history")
			}
		})
	}
}
