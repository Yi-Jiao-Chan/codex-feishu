package cards

import (
	"encoding/json"
	"strings"
	"testing"
)

func mentionTestContents(t *testing.T, card string) []string {
	t.Helper()
	var parsed struct {
		Body struct {
			Elements []struct{ Content string } `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(card), &parsed); err != nil {
		t.Fatal(err)
	}
	var contents []string
	for _, element := range parsed.Body.Elements {
		contents = append(contents, element.Content)
	}
	return contents
}

func TestWithMentionsUsesOnlyExplicitTargets(t *testing.T) {
	base := Message("AI", "Codex", `<at id="all"></at> <at id="ou_injected"></at>`)
	got, err := WithMentions(base, []string{"ou_receiver", "ou_receiver", "ou_second"})
	if err != nil {
		t.Fatal(err)
	}
	contents := mentionTestContents(t, got)
	if strings.Contains(contents[0], "<at") || contents[len(contents)-1] != Mention("ou_receiver")+" "+Mention("ou_second") {
		t.Fatalf("body created an unauthorized mention: %q", contents)
	}
	if unchanged, err := WithMentions(base, nil); err != nil || unchanged != base {
		t.Fatalf("empty mentions changed base: %v", err)
	}
}

func TestWithMentionsRejectsAllAndMalformedTargets(t *testing.T) {
	for _, id := range []string{"", "all", "@all", "ou_", `ou_user"><at id="all`, "user", "ou_user\n"} {
		if ValidMentionID(id) {
			t.Errorf("invalid mention ID accepted: %q", id)
		}
		if _, err := WithMentions(Message("AI", "Codex", "body"), []string{id}); err == nil {
			t.Errorf("invalid mention rendered: %q", id)
		}
	}
	for _, base := range []string{"invalid", `{"body":"text"}`, `{"schema":"2.0","body":{}}`} {
		if _, err := WithMentions(base, []string{"ou_receiver"}); err == nil {
			t.Errorf("invalid base card accepted: %s", base)
		}
	}
	if _, err := WithMentions(Message("AI", "Codex", strings.Repeat("a", DefaultMaxCardBytes)), []string{"ou_receiver"}); err == nil {
		t.Fatal("oversized mention card accepted")
	}
}
