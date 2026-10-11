package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

type runtimeNative struct {
	mu                       sync.Mutex
	threads                  map[string]codexclient.Thread
	turns                    map[string][]codexclient.Turn
	events                   chan codexclient.Event
	listStarted, releaseList chan struct{}
	listOnce                 sync.Once
}

func (n *runtimeNative) Events() <-chan codexclient.Event { return n.events }
func (n *runtimeNative) Err() error                       { return errors.New("test disconnect") }
func (n *runtimeNative) ListArchive(ctx context.Context, archived bool) ([]codexclient.Thread, error) {
	if archived {
		return nil, nil
	}
	if n.listStarted != nil {
		n.listOnce.Do(func() { close(n.listStarted) })
		select {
		case <-n.releaseList:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var result []codexclient.Thread
	for _, t := range n.threads {
		result = append(result, t)
	}
	return result, nil
}
func (n *runtimeNative) LoadedIDs(context.Context) ([]string, error) { return nil, nil }
func (n *runtimeNative) Metadata(_ context.Context, id string) (codexclient.Thread, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if t, ok := n.threads[id]; ok {
		return t, nil
	}
	return codexclient.Thread{}, sql.ErrNoRows
}
func (n *runtimeNative) Attach(ctx context.Context, id string) (codexclient.Thread, error) {
	return n.Metadata(ctx, id)
}
func (n *runtimeNative) Turns(_ context.Context, id string) ([]codexclient.Turn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]codexclient.Turn(nil), n.turns[id]...), nil
}
func (n *runtimeNative) set(id, root, status string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	old := time.Now().Add(-time.Hour).Unix()
	t := codexclient.Thread{ID: id, CWD: root, Source: json.RawMessage(`"cli"`), CreatedAt: time.Now().Unix()}
	n.threads[id] = t
	text, _ := json.Marshal(map[string]string{"type": "text", "text": "public input"})
	turn := codexclient.Turn{ID: "turn-" + id, Status: status, ItemsView: "full", StartedAt: &old, Items: []codexclient.Item{{ID: "user-" + id, Type: "userMessage", Content: []json.RawMessage{text}}}}
	if status == "completed" {
		now := time.Now().Unix()
		phase := "final_answer"
		turn.CompletedAt = &now
		turn.Items = append(turn.Items, codexclient.Item{Type: "agentMessage", ID: "answer-" + id, Text: "public result", Phase: &phase})
	}
	n.turns[id] = []codexclient.Turn{turn}
}

type runtimeWrite struct{ op, target, parent, card string }
type runtimeGateway struct {
	mu                           sync.Mutex
	groups                       int
	writes                       []runtimeWrite
	createStarted, releaseCreate chan struct{}
}

func (g *runtimeGateway) CreateGroup(ctx context.Context, _, owner string, members []string, _ string) (string, error) {
	ownerIncluded := false
	for _, id := range members {
		if id == owner {
			ownerIncluded = true
		}
	}
	if !ownerIncluded {
		return "", errors.New("owner must be included explicitly")
	}
	g.mu.Lock()
	g.groups++
	g.mu.Unlock()
	if g.createStarted != nil {
		select {
		case g.createStarted <- struct{}{}:
		default:
		}
	}
	if g.releaseCreate != nil {
		select {
		case <-g.releaseCreate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "new-chat", nil
}
func (g *runtimeGateway) add(op, target, parent, card string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writes = append(g.writes, runtimeWrite{op, target, parent, card})
	return fmt.Sprintf("remote-%d", len(g.writes)), nil
}
func (g *runtimeGateway) SendCard(_ context.Context, _, target, card, _ string) (string, error) {
	return g.add("send", target, "", card)
}
func (g *runtimeGateway) ReplyCard(_ context.Context, parent, card, _ string) (string, error) {
	return g.add("reply", "", parent, card)
}
func (g *runtimeGateway) UpdateCard(_ context.Context, target, card string) error {
	_, err := g.add("update", target, "", card)
	return err
}
func runtimeWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("runtime condition not reached")
}
func runtimeFixture(t *testing.T) (*store.Store, *runtimeNative, *runtimeGateway, *Runtime) {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	n := &runtimeNative{threads: map[string]codexclient.Thread{}, turns: map[string][]codexclient.Turn{}, events: make(chan codexclient.Event, 512)}
	g := &runtimeGateway{}
	r := &Runtime{Store: db, Native: n, Groups: g, Messages: g, HostID: "host", AppID: "app", OwnerID: "owner", AllowedRoots: []string{root}, DiscoveryEnabled: true, ScanInterval: 20 * time.Millisecond, PollInterval: 10 * time.Millisecond}
	return db, n, g, r
}
func TestRuntimePersistsBirthDuringSnapshotAndFinalDuringSlowGroup(t *testing.T) {
	db, n, g, r := runtimeFixture(t)
	n.set("old", r.AllowedRoots[0], "completed")
	n.listStarted = make(chan struct{})
	n.releaseList = make(chan struct{})
	g.createStarted = make(chan struct{}, 1)
	g.releaseCreate = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case <-n.listStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not start")
	}
	n.set("new", r.AllowedRoots[0], "inProgress")
	tmeta, _ := n.Metadata(ctx, "new")
	b, _ := json.Marshal(map[string]any{"thread": tmeta})
	n.events <- codexclient.Event{Method: "thread/started", Params: b}
	runtimeWait(t, func() bool { o, err := db.Observation(ctx, "host", "new"); return err == nil && o.Origin == "new" })
	close(n.releaseList)
	select {
	case <-g.createStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("group did not start")
	}
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
		t.Fatalf("binding preceded remote confirmation: %v", err)
	}
	g.mu.Lock()
	writes := len(g.writes)
	g.mu.Unlock()
	if writes != 0 {
		t.Fatal("content sent before group existed")
	}
	close(g.releaseCreate)
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
	o, err := db.Observation(ctx, "host", "old")
	if err != nil || o.State != "ignored" {
		t.Fatalf("old history activated: %+v %v", o, err)
	}
	g.mu.Lock()
	if g.groups != 1 {
		t.Errorf("groups=%d", g.groups)
	}
	for _, w := range g.writes {
		if w.op == "reply" && w.parent != "remote-1" {
			t.Errorf("lost actual human reply relation: %s", w.parent)
		}
	}
	g.mu.Unlock()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("workers were not joined")
	}
}

