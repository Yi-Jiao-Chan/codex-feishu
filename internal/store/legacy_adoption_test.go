package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func legacyStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "legacy # test.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
CREATE TABLE m0_operations(key TEXT PRIMARY KEY,kind TEXT NOT NULL,state TEXT NOT NULL,remote_id TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL);
CREATE TABLE m0_bindings(thread_id TEXT PRIMARY KEY,chat_id TEXT UNIQUE NOT NULL);
CREATE TABLE m0_receipts(id TEXT PRIMARY KEY,app_id TEXT NOT NULL,message_id TEXT NOT NULL,thread_id TEXT NOT NULL,chat_id TEXT NOT NULL,body TEXT NOT NULL,client_id TEXT NOT NULL UNIQUE,queue_id TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,card_id TEXT NOT NULL DEFAULT '',action_token TEXT NOT NULL UNIQUE,UNIQUE(app_id,message_id));
CREATE TABLE m0_actions(receipt_id TEXT PRIMARY KEY,kind TEXT NOT NULL,state TEXT NOT NULL);
CREATE TABLE m0_reactions(receipt_id TEXT PRIMARY KEY,emoji TEXT NOT NULL DEFAULT '',reaction_id TEXT NOT NULL DEFAULT '',revision INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL DEFAULT 'confirmed');
INSERT INTO m0_bindings VALUES('thread','chat');
INSERT INTO m0_receipts VALUES('receipt','app','original','thread','chat','private body must stay private','client','queue','steered','queue-card','token');
INSERT INTO m0_actions VALUES('receipt','steer','done');
INSERT INTO m0_reactions VALUES('receipt','CheckMark','reaction',3,'confirmed');
INSERT INTO m0_operations VALUES('m0.mirror.thread.human.0','feishu.mirror','confirmed','human-card',1);
INSERT INTO m0_operations VALUES('m0.progress.thread.turn','feishu.progress','confirmed','progress-card',1);
INSERT INTO m0_operations VALUES('m0.final.thread.turn.0','feishu.final','confirmed','final-card',1);
INSERT INTO m0_operations VALUES('m0.receipt-card.receipt','feishu.send_card','confirmed','queue-card',1);
INSERT INTO m0_operations VALUES('m0.steer.receipt','codex.turn.steer','confirmed','turn',1);
PRAGMA user_version=1;
PRAGMA foreign_keys=ON;
`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		db.Close()
		t.Fatal(err)
	}
	s := &Store{DB: db}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func legacyPlan(t *testing.T, s *Store) LegacyAdoption {
	t.Helper()
	i, err := s.LegacyInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := func(key, kind, id string) Mirror {
		return Mirror{Key: key, Kind: kind, TurnID: "turn", Card: `{"body":"replacement"}`, Revision: 1, State: "ready", Terminal: true, Replay: true, RemoteID: id}
	}
	human := m("human:"+strings.Repeat("a", 64), "human", "human-card")
	progress := m("progress:"+strings.Repeat("b", 64), "progress", "progress-card")
	progress.ParentKey = human.Key
	final := m("final:"+strings.Repeat("c", 64), "final", "final-card")
	final.ParentKey, final.EventType, final.EventAt = human.Key, "completed", 2
	queue := m(QueueCardKey("receipt"), "progress", "queue-card")
	queue.TurnID, queue.ParentMessageID = queue.Key, "original"
	return LegacyAdoption{
		HostID: "host", AppID: "app", OwnerID: "owner", SourceDigest: i.Digest(), NativeDigest: strings.Repeat("a", 64), StartedAtMS: 10000,
		Threads: []LegacyThreadSeed{{ThreadID: "thread", ChatID: "chat", CWD: "/project", ProjectRoot: "/project", FirstUserItemID: "human", CreatedAt: 1,
			Mirrors: []Mirror{human, progress, final, queue}, Links: []InputLink{{ReceiptID: "receipt", TurnID: "turn", ItemID: "steer-input"}}}},
	}
}

func assertLegacyUnchanged(t *testing.T, s *Store, digest string) {
	t.Helper()
	i, err := s.LegacyInventory(context.Background())
	if err != nil || i.Schema != 1 || i.Digest() != digest {
		t.Fatal("legacy state changed", i.Schema, err)
	}
	var count int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('bindings','mirrors','legacy_adoptions')`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial migration committed", count, err)
	}
}

