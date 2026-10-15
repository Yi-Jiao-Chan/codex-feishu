package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func input(id, text string) codexclient.Item {
	body, _ := json.Marshal(map[string]any{"type": "text", "text": text})
	return codexclient.Item{ID: id, Type: "userMessage", Content: []json.RawMessage{body}}
}
func agent(id, text, phase string) codexclient.Item {
	i := codexclient.Item{ID: id, Type: "agentMessage", Text: text}
	if phase != "" {
		i.Phase = &phase
	}
	return i
}

func engineFixture(t *testing.T) (Engine, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sync.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.BeginBaseline(ctx, "host", "app", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveNew(ctx, "host", "thread", "/project", 101); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEligible(ctx, "host", "thread", "user", "/project", "/project"); err != nil {
		t.Fatal(err)
	}
	return Engine{Store: s, HostID: "host"}, path
}

func bindEngine(t *testing.T, e Engine) {
	t.Helper()
	ctx := context.Background()
	i, err := e.Store.PrepareGroup(ctx, "host", "thread", "owner", "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.ClaimGroupDispatch(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.ConfirmGroup(ctx, i.ID, "chat"); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
}

type cardWrite struct{ Operation, ID, Parent, Card string }
type cardGateway struct {
	writes []cardWrite
	cards  map[string]string
}

func (g *cardGateway) created(operation, parent, card string) (string, error) {
	if g.cards == nil {
		g.cards = map[string]string{}
	}
	id := fmt.Sprintf("message-%d", len(g.writes)+1)
	g.cards[id] = card
	g.writes = append(g.writes, cardWrite{operation, id, parent, card})
	return id, nil
}
func (g *cardGateway) SendCard(_ context.Context, _, _, card, _ string) (string, error) {
	return g.created("send", "", card)
}
func (g *cardGateway) ReplyCard(_ context.Context, parent, card, _ string) (string, error) {
	return g.created("reply", parent, card)
}
func (g *cardGateway) UpdateCard(_ context.Context, id, card string) error {
	if _, exists := g.cards[id]; !exists {
		return errors.New("card does not exist")
	}
	g.cards[id] = card
	g.writes = append(g.writes, cardWrite{"update", id, "", card})
	return nil
}

func drain(t *testing.T, e Engine, g *cardGateway, at int64) {
	t.Helper()
	o := Outbound{Store: e.Store, Gateway: g}
	for i := 0; i < 1000; i++ {
		r, err := o.DispatchOne(context.Background(), "host", "thread", at)
		if err != nil || r.AttemptID != "" && r.State != "confirmed" {
			t.Fatalf("delivery failed: %+v %v", r, err)
		}
		if r.AttemptID == "" {
			return
		}
	}
	t.Fatal("outbound did not settle")
}

func apply(t *testing.T, e Engine, at int64, replay bool, turns ...codexclient.Turn) {
	t.Helper()
	if err := e.Apply(context.Background(), "thread", turns, replay, at); err != nil {
		t.Fatal(err)
	}
}

func TestSyncPublicHistoryCreatesOneProcessAndOneFinalWithParent(t *testing.T) {
	ctx := context.Background()
	e, path := engineFixture(t)
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{
		input("user", "request"), agent("progress", "starting", "commentary"),
		{ID: "reason", Type: "reasoning", Text: "hidden-reasoning"},
		{ID: "tool", Type: "commandExecution", Status: "inProgress", Text: "secret-tool-output"},
	}}
	apply(t, e, 200, false, turn)
	mirrors, err := e.Store.Mirrors(ctx, "host", "thread")
	if err != nil || len(mirrors) != 2 {
		t.Fatalf("history not staged before group: %d %v", len(mirrors), err)
	}
	for _, m := range mirrors {
		if strings.Contains(m.Card, "hidden-reasoning") || strings.Contains(m.Card, "secret-tool-output") || strings.Contains(m.Card, "Codex App") {
			t.Fatal("private output or unproven source leaked")
		}
	}
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 1000)
	if len(g.writes) != 2 || g.writes[0].Operation != "send" || g.writes[1].Operation != "reply" || g.writes[1].Parent != g.writes[0].ID {
		t.Fatalf("missing group reply relation: %+v", g.writes)
	}
	turn.Items = append(turn.Items, agent("progress2", "next public action", "commentary"))
	apply(t, e, 201, false, turn)
	drain(t, e, g, 2999)
	if len(g.writes) != 2 {
		t.Fatal("progress updated inside two-second interval")
	}
	drain(t, e, g, 3000)
	if len(g.writes) != 3 || g.writes[2].Operation != "update" || g.writes[2].ID != g.writes[1].ID {
		t.Fatal("progress created another message")
	}
	turn.Status = "completed"
	turn.Items = append(turn.Items, agent("final1", "final part one", "final_answer"), agent("final2", "final part two", "final_answer"), agent("last-comment", "not a final answer", "commentary"))
	apply(t, e, 202, false, turn)
	if done, err := e.FinalDelivered(ctx, "thread", "turn"); err != nil || done {
		t.Fatalf("native completion mistaken for delivery: %v %v", done, err)
	}
	drain(t, e, g, 3001)
	if len(g.writes) != 5 || g.writes[3].Operation != "update" || g.writes[4].Operation != "reply" || g.writes[4].Parent != g.writes[0].ID {
		t.Fatalf("final did not remain independent: %+v", g.writes)
	}
	for _, record := range []string{"starting", "next public action", "not a final answer"} {
		if !strings.Contains(g.writes[3].Card, record) {
			t.Fatal("terminal progress discarded a public process record")
		}
	}
	if !strings.Contains(g.writes[3].Card, `"tag":"collapsible_panel"`) || strings.Contains(g.writes[3].Card, "final part one") || strings.Contains(g.writes[3].Card, "hidden-reasoning") || strings.Contains(g.writes[3].Card, "secret-tool-output") {
		t.Fatal("terminal process panel included final/private content or was absent")
	}
	if !strings.Contains(g.writes[4].Card, "final part one") || !strings.Contains(g.writes[4].Card, "final part two") || strings.Contains(g.writes[4].Card, "not a final answer") {
		t.Fatal("native final phase was not respected")
	}
	if done, err := e.FinalDelivered(ctx, "thread", "turn"); err != nil || !done {
		t.Fatalf("confirmed final not recognized: %v %v", done, err)
	}
	retainedProgress, err := e.Store.Mirror(ctx, "host", "thread", mirrorKey("progress", "turn"))
	if err != nil {
		t.Fatal(err)
	}
	e.Store.Close()
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	e.Store = reopened
	apply(t, e, 203, true, turn)
	drain(t, e, g, 9000)
	if len(g.writes) != 5 {
		t.Fatal("reconnect reposted old context")
	}
	mirrors, _ = e.Store.Mirrors(ctx, "host", "thread")
	for _, m := range mirrors {
		if m.Replay {
			t.Fatal("reconnect replaced original live event provenance")
		}
		if m.Kind == "progress" && (m.Card != retainedProgress.Card || m.RemoteID != g.writes[1].ID) {
			t.Fatal("restart changed retained process history or its real message")
		}
	}
}