func runtimeBind(t *testing.T, s *store.Store, r *Runtime, id, chat string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.BeginBaseline(ctx, r.HostID, r.AppID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteBaseline(ctx, r.HostID, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveNew(ctx, r.HostID, id, r.AllowedRoots[0], time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEligible(ctx, r.HostID, id, "user-"+id, r.AllowedRoots[0], r.AllowedRoots[0]); err != nil {
		t.Fatal(err)
	}
	i, err := s.PrepareGroup(ctx, r.HostID, id, "owner", "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimGroupDispatch(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmGroup(ctx, i.ID, chat); err != nil {
		t.Fatal(err)
	}
	if err = s.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeRoutesMultipleThreadsCoalescesAndJoinsOnDisconnect(t *testing.T) {
	db, n, g, r := runtimeFixture(t)
	r.DiscoveryEnabled = false
	for _, id := range []string{"one", "two"} {
		n.set(id, r.AllowedRoots[0], "completed")
		runtimeBind(t, db, r, id, "chat-"+id)
	}
	var mu sync.Mutex
	active := map[string]bool{}
	counts := map[string]int{}
	overlap := false
	r.ActorTick = func(ctx context.Context, id string) error {
		mu.Lock()
		if active[id] {
			overlap = true
		}
		active[id] = true
		counts[id]++
		mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(time.Millisecond):
		}
		mu.Lock()
		active[id] = false
		mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	runtimeWait(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.writes) == 4 })
	for i := 0; i < 200; i++ {
		id := "one"
		if i%2 == 0 {
			id = "two"
		}
		b, _ := json.Marshal(map[string]string{"threadId": id})
		n.events <- codexclient.Event{Method: "item/agentMessage/delta", Params: b}
	}
	runtimeWait(t, func() bool { mu.Lock(); defer mu.Unlock(); return counts["one"] >= 2 && counts["two"] >= 2 })
	close(n.events)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("disconnect was hidden")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not join workers")
	}
	mu.Lock()
	if overlap || counts["one"] > 15 || counts["two"] > 15 {
		t.Errorf("event burst was not serialized/coalesced: %v %#v", overlap, counts)
	}
	before := counts["one"] + counts["two"]
	mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	if before != counts["one"]+counts["two"] {
		t.Error("native work continued after generation return")
	}
	mu.Unlock()
	for _, id := range []string{"one", "two"} {
		b, err := db.BindingByChat(ctx, "host", "chat-"+id)
		if err != nil || b.ThreadID != id {
			t.Fatalf("bad chat route: %+v %v", b, err)
		}
	}
	if _, err := db.BindingByChat(ctx, "different-host", "chat-one"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cross-host route allowed")
	}
}

func TestRuntimeMuteOnlyAffectsFutureGroupsAndUnmuteDoesNotBackfill(t *testing.T) {
	db, n, g, r := runtimeFixture(t)
	n.set("bound", r.AllowedRoots[0], "completed")
	runtimeBind(t, db, r, "bound", "existing-chat")
	command := rules.DirectoryCommand{Action: rules.DirectoryMute, Rule: rules.DirectoryRule{Path: r.AllowedRoots[0], Recursive: true}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := db.ApplyDirectoryCommand(ctx, r.HostID, r.AppID, command); err != nil {
		t.Fatal(err)
	}
	r.DirectoryRules = func(ctx context.Context) ([]rules.DirectoryRule, error) {
		return db.DirectoryRules(ctx, r.HostID, r.AppID)
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	runtimeWait(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.writes) == 2 })
	n.set("muted", r.AllowedRoots[0], "completed")
	meta, _ := n.Metadata(ctx, "muted")
	params, _ := json.Marshal(map[string]any{"thread": meta})
	n.events <- codexclient.Event{Method: "thread/started", Params: params}
	runtimeWait(t, func() bool {
		o, err := db.Observation(ctx, r.HostID, "muted")
		return err == nil && o.State == "excluded" && o.ExclusionReason == "directory_muted"
	})
	command.Action = rules.DirectoryUnmute
	if err := db.ApplyDirectoryCommand(ctx, r.HostID, r.AppID, command); err != nil {
		t.Fatal(err)
	}
	r.Wake("muted")
	time.Sleep(60 * time.Millisecond)
	g.mu.Lock()
	if g.groups != 0 {
		t.Error("unmute backfilled a muted historical candidate")
	}
	g.mu.Unlock()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not stop")
	}
}
