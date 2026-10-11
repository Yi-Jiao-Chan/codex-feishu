package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

// Exercise the real planner -> atomic schema adoption -> normal reconciler ->
// outbox path, without manually installing remote IDs into formal tables.
func TestLegacyAtomicAdoptionAndLiveSyncPreserveRouteAndHistory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "m0.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`
CREATE TABLE m0_operations(key TEXT PRIMARY KEY,kind TEXT NOT NULL,state TEXT NOT NULL,remote_id TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL);
CREATE TABLE m0_bindings(thread_id TEXT PRIMARY KEY,chat_id TEXT UNIQUE NOT NULL);
CREATE TABLE m0_receipts(id TEXT PRIMARY KEY,app_id TEXT NOT NULL,message_id TEXT NOT NULL,thread_id TEXT NOT NULL,chat_id TEXT NOT NULL,body TEXT NOT NULL,client_id TEXT NOT NULL UNIQUE,queue_id TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,card_id TEXT NOT NULL DEFAULT '',action_token TEXT NOT NULL UNIQUE,UNIQUE(app_id,message_id));
CREATE TABLE m0_actions(receipt_id TEXT PRIMARY KEY,kind TEXT NOT NULL,state TEXT NOT NULL);
CREATE TABLE m0_reactions(receipt_id TEXT PRIMARY KEY,emoji TEXT NOT NULL DEFAULT '',reaction_id TEXT NOT NULL DEFAULT '',revision INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL DEFAULT 'confirmed');
INSERT INTO m0_bindings VALUES('thread','existing-chat');
INSERT INTO m0_receipts VALUES('receipt','app','feishu-original','thread','existing-chat','feishu question','client','old-queue','consumed','','token');
INSERT INTO m0_reactions VALUES('receipt','CheckMark','reaction',3,'confirmed');
INSERT INTO m0_operations VALUES('m0.mirror.thread.u.0','feishu.mirror','confirmed','app-human-card',1);
INSERT INTO m0_operations VALUES('m0.mirror.thread.a.0','feishu.mirror','confirmed','app-final-card',1);
INSERT INTO m0_operations VALUES('m0.progress.thread.turn2','feishu.progress','confirmed','progress-card',1);
INSERT INTO m0_operations VALUES('m0.final.thread.turn2.0','feishu.final','confirmed','feishu-final-card',1);
INSERT INTO m0_operations VALUES('m0.receipt-card.receipt','feishu.send_card','confirmed','recalled-card',1);
PRAGMA user_version=1;
`)
	closeErr := raw.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenExisting(ctx, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	i, err := s.LegacyInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	client := "client"
	feishuInput := input("external-u", "feishu question")
	feishuInput.ClientID = &client
	turns := []codexclient.Turn{
		{ID: "turn1", Status: "completed", Items: []codexclient.Item{input("u", "app question"), agent("a", "app answer", "final_answer")}},
		{ID: "turn2", Status: "completed", Items: []codexclient.Item{feishuInput, agent("external-a", "feishu answer", "final_answer")}},
	}
	seed, err := PlanLegacyThread("host", store.LegacyThreadSeed{ThreadID: "thread", ChatID: "existing-chat", CWD: "/project", ProjectRoot: "/project", FirstUserItemID: "u", CreatedAt: 1}, turns, i, 10000, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := store.LegacyAdoption{HostID: "host", AppID: "app", OwnerID: "owner", SourceDigest: i.Digest(), NativeDigest: strings.Repeat("a", 64), StartedAtMS: 10000, Threads: []store.LegacyThreadSeed{seed}}
	if changed, err := s.AdoptLegacy(ctx, p); err != nil || !changed {
		t.Fatal("pure plan incompatible with atomic adoption", changed, err)
	}
	e := Engine{Store: s, HostID: "host"}
	if err := e.Apply(ctx, "thread", turns, true, 10001); err != nil {
		t.Fatal(err)
	}
	g := &cardGateway{cards: map[string]string{}}
	for _, m := range seed.Mirrors {
		g.cards[m.RemoteID] = "old card"
	}
	drain(t, e, g, 10002)
	if len(g.writes) != len(seed.Mirrors) || len(seed.Mirrors) != 4 {
		t.Fatal("historical input duplicated or original card lost")
	}
	for _, w := range g.writes {
		if w.Operation != "update" || w.ID == "recalled-card" {
			t.Fatal("adoption reposted or revived a recalled message")
		}
	}
	for _, turn := range turns {
		if delivered, err := e.FinalDelivered(ctx, "thread", turn.ID); err != nil || !delivered {
			t.Fatal("historical result delivery unproven", err)
		}
	}
	if err := e.Apply(ctx, "thread", turns, true, 10003); err != nil {
		t.Fatal(err)
	}
	drain(t, e, g, 10004)
	if len(g.writes) != 4 {
		t.Fatal("complete history re-sent after adoption")
	}
	b, err := s.Binding(ctx, "host", "thread")
	if err != nil || b.ChatID != "existing-chat" {
		t.Fatal("existing group binding changed", err)
	}
	if claimed, err := s.ClaimGroupDispatch(ctx, b.IntentID); err != nil || claimed {
		t.Fatal("adopted group recreation allowed", err)
	}
}
