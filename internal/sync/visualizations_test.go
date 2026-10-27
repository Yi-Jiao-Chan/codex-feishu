package sync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/filepreview"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

func visualized(path string) string {
	payload, _ := json.Marshal(map[string]string{"path": path})
	return "\ue200visualize\ue202" + string(payload) + "\ue201"
}

func TestVisualizationFallbackPrecedesAllPublicRendering(t *testing.T) {
	e, _ := engineFixture(t)
	e.MaxCardBytes = 1200
	marker := visualized("/project/" + strings.Repeat("nested/", 400) + "chart.html")
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", Items: []codexclient.Item{
		input("user", "request"), agent("progress", marker, "commentary"),
	}}
	apply(t, e, 200, false, turn)
	progress, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if err != nil || !strings.Contains(progress.Card, codexclient.VisualizationFallback) || strings.Contains(progress.Card, "visualize") {
		t.Fatal("preview-disabled progress exposed the App-only visualization")
	}
	turn.Status = "completed"
	turn.Items = append(turn.Items, agent("final", marker, "final_answer"))
	plan, err := e.renderSnapshot("thread", []codexclient.Turn{turn}, nil, nil, true, 201)
	if err != nil {
		t.Fatal(err)
	}
	finals := 0
	for _, m := range plan.Docs {
		if m.Kind == "final" {
			finals++
			if len(m.Card) > e.MaxCardBytes || !strings.Contains(m.Card, codexclient.VisualizationFallback) || strings.Contains(m.Card, "nested/") {
				t.Fatal("pure rendering did not normalize before the card byte budget")
			}
		}
	}
	if finals != 1 || turn.Items[1].Text != marker || turn.Items[2].PublicText() != marker {
		t.Fatal("visualization rendering changed native text or split the private wrapper")
	}
	// Normalization must follow duplicate validation: two different native
	// items can have identical fallback text without being the same snapshot.
	turn.Items = append(turn.Items, agent("final", visualized("/different/chart.html"), "final_answer"))
	if err := e.Apply(context.Background(), "thread", []codexclient.Turn{turn}, false, 202); err == nil {
		t.Fatal("normalization hid conflicting native item identities")
	}
}

func TestVisualizationPreviewReachesBothPhasesBeforeSplitting(t *testing.T) {
	e, _ := engineFixture(t)
	bindEngine(t, e)
	e.MaxCardBytes = 1200
	dir := t.TempDir()
	root := filepath.Join(dir, "workspace")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "preview.key")
	if err := os.WriteFile(secret, []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := filepreview.New(filepreview.Options{BaseURL: "http://127.0.0.1:8787", AllowedRoots: []string{root},
		Directory: filepath.Join(dir, "registry"), SecretFile: secret})
	if err != nil {
		t.Fatal(err)
	}
	defer preview.Close()
	var body strings.Builder
	firstVisualization := ""
	for n := range 12 {
		path := filepath.Join(root, strings.Repeat("a", n+1)+".html")
		if err := os.WriteFile(path, []byte("<p>synthetic chart</p>"), 0600); err != nil {
			t.Fatal(err)
		}
		body.WriteString("图表：\n\n" + visualized(path) + "\n\n")
		if n == 0 {
			firstVisualization = "图表：\n\n" + visualized(path) + "\n\n"
		}
	}
	firstPreview, err := preview.Rewrite(context.Background(), firstVisualization)
	if err != nil {
		t.Fatal(err)
	}
	// The progress card retains complete records inside the outer history panel.
	// Reserve its actual wire overhead and at least one complete preview link;
	// the final below still uses the small splitting budget independently.
	e.MaxCardBytes = len(cards.Task(cards.TaskView{Status: "inProgress", Progress: cards.ProgressView{Body: firstPreview, CurrentItemID: "progress", Records: []cards.ProgressRecord{{ItemID: "progress", Text: firstPreview, ReceivedAtMS: 200}}, ElapsedSeconds: -1}})) + cards.MaxRequestMarkerBytes
	raw := body.String()
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", Items: []codexclient.Item{
		input("user", visualized("/user/example.html")), agent("progress", raw, "partial_answer"),
	}}
	e.PreparePublic = func(ctx context.Context, _ string, text string) (string, error) {
		return preview.Rewrite(ctx, text)
	}
	apply(t, e, 200, false, turn)
	progress, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if err != nil || !strings.Contains(progress.Card, "/f/") || strings.Contains(progress.Card, "\ue200") {
		t.Fatal("partial answer did not receive visualization previews")
	}
	turn.Status = "completed"
	e.MaxCardBytes = 1200
	turn.Items = append(turn.Items, agent("final", raw, "final_answer"))
	original := append([]codexclient.Item(nil), turn.Items...)
	apply(t, e, 201, false, turn)
	if !reflect.DeepEqual(turn.Items, original) {
		t.Fatal("preview changed caller/native items")
	}
	mirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	finals, urls := 0, 0
	for _, m := range mirrors {
		if len(m.Card) > e.MaxCardBytes {
			t.Fatal("visualization links exceeded the complete card byte budget")
		}
		if m.Kind == "human" && !strings.Contains(m.Card, "visualize") {
			t.Fatal("assistant normalization changed a user code example")
		}
		if m.Kind == "final" {
			finals++
			urls += strings.Count(m.Card, "http://127.0.0.1:8787/f/")
			if strings.Contains(m.Card, "\ue200") || strings.Contains(m.Card, root) || strings.Contains(m.Card, codexclient.VisualizationFallback) {
				t.Fatal("final visualization retained a private wrapper or fallback")
			}
		}
	}
	if finals < 2 || urls != 12 {
		t.Fatalf("split visualization content was lost: %d cards, %d links", finals, urls)
	}
	gateway := &cardGateway{}
	drain(t, e, gateway, 1000)
	writes := len(gateway.writes)
	apply(t, e, 202, true, turn)
	drain(t, e, gateway, 5000)
	if len(gateway.writes) != writes {
		t.Fatal("replay changed stable visualization cards")
	}
}

