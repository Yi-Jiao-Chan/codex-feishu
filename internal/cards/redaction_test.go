package cards

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSafeReplacesOnlyEmailAtWithLiteralAngleBrackets(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "联系 alice@example.invalid。", "联系 alice<A>example.invalid。"},
		{"aliases", "First.Last+ci@Sub.Example.INVALID, build_bot@ci.example.invalid", "First.Last+ci<A>Sub.Example.INVALID, build_bot<A>ci.example.invalid"},
		{"git identity", "Synthetic Author <123+synthetic@users.noreply.example.invalid>", "Synthetic Author &lt;123+synthetic<A>users.noreply.example.invalid&gt;"},
		{"markdown", "**alice@example.invalid** and `bob@example.invalid`", "**alice<A>example.invalid** and `bob<A>example.invalid`"},
		{"email link", "[联系作者](mailto:alice@example.invalid)", "联系作者 alice<A>example.invalid"},
		{"email label", "[alice@example.invalid](mailto:alice@example.invalid)", "alice<A>example.invalid"},
		{"email label and query", "[alice@example.invalid](MAILTO:bob@example.invalid?cc=carol@example.invalid)", "alice<A>example.invalid bob<A>example.invalid"},
		{"email autolinks", "<alice@example.invalid> <mailto:bob@example.invalid> mailto:carol@example.invalid", "&lt;alice<A>example.invalid&gt; &lt;bob<A>example.invalid&gt; carol<A>example.invalid"},
		{"other text", "@operator https://example.invalid/path value@localhost 2 > 1", "@operator https://example.invalid/path value@localhost 2 &gt; 1"},
		{"mention safety", "<at id=\"all\"></at> alice@example.invalid", "&lt;at id=\"all\"&gt;&lt;/at&gt; alice<A>example.invalid"},
		{"input placeholder safety", "<A> alice@example.invalid", "&lt;A&gt; alice<A>example.invalid"},
		{"link label safety", "[<at id=\"all\"></at>](mailto:alice@example.invalid)", "&lt;at id=\"all\"&gt;&lt;/at&gt; alice<A>example.invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Safe(tc.input); got != tc.want {
				t.Fatalf("unexpected public text: %q", got)
			}
		})
	}
}

func TestEmailRedactionPreservesMarkdownMentionsAndRequestMarker(t *testing.T) {
	body := "> 联系 alice@example.invalid\n\n`user.email=carol@example.invalid`\n\n```sh\ngit config user.email bob+work@example.invalid\n```"
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
	if first["content"] != Mention("ou_synthetic")+"\n\n> 联系 alice<A>example.invalid" || first["element_id"] != "cf_synthetic" {
		t.Fatal("redaction changed the trusted mention, quote, or request marker")
	}
	inline := humanElements(t, card)[1].(map[string]any)["content"].(string)
	if inline != "`user.email=carol<A>example.invalid`" {
		t.Fatalf("inline code did not preserve the address and literal brackets: %q", inline)
	}
	code := humanElements(t, card)[2].(map[string]any)["content"].(string)
	if code != "```sh\ngit config user.email bob+work<A>example.invalid\n```" {
		t.Fatalf("code block lost its structure: %q", code)
	}
	if strings.Contains(card, "@example.invalid") || strings.Contains(card, `\u0026lt;A\u0026gt;`) {
		t.Fatal("card retained an email separator or sent an HTML entity as its replacement")
	}
}

func TestSplitMessageNeverSplitsAnEmailBeforeRedaction(t *testing.T) {
	address := strings.Repeat("long", 35) + "+ci@example.invalid"
	for _, body := range []string{
		strings.Repeat("文字 "+address+" end\n", 30),
		"```text\n" + strings.Repeat("prefix "+address+" suffix\n", 30) + "```",
		strings.Repeat("[联系](mailto:"+address+") end\n", 30),
	} {
		parts := checkSegments(t, "AI 回复", "Codex", body, 700)
		if len(parts) < 2 {
			t.Fatal("fixture did not exercise splitting")
		}
		masked := 0
		for _, part := range parts {
			for _, raw := range humanElements(t, part.Card) {
				content, _ := raw.(map[string]any)["content"].(string)
				masked += strings.Count(content, strings.ReplaceAll(address, "@", "<A>"))
				if strings.Contains(content, "@") || strings.Contains(content, "&lt;A&gt;") {
					t.Fatal("splitting kept an email separator or encoded the replacement as an entity")
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