func TestFeishuHistoryLinksPreventEchoAndSteerDoesNotMoveAnchor(t *testing.T) {
	ctx := context.Background()
	e, _ := engineFixture(t)
	r, fresh, err := e.Store.Claim(ctx, store.Receipt{AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: "feishu-first", Body: "bridge-input"})
	if err != nil || !fresh {
		t.Fatal(err)
	}
	u := input("user", "bridge-input")
	u.ClientID = &r.ClientID
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", Items: []codexclient.Item{u, agent("commentary", "working", "commentary")}}
	apply(t, e, 200, false, turn)
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 1000)
	if len(g.writes) != 1 || g.writes[0].Operation != "reply" || g.writes[0].Parent != "feishu-first" {
		t.Fatalf("Feishu human body echoed or wrong anchor: %+v", g.writes)
	}
	r2, _, err := e.Store.Claim(ctx, store.Receipt{AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: "feishu-supplement", Body: "supplement-input"})
	if err != nil {
		t.Fatal(err)
	}
	u2 := input("steer", "supplement-input")
	u2.ClientID = &r2.ClientID
	turn.Items = append(turn.Items, u2, agent("answer", "final result", "final_answer"))
	turn.Status = "completed"
	apply(t, e, 201, false, turn)
	drain(t, e, g, 1001)
	if len(g.writes) != 3 || g.writes[2].Operation != "reply" || g.writes[2].Parent != "feishu-first" {
		t.Fatal("steer moved initiating reply anchor")
	}
	for _, write := range g.writes {
		if strings.Contains(write.Card, "bridge-input") || strings.Contains(write.Card, "supplement-input") {
			t.Fatal("bridge input body was echoed")
		}
	}
	links, err := e.Store.InputLinks(ctx, "host", "thread")
	if err != nil || len(links) != 2 {
		t.Fatalf("exact receipt history links missing: %+v %v", links, err)
	}
	apply(t, e, 202, true, turn)
	drain(t, e, g, 6000)
	if len(g.writes) != 3 {
		t.Fatal("receipt/link replay duplicated messages")
	}
}

