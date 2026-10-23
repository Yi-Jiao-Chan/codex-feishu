package cards

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelSettingsTextUsesStableFastLabels(t *testing.T) {
	effort := "high"
	fast := "fast"
	priority := "priority"
	standard := "default"
	unknown := "future-tier"
	tests := []struct {
		name      string
		tier      *string
		tierKnown bool
		want      string
	}{
		{name: "fast", tier: &fast, tierKnown: true, want: "gpt-test · high · fast"},
		{name: "priority", tier: &priority, tierKnown: true, want: "gpt-test · high · fast"},
		{name: "default", tier: &standard, tierKnown: true, want: "gpt-test · high"},
		{name: "known-null", tierKnown: true, want: "gpt-test · high"},
		{name: "unsupported", tier: &unknown, tierKnown: true, want: "gpt-test · high"},
		{name: "missing", tier: &fast, want: "gpt-test · high"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ModelSettingsText("gpt-test", &effort, tc.tier, tc.tierKnown)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := ModelSettingsText("", nil, nil, false); got != "模型未知" {
		t.Fatalf("unknown settings: %q", got)
	}
	if got := ModelSettingsText("gpt-test", nil, &fast, true); got != "gpt-test · fast" {
		t.Fatalf("fast without effort: %q", got)
	}
	if got := ModelSettingsText("gpt-test", nil, &standard, true); got != "gpt-test" {
		t.Fatalf("default without effort: %q", got)
	}
}

func TestModelSettingsTextBoundsUntrustedValues(t *testing.T) {
	if got := ModelSettingsText(strings.Repeat("m", maxSessionSettingChars+1), nil, nil, false); got != "模型未知" {
		t.Fatalf("oversized model was not bounded: %q", got)
	}
	if got := ModelSettingsText("model", stringPointer("high\nunsafe"), nil, false); got != "model" {
		t.Fatalf("control character was not omitted: %q", got)
	}
}

func TestModelSettingsTextIsEscapedByMessageFooter(t *testing.T) {
	model := `<at id="ou_malicious"></at>`
	card := Message("AI 回复", ModelSettingsText(model, nil, nil, false), "reply")
	if strings.Contains(card, model) || strings.Contains(card, "<at") {
		t.Fatal("native model markup reached the card footer")
	}
	var doc struct {
		Body struct {
			Elements []struct {
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(card), &doc); err != nil {
		t.Fatal(err)
	}
	last := doc.Body.Elements[len(doc.Body.Elements)-1].Content
	if !strings.Contains(last, "&lt;at id=\"") || !strings.Contains(last, "&lt;/at&gt;") {
		t.Fatalf("escaped model is missing from footer: %q", last)
	}
}

func stringPointer(value string) *string { return &value }