func TestLegacyOpenRefusesBeforeChangingSchemaPermissionsOrJournalMode(t *testing.T) {
	s, path := legacyStore(t)
	s.Close()
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(path); !errors.Is(err, ErrLegacyAdoptionRequired) {
		if opened != nil {
			opened.Close()
		}
		t.Fatal("legacy opener did not require reviewed adoption", err)
	}
	after, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || string(before) != string(after) || info.Mode().Perm() != 0640 {
		t.Fatal("guard changed legacy database", err, statErr)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err = os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatal("guard created write sidecar", suffix, err)
		}
	}
}

func TestLegacyOpenGuardReadsLiveWALWithoutChangingDatabaseState(t *testing.T) {
	s, path := legacyStore(t)
	if _, err := s.DB.Exec(`PRAGMA journal_mode=WAL; UPDATE m0_receipts SET body='committed in live WAL';`); err != nil {
		t.Fatal(err)
	}
	before, err := s.LegacyInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	walBefore, err := os.ReadFile(path + "-wal")
	if err != nil || len(walBefore) == 0 {
		t.Fatal("live WAL fixture absent", err)
	}
	if opened, err := Open(path); !errors.Is(err, ErrLegacyAdoptionRequired) {
		if opened != nil {
			opened.Close()
		}
		t.Fatal("guard failed in WAL mode", err)
	}
	after, err := s.LegacyInventory(context.Background())
	walAfter, walErr := os.ReadFile(path + "-wal")
	var mode string
	modeErr := s.DB.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	info, statErr := os.Stat(path)
	if err != nil || walErr != nil || modeErr != nil || statErr != nil || before.Digest() != after.Digest() || string(walBefore) != string(walAfter) || mode != "wal" || info.Mode().Perm() != 0600 {
		t.Fatal("guard changed WAL, data, schema, journal mode or permissions", err, walErr, modeErr, statErr)
	}
	// No assertion about the WAL reader index's file set: SQLite can create
	// reader sidecars on a clean WAL database, even for a mode=ro connection.
}

