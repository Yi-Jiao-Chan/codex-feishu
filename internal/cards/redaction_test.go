package cards

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSafeMasksEmailAddressesAsLiteralPlaceholder(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "联系 alice@example.invalid。", "联系 &lt;A&gt;。"},
		{"aliases", "First.Last+ci@Sub.Example.INVALID, build_bot@ci.example.invalid", "&lt;A&gt;, &lt;A&gt;"},
		{"git identity", "Synthetic Author <123+synthetic@users.noreply.example.invalid>", "Synthetic Author &lt;A&gt;"},
		{"markdown", "**alice@example.invalid** and `bob@example.invalid`", "**&lt;A&gt;** and `&lt;A&gt;`"},
		{"email link", "[联系作者](mailto:alice@example.invalid)", "联系作者 &lt;A&gt;"},
		{"email label and query", "[alice@example.invalid](MAILTO:bob@example.invalid?cc=carol@example.invalid)", "&lt;A&gt;"},
		{"email autolinks", "<alice@example.invalid> <mailto:bob@example.invalid> mailto:carol@example.invalid", "&lt;A&gt; &lt;A&gt; &lt;A&gt;"},
		{"other text", "@operator https://example.invalid/path value@localhost 2 > 1", "@operator https://example.invalid/path value@localhost 2 &gt; 1"},
		{"mention safety", "<at id=\"all\"></at> alice@example.invalid", "&lt;at id=\"all\"&gt;&lt;/at&gt; &lt;A&gt;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Safe(tc.input); got != tc.want {
				t.Fatalf("unexpected public text: %q", got)
			}
		})
	}
}

func TestEmailRedactionPreservesMarkdownMentionsAndRequestMarker(t *testing.T) {
	body := "> 联系 alice@example.invalid\n\n```sh\ngit config user.email bob+work@example.invalid\n```"
	card := Message("AI 回复", "Codex", body)
	card, err := WithMentions(card, []string{"ou_synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	card, marked, err := WithRequestMarker(card, "cf_synthetic")
	if err != nil || !marked {
		t.Fatalf("marker failed: %v", err)
	}
	var decoded any
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatal(err)
	}
	first := humanElements(t, card)[0].(map[string]any)
	if first["content"] != Mention("ou_synthetic")+"\n\n> 联系 &lt;A&gt;" || first["element_id"] != "cf_synthetic" {
		t.Fatal("redaction changed the trusted mention, quote, or request marker")
	}
	code := humanElements(t, card)[1].(map[string]any)["content"].(string)
	if code != "```sh\ngit config user.email &lt;A&gt;\n```" {
		t.Fatalf("code block lost its structure: %q", code)
	}
	if strings.Contains(card, "@example.invalid") || strings.Contains(card, "<A>") {
		t.Fatal("card retained an address or activated the placeholder as a tag")
	}
}

func TestSplitMessageNeverSplitsAnEmailBeforeRedaction(t *testing.T) {
	address := strings.Repeat("long", 35) + "+ci@example.invalid"
	for _, body := range []string{
		strings.Repeat("文字 "+address+" end\n", 30),
		"```text\n" + strings.Repeat("prefix "+address+" suffix\n", 30) + "```",
		strings.Repeat("[联系](mailto:"+address+") end\n", 30),
	} {
		parts := checkSegments(t, "AI 回复", "Codex", body, 460)
		if len(parts) < 2 {
			t.Fatal("fixture did not exercise splitting")
		}
		masked := 0
		for _, part := range parts {
			for _, raw := range humanElements(t, part.Card) {
				content, _ := raw.(map[string]any)["content"].(string)
				masked += strings.Count(content, "&lt;A&gt;")
				if strings.Contains(content, "long") || strings.Contains(content, "@") || strings.Contains(content, "example.invalid") {
					t.Fatal("splitting exposed an email fragment")
				}
			}
		}
		if masked != 30 {
			t.Fatalf("lost or duplicated placeholders: %d", masked)
		}
	}
}

func TestShortEmailRedactionUsesActualEncodedCardBudget(t *testing.T) {
	body := strings.Repeat("a@b.co ", 100)
	parts := checkSegments(t, "AI 回复", "Codex", body, 430)
	if len(parts) < 2 {
		t.Fatal("fixture did not exercise the larger encoded placeholder")
	}
	for _, part := range parts {
		if strings.Contains(part.Card, "@") {
			t.Fatal("short address remained in a display segment")
		}
	}
}
