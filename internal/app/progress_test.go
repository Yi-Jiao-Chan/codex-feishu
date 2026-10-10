package app

import (
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeMessages struct {
	sends, updates []string
	parents        []string
	added, deleted []string
	reactionErr    error
	updateErr      error
	sendErr        error
}

func (f *fakeMessages) SendCard(_ context.Context, _ string, _ string, card string, _ string) (string, error) {
	f.sends = append(f.sends, card)
	return fmt.Sprint("msg-", len(f.sends)), f.sendErr
}
func (f *fakeMessages) ReplyCard(ctx context.Context, parent, card, uuid string) (string, error) {
	f.parents = append(f.parents, parent)
	return f.SendCard(ctx, "chat_id", "chat", card, uuid)
}
func (f *fakeMessages) UpdateCard(_ context.Context, _ string, card string) error {
	f.updates = append(f.updates, card)
	return f.updateErr
}
func (f *fakeMessages) AddReaction(_ context.Context, _ string, emoji string) (string, error) {
	f.added = append(f.added, emoji)
	return fmt.Sprint("reaction-", len(f.added)), f.reactionErr
}
func (f *fakeMessages) DeleteReaction(_ context.Context, _ string, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func harness(t *testing.T) (*M0, *fakeMessages) {
	t.Helper()
	db, e := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	g := &fakeMessages{}
	return &M0{db: db, g: g, thread: "thread", chat: "chat", turnParents: map[string]string{"turn": "original"}}, g
}
func TestTurnProgressCoalescesAndOnlyPublishesFinal(t *testing.T) {
	m, g := harness(t)
	ctx := context.Background()
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", Items: []codexclient.Item{{Type: "agentMessage", ID: "c1", Text: "starting", Phase: str("commentary")}, {Type: "reasoning", Text: "private"}}}
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	turn.Items = append(turn.Items, codexclient.Item{Type: "agentMessage", ID: "c2", Text: "still working", Phase: str("commentary")})
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	if len(g.sends) != 1 || len(g.updates) != 0 {
		t.Fatal("progress sent twice or throttle ignored")
	}
	m.progress[turn.ID] = progressState{updated: time.Now().Add(-3 * time.Second)}
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	if len(g.updates) != 1 || !strings.Contains(g.updates[0], "still working") || strings.Contains(g.updates[0], "private") {
		t.Fatal("public progress not replaced correctly")
	}
	turn.Status = "completed"
	turn.Items = append(turn.Items, codexclient.Item{Type: "agentMessage", ID: "final", Text: "result", Phase: str("final_answer")})
	turn.Items = append(turn.Items, codexclient.Item{Type: "agentMessage", ID: "final2", Text: "extra result", Phase: str("final_answer")})
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	if len(g.sends) != 2 || !strings.Contains(g.sends[1], "result") {
		t.Fatal("one final required")
	}
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	// Reconnect / restart reads the same turns, without duplicate progress or final.
	m.progress = nil
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	if len(g.sends) != 2 {
		t.Fatal("final replayed")
	}
}
func TestCompletedCommentaryIsNotFinalAndLegacyUsesLast(t *testing.T) {
	m, g := harness(t)
	ctx := context.Background()
	turn := codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{{Type: "agentMessage", Text: "waiting", Phase: str("commentary")}}}
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	if len(g.sends) != 0 {
		t.Fatal("commentary mistaken for final")
	}
	turn.Items = append(turn.Items, codexclient.Item{Type: "agentMessage", ID: "a", Text: "old progress"}, codexclient.Item{Type: "agentMessage", ID: "b", Text: "legacy final"})
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	if len(g.sends) != 1 || !strings.Contains(g.sends[0], "legacy final") {
		t.Fatal("legacy selection")
	}
}
func TestOldMirroredFinalDoesNotReplay(t *testing.T) {
	m, g := harness(t)
	ctx := context.Background()
	key := "m0.mirror.thread.final.0"
	m.db.Begin(ctx, key, "legacy")
	m.db.Finish(ctx, key, "confirmed", "old-card")
	e := m.syncTurn(ctx, codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{{Type: "agentMessage", ID: "final", Text: "old result", Phase: str("final_answer")}}})
	if e != nil || len(g.sends) != 0 {
		t.Fatalf("old final replayed %v", e)
	}
}
func TestReactionLifecycleAndAmbiguousWriteDoNotRetry(t *testing.T) {
	m, g := harness(t)
	ctx := context.Background()
	r := store.Receipt{ID: "receipt", MessageID: "original", State: "received"}
	m.syncReaction(ctx, r, codexclient.Turn{})
	r.State = "queued"
	m.syncReaction(ctx, r, codexclient.Turn{})
	r.State = "consumed"
	turn := codexclient.Turn{ID: "turn", Status: "inProgress"}
	m.syncReaction(ctx, r, turn)
	turn.Status = "completed"
	m.syncReaction(ctx, r, turn)
	m.syncReaction(ctx, r, turn)
	if strings.Join(g.added, ",") != "Get,THINKING,OnIt,CheckMark" || len(g.deleted) != 3 {
		t.Fatalf("wrong lifecycle %v %v", g.added, g.deleted)
	}
	g.reactionErr = &feishu.Unknown{Operation: "POST reaction"}
	r.ID = "ambiguous"
	r.State = "queued"
	m.syncReaction(ctx, r, codexclient.Turn{})
	m.syncReaction(ctx, r, codexclient.Turn{})
	r.State = "consumed"
	m.syncReaction(ctx, r, turn)
	if len(g.added) != 5 {
		t.Fatal("ambiguous reaction retried")
	}
}