func TestLegacyAdoptionImportsExistingRoutesAndOnlyClaimsUpdates(t *testing.T) {
	ctx := context.Background()
	s, path := legacyStore(t)
	before, err := s.LegacyInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := legacyPlan(t, s)
	if changed, err := s.AdoptLegacy(ctx, p); err != nil || !changed {
		t.Fatal("adoption failed", changed, err)
	}
	after, err := s.LegacyInventory(ctx)
	after.Schema = 1 // The schema changes; old private journals must not.
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("adoption rewrote original journal", err)
	}
	b, err := s.Binding(ctx, "host", "thread")
	if err != nil || b.ChatID != "chat" {
		t.Fatal("route not reused", err)
	}
	if claimed, err := s.ClaimGroupDispatch(ctx, b.IntentID); err != nil || claimed {
		t.Fatal("adopted route could create another group", err)
	}
	if err = s.CompleteBaseline(ctx, "host", []string{"thread", "old-unbound"}); err != nil {
		t.Fatal(err)
	}
	obs, err := s.Observation(ctx, "host", "thread")
	other, otherErr := s.Observation(ctx, "host", "old-unbound")
	if err != nil || otherErr != nil || obs.State != "eligible" || other.State != "ignored" {
		t.Fatal("adoption changed initial discovery semantics", err, otherErr)
	}
	inputs, err := s.Submissions(ctx, SubmissionScope{"host", "app", "thread"})
	if err != nil || len(inputs) != 1 || inputs[0].State != "steered" || inputs[0].ID != "receipt" || inputs[0].ClientID != "client" || inputs[0].TurnID != "turn" || inputs[0].ItemID != "steer-input" {
		t.Fatal("native input identities lost", err)
	}
	for _, m := range p.Threads[0].Mirrors {
		a, claimed, err := s.ClaimMirror(ctx, "host", "thread", m.Key, 1, 10001)
		if err != nil || !claimed || a.Operation != "update" || a.TargetMessageID != m.RemoteID || a.ChatID != "chat" {
			t.Fatal("historical card could be sent again", a.Operation, err)
		}
		if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", m.RemoteID); err != nil {
			t.Fatal(err)
		}
	}
	// Later normal activity must not make exact committed-plan replay destructive.
	if _, err := s.DB.Exec(`UPDATE m0_receipts SET body='new private value' WHERE id='receipt'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if changed, err := reopened.AdoptLegacy(ctx, p); err != nil || changed {
		t.Fatal("committed plan not idempotent after reopen", changed, err)
	}
	ready, err := reopened.ReadyMirrors(ctx, "host", "thread")
	if err != nil || len(ready) != 0 {
		t.Fatal("replay reopened delivered cards", err)
	}
	p.OwnerID = "other-owner"
	if changed, err := reopened.AdoptLegacy(ctx, p); !errors.Is(err, ErrConflict) || changed {
		t.Fatal("different plan accepted", changed, err)
	}
}

func TestLegacyAdoptionRejectsStaleUncertainAndUnsafeSeedsAtomically(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Store, *LegacyAdoption)
		want error
	}{
		{"stale body", func(s *Store, p *LegacyAdoption) { s.DB.Exec(`UPDATE m0_receipts SET body='changed'`) }, ErrConflict},
		{"unknown despite done action", func(s *Store, p *LegacyAdoption) {
			s.DB.Exec(`UPDATE m0_operations SET state='unknown' WHERE kind='codex.turn.steer'`)
			i, _ := s.LegacyInventory(context.Background())
			p.SourceDigest = i.Digest()
		}, ErrLegacyPending},
		{"native link missing", func(_ *Store, p *LegacyAdoption) { p.Threads[0].Links = nil }, ErrLegacyPending},
		{"remote ID fabricated", func(_ *Store, p *LegacyAdoption) { p.Threads[0].Mirrors[0].RemoteID = "not-known" }, ErrConflict},
		{"remote ID reused", func(_ *Store, p *LegacyAdoption) { p.Threads[0].Mirrors[1].RemoteID = "human-card" }, ErrConflict},
		{"reply parent fabricated", func(_ *Store, p *LegacyAdoption) { p.Threads[0].Mirrors[1].ParentKey = "not-known" }, ErrConflict},
		{"binding omitted", func(_ *Store, p *LegacyAdoption) { p.Threads = nil }, ErrConflict},
		{"mirrors omitted", func(_ *Store, p *LegacyAdoption) { p.Threads[0].Mirrors = nil }, ErrConflict},
		{"malformed canonical key", func(_ *Store, p *LegacyAdoption) { p.Threads[0].Mirrors[2].Key = "final:fake" }, ErrConflict},
		{"receipt card recalled", func(s *Store, p *LegacyAdoption) {
			s.DB.Exec(`UPDATE m0_receipts SET card_id=''`)
			i, _ := s.LegacyInventory(context.Background())
			p.SourceDigest = i.Digest()
		}, ErrConflict},
		{"old mirror retired", func(s *Store, p *LegacyAdoption) {
			s.DB.Exec(`UPDATE m0_operations SET state='retired' WHERE remote_id='human-card'`)
			i, _ := s.LegacyInventory(context.Background())
			p.SourceDigest = i.Digest()
		}, ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := legacyStore(t)
			p := legacyPlan(t, s)
			tc.edit(s, &p)
			before, err := s.LegacyInventory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := s.AdoptLegacy(context.Background(), p); changed || !errors.Is(err, tc.want) {
				t.Fatal("unsafe adoption accepted", changed, err)
			}
			assertLegacyUnchanged(t, s, before.Digest())
		})
	}
}

func TestLegacyAdoptionRollsBackDDLOnLateSQLFailure(t *testing.T) {
	s, _ := legacyStore(t)
	p := legacyPlan(t, s)
	// A damaged conflicting formal table makes the final input import fail,
	// after all DDL, bindings and cards have been written in the transaction.
	if _, err := s.DB.Exec(`CREATE TABLE submission_inputs(
receipt_id TEXT PRIMARY KEY CHECK(1=0),host_id TEXT NOT NULL,app_id TEXT NOT NULL,thread_id TEXT NOT NULL,
state TEXT NOT NULL,queue_id TEXT NOT NULL DEFAULT '',queue_ordinal INTEGER NOT NULL DEFAULT 0,
revision INTEGER NOT NULL DEFAULT 1,turn_id TEXT NOT NULL DEFAULT '',item_id TEXT NOT NULL DEFAULT '');`); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.AdoptLegacy(context.Background(), p); changed || err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("failed transaction not safely rejected", changed, err)
	}
	assertLegacyUnchanged(t, s, p.SourceDigest)
}

func TestLegacyDiagnosticsOmitPrivateBodiesAndDigestBindsThem(t *testing.T) {
	s, _ := legacyStore(t)
	i, err := s.LegacyInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := legacyPlan(t, s)
	for _, value := range []any{i, p} {
		b, err := json.Marshal(value)
		if err != nil || string(b) != "{}" {
			t.Fatal("raw migration plan could expose conversation data", err)
		}
	}
	digest := i.Digest()
	i.Receipts[0].Body = "changed"
	if digest == i.Digest() || !legacyDigest(i.Digest()) || !legacyDigest(p.ID()) {
		t.Fatal("private body not bound to plan digest")
	}
	id := p.ID()
	p.Threads[0].Mirrors[0].Card = `{"body":"changed"}`
	if id == p.ID() {
		t.Fatal("prepared render not bound to adoption ID")
	}
}

func TestLegacyQuiescenceRequiresTerminalAndConsistentInventory(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*LegacyInventory)
		want error
	}{
		{"queued input", func(i *LegacyInventory) { i.Receipts[0].State = "queued" }, ErrLegacyPending},
		{"received input", func(i *LegacyInventory) { i.Receipts[0].State = "received" }, ErrLegacyPending},
		{"unknown operation", func(i *LegacyInventory) { i.Operations[0].State = "unknown" }, ErrLegacyPending},
		{"sending operation", func(i *LegacyInventory) { i.Operations[0].State = "sending" }, ErrLegacyPending},
		{"processing action", func(i *LegacyInventory) { i.Actions[0].State = "processing" }, ErrLegacyPending},
		{"changing reaction", func(i *LegacyInventory) { i.Reactions[0].State = "changing" }, ErrLegacyPending},
		{"unknown reaction", func(i *LegacyInventory) { i.Reactions[0].State = "unknown" }, ErrLegacyPending},
		{"other app", func(i *LegacyInventory) { i.Receipts[0].AppID = "other" }, ErrConflict},
		{"missing route", func(i *LegacyInventory) { i.Bindings = nil }, ErrConflict},
		{"route mismatch", func(i *LegacyInventory) { i.Bindings[0].ChatID = "other" }, ErrConflict},
		{"duplicate binding", func(i *LegacyInventory) { i.Bindings = append(i.Bindings, i.Bindings[0]) }, ErrConflict},
		{"duplicate receipt", func(i *LegacyInventory) { i.Receipts = append(i.Receipts, i.Receipts[0]) }, ErrConflict},
		{"duplicate operation", func(i *LegacyInventory) { i.Operations = append(i.Operations, i.Operations[0]) }, ErrConflict},
		{"orphan action", func(i *LegacyInventory) { i.Actions[0].ReceiptID = "not-known" }, ErrConflict},
		{"duplicate action", func(i *LegacyInventory) { i.Actions = append(i.Actions, i.Actions[0]) }, ErrConflict},
		{"orphan reaction", func(i *LegacyInventory) { i.Reactions[0].ReceiptID = "not-known" }, ErrConflict},
		{"duplicate reaction", func(i *LegacyInventory) { i.Reactions = append(i.Reactions, i.Reactions[0]) }, ErrConflict},
		{"reaction ID missing", func(i *LegacyInventory) { i.Reactions[0].ReactionID = "" }, ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := legacyStore(t)
			i, err := s.LegacyInventory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := i.Quiescent("app"); err != nil {
				t.Fatal("valid fixture rejected", err)
			}
			tc.edit(&i)
			if err := i.Quiescent("app"); !errors.Is(err, tc.want) {
				t.Fatal("unverified inventory accepted", err)
			}
		})
	}
}