func TestVisualizationCorrectsDeliveredCardWithoutResendingOrMentioning(t *testing.T) {
	ctx := context.Background()
	e, _ := engineFixture(t)
	bindEngine(t, e)
	event := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	completed := event.Unix()
	marker := visualized("/project/chart.html")
	turn := codexclient.Turn{ID: "turn", Status: "completed", CompletedAt: &completed, Items: []codexclient.Item{
		input("user", "request"), agent("final", marker, "final_answer"),
	}}
	// Seed the old version's already-delivered card under its stable identity.
	plan, err := e.renderSnapshot("thread", []codexclient.Turn{turn}, nil, nil, false, event.UnixMilli()+1000)
	if err != nil {
		t.Fatal(err)
	}
	key := mirrorKey("final", "turn", "0")
	for n := range plan.Docs {
		if plan.Docs[n].Key == key {
			plan.Docs[n].Card = cards.Message("AI 回复", "Codex", marker)
		}
	}
	if err := e.Store.StageSnapshot(ctx, "host", "thread", plan.Docs, plan.Links, plan.Mark); err != nil {
		t.Fatal(err)
	}
	policy, _ := rules.DefaultPolicy("UTC")
	if _, err := e.Store.InitNotificationPolicyAt(ctx, "app", "ou_user", policy, event.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	options := &store.MentionOptions{Enabled: true, Targets: []string{"ou_user"}}
	if err := e.Store.ScheduleMentions(ctx, "host", "thread", *options); err != nil {
		t.Fatal(err)
	}
	gateway := &cardGateway{}
	out := Outbound{Store: e.Store, Gateway: gateway, Mentions: options,
		Membership: &mentionMembership{s: e.Store, t: t, at: event.Add(time.Minute), ids: []string{"ou_user"}}}
	dispatch := func() {
		t.Helper()
		for range 10 {
			result, err := out.DispatchOne(ctx, "host", "thread", event.UnixMilli()+60000)
			if err != nil || result.AttemptID != "" && result.State != "confirmed" {
				t.Fatalf("delivery failed: %+v, %v", result, err)
			}
			if result.AttemptID == "" {
				return
			}
		}
		t.Fatal("visualization delivery did not settle")
	}
	dispatch()
	old, err := e.Store.Mirror(ctx, "host", "thread", key)
	if err != nil || old.RemoteID == "" || !strings.Contains(gateway.cards[old.RemoteID], `\u003cat id=`) {
		t.Fatal("old visualization card was not delivered with its initial reminder")
	}
	before := len(gateway.writes)
	apply(t, e, event.UnixMilli()+2000, true, turn)
	if err := e.Store.ScheduleMentions(ctx, "host", "thread", *options); err != nil {
		t.Fatal(err)
	}
	dispatch()
	corrected, err := e.Store.Mirror(ctx, "host", "thread", key)
	if err != nil || corrected.RemoteID != old.RemoteID || corrected.Revision != old.Revision+1 ||
		len(gateway.writes) != before+1 || gateway.writes[before].Operation != "update" ||
		gateway.writes[before].ID != old.RemoteID || corrected.Card != "" || corrected.CardHash == "" || !strings.Contains(gateway.writes[before].Card, codexclient.VisualizationFallback) ||
		strings.Contains(gateway.writes[before].Card, `\u003cat id=`) {
		t.Fatal("visualization correction did not update the original card without a reminder")
	}
	apply(t, e, event.UnixMilli()+3000, true, turn)
	dispatch()
	if len(gateway.writes) != before+1 {
		t.Fatal("a second reconciliation resent the correction")
	}
}

func TestLegacyVisualizationAdoptionUsesReadableDisplay(t *testing.T) {
	seed, turns, inventory := legacyFixture()
	marker := visualized("/project/chart.html")
	turns[0].Items[1].Text = marker
	got := planLegacy(t, seed, turns, inventory)
	for _, mirror := range got.Mirrors {
		if mirror.Kind == "final" && (mirror.RemoteID != "final" || !strings.Contains(mirror.Card, codexclient.VisualizationFallback) || strings.Contains(mirror.Card, "visualize")) {
			t.Fatal("legacy adoption changed identity or exposed App-only markup")
		}
	}
	if turns[0].Items[1].Text != marker {
		t.Fatal("legacy adoption changed native history")
	}
}