func TestCompletedBackfillDoesNotInventProcessAndLegacyIDsAreTurnScoped(t *testing.T) {
	e, _ := engineFixture(t)
	var turns []codexclient.Turn
	for n := 0; n < 80; n++ {
		turns = append(turns, codexclient.Turn{ID: fmt.Sprintf("turn-%d", n), Status: "completed", Items: []codexclient.Item{
			input("legacy-item-0", fmt.Sprintf("question-%d", n)),
			agent("legacy-comment", "historical public process", "commentary"),
			agent("legacy-item-1", fmt.Sprintf("answer-%d", n), ""),
		}})
	}
	apply(t, e, 200, true, turns...)
	mirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil || len(mirrors) != 160 {
		t.Fatalf("multi-turn/legacy IDs lost content: %d %v", len(mirrors), err)
	}
	for _, m := range mirrors {
		if m.Kind == "progress" || !m.Replay {
			t.Fatal("historical turn acquired fresh process/live provenance")
		}
	}
}

func TestFinalPhaseAndEmptyTerminalOutcomes(t *testing.T) {
	for _, tc := range []struct {
		status, expect string
		items          []codexclient.Item
	}{
		{"completed", "latest legacy", []codexclient.Item{agent("old", "old legacy", ""), agent("latest", "latest legacy", ""), agent("comment", "commentary-only", "commentary")}},
		{"completed", "未返回公开回复", []codexclient.Item{agent("comment", "commentary-only", "commentary")}},
		{"failed", "执行失败", nil},
		{"interrupted", "已停止", nil},
	} {
		t.Run(tc.status+tc.expect, func(t *testing.T) {
			e, _ := engineFixture(t)
			turn := codexclient.Turn{ID: "turn", Status: tc.status, Items: append([]codexclient.Item{input("user", "question")}, tc.items...)}
			apply(t, e, 200, false, turn)
			m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
			if err != nil || !strings.Contains(m.Card, tc.expect) || strings.Contains(m.Card, "commentary-only") {
				t.Fatalf("bad terminal outcome: %s %v", m.Card, err)
			}
		})
	}
}

func TestLongFinalCorrectionSuppressesUnsentObsoleteSegments(t *testing.T) {
	ctx := context.Background()
	e, _ := engineFixture(t)
	e.MaxCardBytes = 850
	turn := codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{input("user", "question"), agent("answer", "```go\n"+strings.Repeat("fmt.Println(\"中文😀\")\n", 100)+"```", "final_answer")}}
	apply(t, e, 200, false, turn)
	before, _ := e.Store.Mirrors(ctx, "host", "thread")
	if len(before) < 4 {
		t.Fatal("fixture did not require long-code segmentation")
	}
	turn.Items[1].Text = "short corrected answer"
	apply(t, e, 201, true, turn)
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 1000)
	if len(g.writes) != 2 || !strings.Contains(g.writes[1].Card, "short corrected answer") {
		t.Fatalf("obsolete unsent segments generated new cards: %d", len(g.writes))
	}
	if done, err := e.FinalDelivered(ctx, "thread", "turn"); err != nil || !done {
		t.Fatalf("suppressed tails block completion: %v %v", done, err)
	}
}

func TestLongFinalCorrectionUpdatesExistingSegmentsWithoutReposting(t *testing.T) {
	ctx := context.Background()
	e, _ := engineFixture(t)
	e.MaxCardBytes = 850
	turn := codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{input("user", "question"), agent("answer", "```go\n"+strings.Repeat("long-code-marker\n", 200)+"```", "final_answer")}}
	apply(t, e, 200, false, turn)
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 1000)
	oldCount := len(g.writes)
	for _, w := range g.writes {
		if len(w.Card) > 850 || !json.Valid([]byte(w.Card)) {
			t.Fatal("serialized card size bound violated")
		}
	}
	turn.Items[1].Text = "short corrected answer"
	apply(t, e, 201, true, turn)
	if done, _ := e.FinalDelivered(ctx, "thread", "turn"); done {
		t.Fatal("obsolete delivered tails ignored before replacement PATCH")
	}
	drain(t, e, g, 2000)
	for _, w := range g.writes[oldCount:] {
		if w.Operation != "update" {
			t.Fatal("correction sent new cards")
		}
	}
	for _, card := range g.cards {
		if strings.Contains(card, "long-code-marker") {
			t.Fatal("obsolete code remained in an old segment")
		}
	}
	if done, err := e.FinalDelivered(ctx, "thread", "turn"); err != nil || !done {
		t.Fatalf("corrected result not delivered: %v %v", done, err)
	}
}