func TestProgressFailureStillSendsFinal(t *testing.T) {
	m, g := harness(t)
	ctx := context.Background()
	turn := codexclient.Turn{ID: "turn", Status: "inProgress"}
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	g.updateErr = &feishu.Rejected{Code: 230011}
	turn.Status = "completed"
	turn.Items = []codexclient.Item{{Type: "agentMessage", ID: "final", Text: "result", Phase: str("final_answer")}}
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	if len(g.sends) != 2 || !m.finalDelivered(ctx, turn) {
		t.Fatal("progress failure blocked final")
	}
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	if len(g.updates) != 1 {
		t.Fatal("dead progress card updated repeatedly")
	}
}
func TestUnknownFinalNeverCreatesCompletionReaction(t *testing.T) {
	m, g := harness(t)
	ctx := context.Background()
	r := store.Receipt{ID: "receipt", MessageID: "original", State: "consumed"}
	turn := codexclient.Turn{ID: "turn", Status: "inProgress"}
	m.syncReaction(ctx, r, turn)
	g.sendErr = &feishu.Unknown{Operation: "send final"}
	turn.Status = "completed"
	turn.Items = []codexclient.Item{{Type: "agentMessage", ID: "final", Text: "result", Phase: str("final_answer")}}
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	m.syncReaction(ctx, r, turn)
	if e := m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	m.syncReaction(ctx, r, turn)
	if len(g.sends) != 1 || strings.Join(g.added, ",") != "OnIt" || len(g.deleted) != 1 || m.finalDelivered(ctx, turn) {
		t.Fatal("ambiguous final replayed or marked delivered")
	}
}

func TestTurnReplyAnchorsReceiptOrAppMirror(t *testing.T) {
	m, g := harness(t)
	ctx := context.Background()
	m.turnParents = nil
	original, _, e := m.db.Claim(ctx, store.Receipt{AppID: "app", MessageID: "om_original", ThreadID: m.thread, ChatID: m.chat, Body: "original"})
	if e != nil {
		t.Fatal(e)
	}
	steer, _, e := m.db.Claim(ctx, store.Receipt{AppID: "app", MessageID: "om_steer", ThreadID: m.thread, ChatID: m.chat, Body: "steer"})
	if e != nil {
		t.Fatal(e)
	}
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", Items: []codexclient.Item{{Type: "userMessage", ID: "first", ClientID: &original.ClientID}, {Type: "userMessage", ID: "steer", ClientID: &steer.ClientID}}}
	if e = m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	turn.Status = "completed"
	turn.Items = append(turn.Items, codexclient.Item{Type: "agentMessage", ID: "final", Text: "result", Phase: str("final_answer")})
	if e = m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	if strings.Join(g.parents, ",") != "om_original,om_original" {
		t.Fatalf("turn anchor shifted to steer: %v", g.parents)
	}
	steer.State = "queued"
	if e = m.receiptCard(ctx, &steer); e != nil {
		t.Fatal(e)
	}
	if g.parents[2] != "om_steer" {
		t.Fatal("queue card quoted another input")
	}
	m.db.Begin(ctx, "m0.mirror.thread.app-input.0", "feishu.mirror")
	m.db.Finish(ctx, "m0.mirror.thread.app-input.0", "confirmed", "om_app_mirror")
	parent, e := m.turnParent(ctx, codexclient.Turn{ID: "app-turn", Items: []codexclient.Item{{Type: "userMessage", ID: "app-input"}}})
	if e != nil || parent != "om_app_mirror" {
		t.Fatalf("App mirror anchor %q %v", parent, e)
	}
}
func TestMissingFirstInputAnchorDoesNotQuoteLaterSteer(t *testing.T) {
	m, g := harness(t)
	ctx := context.Background()
	m.turnParents = nil
	r, _, e := m.db.Claim(ctx, store.Receipt{AppID: "app", MessageID: "later", ThreadID: m.thread, ChatID: m.chat, Body: "steer"})
	if e != nil {
		t.Fatal(e)
	}
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", Items: []codexclient.Item{{Type: "userMessage", ID: "missing-first"}, {Type: "userMessage", ID: "steer", ClientID: &r.ClientID}}}
	if e = m.syncTurn(ctx, turn); e != nil {
		t.Fatal(e)
	}
	if len(g.sends) != 0 {
		t.Fatal("unanchored turn sent without its true parent")
	}
}
