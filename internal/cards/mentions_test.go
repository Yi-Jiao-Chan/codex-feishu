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
	prefix := Mention("ou_receiver") + " " + Mention("ou_second") + " "
	if !strings.HasPrefix(contents[0], prefix) || strings.Count(contents[0], "<at") != 2 || contents[len(contents)-1] != "> AI · Codex" || len(contents) != 2 {
		t.Fatalf("body created an unauthorized mention: %q", contents)
	}
	if unchanged, err := WithMentions(base, nil); err != nil || unchanged != base {
		t.Fatalf("empty mentions changed base: %v", err)
	}
}

func TestMentionsKeepParagraphInlineAndPreserveMarkdownBlocks(t *testing.T) {
	for _, tt := range []struct{ body, want string }{
		{"这是最终答案。", Mention("ou_user") + " 这是最终答案。"},
		{"# 标题\n\n正文", "# " + Mention("ou_user") + " 标题\n\n正文"},
		{"- 第一项\n- 第二项", "- " + Mention("ou_user") + " 第一项\n- 第二项"},
		{"```go\nfmt.Println(1)\n```", Mention("ou_user") + "\n\n```go\nfmt.Println(1)\n```"},
		{"| A | B |\n| --- | --- |", Mention("ou_user") + "\n\n| A | B |\n| --- | --- |"},
		{"    indented code", Mention("ou_user") + "\n\n    indented code"},
	} {
		card, err := WithMentions(Message("AI", "Codex", tt.body), []string{"ou_user"})
		if err != nil {
			t.Fatal(err)
		}
		if got := mentionTestContents(t, card)[0]; got != tt.want {
			t.Fatalf("prefix corrupted visible Markdown: got %q want %q", got, tt.want)
		}
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