func TestProgressHasActualByteBoundAndLateBusyDoesNotRegressCompletion(t *testing.T) {
	ctx := context.Background()
	e, _ := engineFixture(t)
	// This deliberately small budget must still hold the new required omission
	// disclosure, status and a visibly truncated summary, plus the wire marker.
	e.MaxCardBytes = len(cards.Task(cards.TaskView{Status: "inProgress", Progress: cards.ProgressView{Body: "…（内容已截短）", OmittedRecords: 1, ElapsedSeconds: -1}})) + cards.MaxRequestMarkerBytes + 30
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", Items: []codexclient.Item{input("user", "question"), agent("comment", strings.Repeat("<at>😀", 300), "commentary")}}
	apply(t, e, 200, false, turn)
	m, _ := e.Store.Mirror(ctx, "host", "thread", mirrorKey("progress", "turn"))
	if len(m.Card) > e.MaxCardBytes-cards.MaxRequestMarkerBytes || !json.Valid([]byte(m.Card)) {
		t.Fatal("progress was not bounded by actual card bytes")
	}
	turn.Status = "completed"
	turn.Items = append(turn.Items, agent("final", "answer", "final_answer"))
	apply(t, e, 201, false, turn)
	turn.Status = "inProgress"
	apply(t, e, 202, false, turn)
	m, _ = e.Store.Mirror(ctx, "host", "thread", mirrorKey("progress", "turn"))
	if !m.Terminal || !strings.Contains(m.Card, "已完成") {
		t.Fatal("late busy snapshot regressed completion")
	}
}

type historyFixture struct {
	turns []codexclient.Turn
	err   error
}

func (h historyFixture) Turns(context.Context, string) ([]codexclient.Turn, error) {
	return h.turns, h.err
}

func TestInvalidOrIncompleteHistoryCannotAdvanceCheckpoint(t *testing.T) {
	for _, turn := range []codexclient.Turn{
		{ID: "turn", Status: "completed", ItemsView: "summary", Items: []codexclient.Item{input("user", "question")}},
		{ID: "turn", Status: "unrecognized", Items: []codexclient.Item{input("user", "question")}},
		{ID: "turn", Status: "completed", Items: []codexclient.Item{agent("answer", "answer without source", "final_answer")}},
		{ID: "turn", Status: "completed", Items: []codexclient.Item{input("user", "one"), input("user", "conflicting duplicate")}},
	} {
		e, _ := engineFixture(t)
		if err := e.Apply(context.Background(), "thread", []codexclient.Turn{turn}, false, 200); err == nil {
			t.Fatal("incomplete history accepted")
		}
		mirrors, _ := e.Store.Mirrors(context.Background(), "host", "thread")
		if len(mirrors) != 0 {
			t.Fatal("partial history escaped atomic staging")
		}
	}
	e, _ := engineFixture(t)
	e.Native = historyFixture{err: errors.New("private native payload")}
	if err := e.Reconcile(context.Background(), "thread", true, 200); err == nil || strings.Contains(err.Error(), "private native payload") {
		t.Fatal("native read error leaked payload or succeeded")
	}
}

func TestExactDuplicateItemsAreDeduplicatedAndUnknownClientIsNativeInput(t *testing.T) {
	e, _ := engineFixture(t)
	u := input("user", "native input")
	unknown := "unowned-client-id"
	u.ClientID = &unknown
	turn := codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{u, u, agent("final", "answer", "final_answer")}}
	apply(t, e, 200, false, turn)
	mirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil || len(mirrors) != 2 || mirrors[0].Kind != "human" {
		t.Fatalf("unknown client suppressed or duplicate human mirrored: %d %v", len(mirrors), err)
	}
}

func TestTerminalNotificationProvenanceKeepsOriginalSecondsAndReplay(t *testing.T) {
	ctx := context.Background()
	for _, at := range []*int64{nil, intPointer(150), intPointer(150000), intPointer(-1)} {
		e, _ := engineFixture(t)
		e.MaxCardBytes = 650
		turn := codexclient.Turn{ID: "turn", Status: "completed", CompletedAt: at, Items: []codexclient.Item{input("user", "question"), agent("final", strings.Repeat("answer", 200), "final_answer")}}
		apply(t, e, 200000, true, turn)
		first, _ := e.Store.Mirror(ctx, "host", "thread", mirrorKey("final", "turn", "0"))
		expected := int64(0)
		if at != nil && *at == 150 {
			expected = 150
		}
		if first.EventType != "completed" || first.EventAt != expected || !first.Replay {
			t.Fatalf("bad original event evidence: %+v", first)
		}
		turn.CompletedAt = intPointer(199)
		turn.Items[1].Text = "corrected answer"
		apply(t, e, 201000, false, turn)
		first, _ = e.Store.Mirror(ctx, "host", "thread", mirrorKey("final", "turn", "0"))
		if first.EventAt != expected || !first.Replay {
			t.Fatal("correction invented a new terminal notification event")
		}
	}
}

func intPointer(value int64) *int64 { return &value }
