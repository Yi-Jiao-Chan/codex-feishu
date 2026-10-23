package sync

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type sessionSettingsNative struct {
	settings codexclient.ThreadSettings
	err      error
	calls    int
	cancel   context.CancelFunc
}

func (h *sessionSettingsNative) Turns(context.Context, string) ([]codexclient.Turn, error) {
	return nil, nil
}

func (h *sessionSettingsNative) ReadThreadSettings(ctx context.Context, _ string) (codexclient.ThreadSettings, error) {
	h.calls++
	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
		<-ctx.Done()
		return codexclient.ThreadSettings{}, ctx.Err()
	}
	if h.err != nil {
		return codexclient.ThreadSettings{}, h.err
	}
	return h.settings, nil
}

func settingsString(value string) *string { return &value }

func sessionSettingsTurn(id, body string) codexclient.Turn {
	return codexclient.Turn{
		ID:        id,
		Status:    "completed",
		ItemsView: "full",
		Items:     []codexclient.Item{input(id+"-input", "question"), agent(id+"-final", body, "final_answer")},
	}
}

func sessionSettingsFooter(t *testing.T, card string) string {
	t.Helper()
	var doc struct {
		Body struct {
			Elements []struct {
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(card), &doc); err != nil {
		t.Fatalf("invalid final card: %v", err)
	}
	if len(doc.Body.Elements) == 0 {
		t.Fatal("final card has no footer")
	}
	return doc.Body.Elements[len(doc.Body.Elements)-1].Content
}

func TestFinalFooterRecordsLatestLiveSettingsOnce(t *testing.T) {
	e, _ := engineFixture(t)
	native := &sessionSettingsNative{settings: codexclient.ThreadSettings{
		ThreadID:         "thread",
		Model:            "model-live",
		ReasoningEffort:  settingsString("high"),
		ServiceTier:      settingsString("priority"),
		ServiceTierKnown: true,
	}}
	e.Native = native
	apply(t, e, 200, false, sessionSettingsTurn("turn", "live answer"))

	want := cards.ModelSettingsText(native.settings.Model, native.settings.ReasoningEffort, native.settings.ServiceTier, native.settings.ServiceTierKnown)
	m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
	if err != nil {
		t.Fatalf("final footer was not stored: %v", err)
	}
	if got := sessionSettingsFooter(t, m.Card); got != "> "+want {
		t.Fatalf("final footer was not exact: got %q, want %q", got, "> "+want)
	}
	if strings.Contains(m.Card, "AI 回复 · "+want) {
		t.Fatalf("final footer did not capture native settings: %q %v", m.Card, err)
	}
	if native.calls != 1 {
		t.Fatalf("ReadThreadSettings called %d times, want once", native.calls)
	}
	sources, err := e.Store.ReplySettingsSources(context.Background(), "host", "thread")
	if err != nil || sources["turn"] != want {
		t.Fatalf("settings source was not persisted: %#v %v", sources, err)
	}
}

func TestHistoricalAndReplayFinalsRemainUnknown(t *testing.T) {
	for _, replay := range []bool{true, false} {
		t.Run(map[bool]string{true: "replay", false: "historical-live"}[replay], func(t *testing.T) {
			e, _ := engineFixture(t)
			native := &sessionSettingsNative{settings: codexclient.ThreadSettings{
				ThreadID:         "thread",
				Model:            "current-model",
				ReasoningEffort:  settingsString("high"),
				ServiceTier:      settingsString("fast"),
				ServiceTierKnown: true,
			}}
			e.Native = native
			turns := []codexclient.Turn{sessionSettingsTurn("old", "old answer"), sessionSettingsTurn("latest", "latest answer")}
			apply(t, e, 200, replay, turns...)

			sources, err := e.Store.ReplySettingsSources(context.Background(), "host", "thread")
			if err != nil {
				t.Fatal(err)
			}
			unknown := cards.ModelSettingsText("", nil, nil, false)
			if sources["old"] != unknown {
				t.Fatalf("historical source was labeled with current settings: %#v", sources)
			}
			if replay {
				if sources["latest"] != unknown || native.calls != 0 {
					t.Fatalf("replay read or labeled native settings: %#v calls=%d", sources, native.calls)
				}
			} else {
				want := cards.ModelSettingsText(native.settings.Model, native.settings.ReasoningEffort, native.settings.ServiceTier, native.settings.ServiceTierKnown)
				if sources["latest"] != want || native.calls != 1 {
					t.Fatalf("latest live source/read mismatch: %#v calls=%d", sources, native.calls)
				}
			}
			for _, turnID := range []string{"old", "latest"} {
				m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", turnID, "0"))
				if err != nil || !strings.Contains(m.Card, sources[turnID]) || strings.Contains(m.Card, "AI 回复 · "+sources[turnID]) {
					t.Fatalf("turn %s footer mismatch: %q %v", turnID, m.Card, err)
				}
			}
		})
	}
}

func TestReplySettingsSourceSurvivesChangesAndRestart(t *testing.T) {
	e, path := engineFixture(t)
	native := &sessionSettingsNative{settings: codexclient.ThreadSettings{
		ThreadID:         "thread",
		Model:            "model-before",
		ReasoningEffort:  settingsString("medium"),
		ServiceTier:      settingsString("default"),
		ServiceTierKnown: true,
	}}
	e.Native = native
	turn := sessionSettingsTurn("turn", "first answer")
	apply(t, e, 200, false, turn)
	before, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
	if err != nil {
		t.Fatal(err)
	}
	want := cards.ModelSettingsText(native.settings.Model, native.settings.ReasoningEffort, native.settings.ServiceTier, native.settings.ServiceTierKnown)
	if !strings.Contains(before.Card, want) || strings.Contains(before.Card, "AI 回复 · "+want) {
		t.Fatalf("initial source missing: %q", before.Card)
	}

	if err := e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	e.Store = reopened
	native.settings.Model = "model-after"
	native.settings.ReasoningEffort = settingsString("low")
	native.settings.ServiceTier = settingsString("fast")
	turn.Items[1].Text = "corrected answer"
	apply(t, e, 201, false, turn)
	after, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
	if err != nil || !strings.Contains(after.Card, want) || strings.Contains(after.Card, "AI 回复 · "+want) || strings.Contains(after.Card, "model-after") {
		t.Fatalf("source changed after restart/settings update: %q %v", after.Card, err)
	}
	if native.calls != 1 {
		t.Fatalf("persisted source did not prevent a second native read: %d", native.calls)
	}
}

func TestReplySettingsSourceSurvivesPublicPreparationRerender(t *testing.T) {
	e, _ := engineFixture(t)
	native := &sessionSettingsNative{settings: codexclient.ThreadSettings{
		ThreadID:         "thread",
		Model:            "model-prepared",
		ReasoningEffort:  settingsString("high"),
		ServiceTier:      settingsString("fast"),
		ServiceTierKnown: true,
	}}
	e.Native = native
	e.PreparePublic = func(context.Context, string, string) (string, error) {
		return "prepared display", nil
	}
	apply(t, e, 200, false, sessionSettingsTurn("turn", "native body"))
	want := cards.ModelSettingsText(native.settings.Model, native.settings.ReasoningEffort, native.settings.ServiceTier, native.settings.ServiceTierKnown)
	m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
	if err != nil || !strings.Contains(m.Card, "prepared display") || !strings.Contains(m.Card, want) || strings.Contains(m.Card, "AI 回复 · "+want) {
		t.Fatalf("prepared final lost source or body: %q %v", m.Card, err)
	}
}

func TestLongFinalUsesSettingsFooterOnEveryBoundedSegment(t *testing.T) {
	e, _ := engineFixture(t)
	e.MaxCardBytes = 900
	native := &sessionSettingsNative{settings: codexclient.ThreadSettings{
		ThreadID:         "thread",
		Model:            "model-long",
		ReasoningEffort:  settingsString("high"),
		ServiceTier:      settingsString("fast"),
		ServiceTierKnown: true,
	}}
	e.Native = native
	apply(t, e, 200, false, sessionSettingsTurn("turn", "```text\n"+strings.Repeat("long line\n", 240)+"```"))
	want := cards.ModelSettingsText(native.settings.Model, native.settings.ReasoningEffort, native.settings.ServiceTier, native.settings.ServiceTierKnown)
	mirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	finals := 0
	for _, mirror := range mirrors {
		if mirror.Kind != "final" {
			continue
		}
		finals++
		if len(mirror.Card) > e.MaxCardBytes-cards.MaxRequestMarkerBytes || !json.Valid([]byte(mirror.Card)) {
			t.Fatalf("segment exceeds public budget: %d", len(mirror.Card))
		}
		if !strings.Contains(mirror.Card, want) || strings.Contains(mirror.Card, "AI 回复 · "+want) {
			t.Fatalf("segment lost settings footer: %q", mirror.Card)
		}
	}
	if finals < 2 {
		t.Fatal("fixture did not require segmented final reply")
	}
}

func TestNativeSettingsFailureFallsBackToUnknownAndCancellationAborts(t *testing.T) {
	e, _ := engineFixture(t)
	native := &sessionSettingsNative{err: errors.New("settings unavailable")}
	e.Native = native
	apply(t, e, 200, false, sessionSettingsTurn("turn", "answer"))
	m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
	if err != nil || !strings.Contains(m.Card, "模型未知") || strings.Contains(m.Card, "AI 回复 · 模型未知") || strings.Contains(m.Card, "settings unavailable") {
		t.Fatalf("settings failure did not fall back safely: %q %v", m.Card, err)
	}

	e2, _ := engineFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	native2 := &sessionSettingsNative{cancel: cancel}
	e2.Native = native2
	err = e2.Apply(ctx, "thread", []codexclient.Turn{sessionSettingsTurn("turn", "answer")}, false, 200)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context cancellation was swallowed: %v", err)
	}
	if mirrors, readErr := e2.Store.Mirrors(context.Background(), "host", "thread"); readErr != nil || len(mirrors) != 0 {
		t.Fatalf("canceled snapshot staged public docs: %d %v", len(mirrors), readErr)
	}
}

func TestMismatchedNativeSettingsThreadFallsBackToUnknown(t *testing.T) {
	e, _ := engineFixture(t)
	native := &sessionSettingsNative{settings: codexclient.ThreadSettings{
		ThreadID:         "another-thread",
		Model:            "wrong-model",
		ReasoningEffort:  settingsString("high"),
		ServiceTier:      settingsString("priority"),
		ServiceTierKnown: true,
	}}
	e.Native = native
	apply(t, e, 200, false, sessionSettingsTurn("turn", "answer"))
	m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
	if err != nil || !strings.Contains(m.Card, "模型未知") || strings.Contains(m.Card, "AI 回复 · 模型未知") || strings.Contains(m.Card, "wrong-model") {
		t.Fatalf("mismatched settings were rendered as current: %q %v", m.Card, err)
	}
	sources, err := e.Store.ReplySettingsSources(context.Background(), "host", "thread")
	if err != nil || sources["turn"] != cards.ModelSettingsText("", nil, nil, false) {
		t.Fatalf("mismatched settings source was not frozen unknown: %#v %v", sources, err)
	}
}

func TestLegacyFinalWithoutSettingsSourceKeepsCodexFooter(t *testing.T) {
	e := Engine{}
	turn := sessionSettingsTurn("turn", "corrected answer")
	old := []store.Mirror{{Key: "legacy-final", TurnID: "turn", Kind: "final", ParentKey: "legacy-human"}}
	plan, err := e.renderSnapshot("thread", []codexclient.Turn{turn}, nil, old, false, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range plan.Docs {
		if doc.Kind == "final" && !strings.Contains(doc.Card, "AI 回复 · Codex") {
			t.Fatalf("legacy final source changed: %q", doc.Card)
		}
	}
}
