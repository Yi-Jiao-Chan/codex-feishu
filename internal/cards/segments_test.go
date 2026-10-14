package cards

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func checkSegments(t *testing.T, role, source, body string, limit int) []Segment {
	t.Helper()
	parts, err := SplitMessage(role, source, body, limit)
	if err != nil {
		t.Fatal(err)
	}
	if limit == 0 {
		limit = DefaultMaxCardBytes
	}
	var raw strings.Builder
	for i, part := range parts {
		raw.WriteString(part.Raw)
		if !utf8.ValidString(part.Raw) || !utf8.ValidString(part.Body) {
			t.Fatalf("segment %d splits a rune", i)
		}
		if !json.Valid([]byte(part.Card)) || len(part.Card) > limit {
			t.Fatalf("segment %d invalid or exceeds limit: %d > %d", i, len(part.Card), limit)
		}
		var card struct {
			Body struct {
				Elements []struct{ Tag, Content string }
			}
		}
		if err := json.Unmarshal([]byte(part.Card), &card); err != nil {
			t.Fatal(err)
		}
		if len(card.Body.Elements) < 2 || card.Body.Elements[0].Tag != "markdown" {
			t.Fatalf("segment %d has no leading Markdown body", i)
		}
		footer := "> " + Safe(role+" · "+source)
		if len(parts) > 1 {
			footer += fmt.Sprintf(" · 第 %d / 共 %d 段", i+1, len(parts))
		}
		last := card.Body.Elements[len(card.Body.Elements)-1]
		if last.Tag != "markdown" || last.Content != footer {
			t.Fatalf("segment %d incorrect footer: %q", i, last.Content)
		}
		segmentSource := source
		if len(parts) > 1 {
			segmentSource += fmt.Sprintf(" · 第 %d / 共 %d 段", i+1, len(parts))
		}
		if part.Card != Message(role, segmentSource, part.Body) {
			t.Fatalf("segment %d does not use public message rendering", i)
		}
	}
	if raw.String() != body {
		t.Fatal("raw segments fail to reconstruct original bytes")
	}
	again, err := SplitMessage(role, source, body, limit)
	if err != nil || !reflect.DeepEqual(parts, again) {
		t.Fatal("segmentation is not deterministic")
	}
	return parts
}

func TestSplitMessageSingleAndDefault(t *testing.T) {
	for _, body := range []string{"", "公开回复\n<at id=\"person\">正文</at> 😀"} {
		parts := checkSegments(t, "AI 回复", "Codex", body, 0)
		if len(parts) != 1 || parts[0].Body != body || parts[0].Card != Message("AI 回复", "Codex", body) {
			t.Fatal("single card changed rendering")
		}
	}
	parts := checkSegments(t, "AI 回复", "Codex", strings.Repeat("中文😀", 10000), 0)
	if len(parts) < 2 {
		t.Fatal("default limit did not split long message")
	}
}

func TestSplitMessageCompactQuotedBreaksKeepRawAndActualBudget(t *testing.T) {
	body := strings.Repeat("> **引用标题**\n>\n> 内容中文 😀 <at id=\"all\"></at>\n> [对应代码](/not-read/file.go:23)\n\n", 150)
	parts := checkSegments(t, "AI 回复", "Codex", body, 900)
	if len(parts) < 2 {
		t.Fatal("quoted fixture did not exercise actual wire segmentation")
	}
	for _, part := range parts {
		if strings.Contains(part.Card, "\\u003cat ") {
			t.Fatal("quoted text activated an unconfigured mention")
		}
	}
}

func TestSplitMessageUnicodeAndEscaping(t *testing.T) {
	for _, body := range []string{
		strings.Repeat("中文😀👨‍👩‍👧‍👦", 300),
		strings.Repeat("<at id=\"person\">&\\\n\t\x00\b\f\r\u2028\u2029</at>", 150),
		strings.Repeat("x", 10000),
	} {
		parts := checkSegments(t, "AI 回复", "Codex <source>", body, 650)
		if len(parts) < 10 {
			t.Fatal("fixture should exercise two-digit segment counters")
		}
		for _, part := range parts {
			if part.Raw != part.Body {
				t.Fatal("plain text unexpectedly gained wrappers")
			}
		}
	}
}

func TestSplitMessageParagraphAndLineBoundaries(t *testing.T) {
	paragraph := strings.Repeat("p", 60) + "\n\n"
	body := strings.Repeat(paragraph, 8)
	limit := len(Message("AI", "Codex · 第 9 / 共 9 段", strings.Repeat(paragraph, 2)+"extra"))
	parts := checkSegments(t, "AI", "Codex", body, limit)
	if len(parts) != 4 {
		t.Fatalf("expected two whole paragraphs per segment, got %d", len(parts))
	}
	for _, part := range parts {
		if part.Raw != strings.Repeat(paragraph, 2) {
			t.Fatalf("paragraph boundary not preserved: %q", part.Raw)
		}
	}
	line := strings.Repeat("l", 60) + "\n"
	body = strings.Repeat(line, 8)
	limit = len(Message("AI", "Codex · 第 9 / 共 9 段", strings.Repeat(line, 2)+"extra"))
	for _, part := range checkSegments(t, "AI", "Codex", body, limit) {
		if part.Raw != strings.Repeat(line, 2) {
			t.Fatal("line boundary not preserved")
		}
	}
}

func TestSplitMessageCodeFenceContinuation(t *testing.T) {
	for _, fixture := range []struct {
		opener string
		closer string
		code   string
	}{
		{"```go", "```", strings.Repeat("fmt.Println(\"中文😀\")\n", 150)},
		{"`````markdown", "`````", strings.Repeat("```\n~~~\ntext\n", 150)},
		{"  ~~~~python", "~~~~", strings.Repeat("~~~\nprint('hello')\r\n", 150)},
		{"```text", "```", strings.Repeat("中文😀", 1500)},
	} {
		t.Run(fixture.opener, func(t *testing.T) {
			body := fixture.opener + "\n" + fixture.code + "\n" + fixture.closer
			parts := checkSegments(t, "AI 回复", "Codex", body, 800)
			if len(parts) < 2 {
				t.Fatal("fixture must split code block")
			}
			for i, part := range parts {
				if !strings.HasPrefix(part.Body, fixture.opener+"\n") || !strings.HasSuffix(part.Body, "\n"+fixture.closer) {
					t.Fatalf("segment %d lost language or matching fences: %q", i, part.Body)
				}
			}
		})
	}
}

func TestSplitMessageMixedAndUnterminatedFence(t *testing.T) {
	body := "introduction\n\n```go\n" + strings.Repeat("code\n", 300) + "```\n\nfinal result survives"
	parts := checkSegments(t, "AI", "Codex", body, 600)
	if !strings.HasSuffix(parts[len(parts)-1].Body, "final result survives") {
		t.Fatal("final answer was lost or wrapped into code")
	}
	body = "~~~text\n" + strings.Repeat("unfinished\n", 150)
	for _, part := range checkSegments(t, "AI", "Codex", body, 600) {
		if !strings.HasPrefix(part.Body, "~~~text\n") || !strings.HasSuffix(part.Body, "\n~~~") {
			t.Fatal("unterminated code block not closed for display")
		}
	}
}

func TestSplitMessageContinuationDoesNotCreateClosingFence(t *testing.T) {
	// A code run originally in the middle of a line must not become a closing
	// delimiter when its segment receives a synthetic opening delimiter.
	body := "```text\n" + strings.Repeat("x", 900) + "```\n" + strings.Repeat("code\n", 50) + "```"
	for _, part := range checkSegments(t, "AI", "Codex", body, 550) {
		if strings.HasPrefix(part.Body, "```text\n```\n") {
			t.Fatal("a mid-line code suffix became a closing delimiter")
		}
	}
}

func TestSplitMessageExactSingleCardLimit(t *testing.T) {
	body := "<at>中文😀</at>"
	limit := len(Message("AI", "Codex", body))
	parts := checkSegments(t, "AI", "Codex", body, limit)
	if len(parts) != 1 {
		t.Fatal("exact fit should keep a single card without a counter")
	}
}

func TestSplitMessageRejectsImpossibleLimitsAndInvalidUTF8(t *testing.T) {
	for _, input := range []struct {
		role, source, body string
		limit              int
	}{
		{"AI", "Codex", "text", -1},
		{"AI", "Codex", "", 1},
		{"AI", strings.Repeat("s", 1000), "text", 500},
		{"AI", "Codex", strings.Repeat("x", 1000), len(Message("AI", "Codex", ""))},
		{"AI", "Codex", "```" + strings.Repeat("language", 200) + "\ncode\n```", 600},
		{"AI", "Codex", string([]byte{0xff}), 500},
		{string([]byte{0xff}), "Codex", "text", 500},
	} {
		parts, err := SplitMessage(input.role, input.source, input.body, input.limit)
		if err == nil || parts != nil {
			t.Fatalf("expected all-or-error for limit %d", input.limit)
		}
	}
}

func TestMarkdownJSONBudgetMatchesSafeStringEncoding(t *testing.T) {
	// Exercise all ASCII controls, HTML safety expansion and JSON's special
	// Unicode escapes against the real serializer, rather than a size estimate.
	var text strings.Builder
	for r := rune(0); r < 128; r++ {
		text.WriteRune(r)
	}
	text.WriteString("中文😀\u2028\u2029")
	body := text.String()
	encoded, err := json.Marshal(Safe(body))
	if err != nil {
		t.Fatal(err)
	}
	got := len(encoded) - 2
	if want := markdownJSONBytes(body); got != want {
		t.Fatalf("encoded byte budget %d does not match serializer %d", want, got)
	}
}

func TestSplitMessageNativeParagraphByteBudget(t *testing.T) {
	paragraph := "A distinct paragraph 😀\n\n"
	limit := len(Message("AI", "Codex · 第 9 / 共 9 段", paragraph+paragraph))
	parts := checkSegments(t, "AI", "Codex", strings.Repeat(paragraph, 8), limit)
	if len(parts) != 4 {
		t.Fatalf("two rendered paragraphs should fit each card, got %d cards", len(parts))
	}
	for _, part := range parts {
		if part.Raw != paragraph+paragraph {
			t.Fatal("native block overhead disturbed whole-paragraph boundaries")
		}
	}
}

func TestSplitMessageMixedNativeBlocksPreserveVisibleText(t *testing.T) {
	var body strings.Builder
	for n := 0; n < 100; n++ {
		fmt.Fprintf(&body, "paragraph-%03d 😀\n\n- list-%03d-a\n- list-%03d-b\n\n> quote-%03d\n> continued\n\n```go\nfmt.Println(\"code-%03d\\\\\\\"中文\u2028\u2029\")\n```\n\n", n, n, n, n, n)
	}
	parts := checkSegments(t, "AI", "Codex", body.String(), 1100)
	var visible strings.Builder
	for _, part := range parts {
		for _, entry := range humanSegmentElements(t, part.Card) {
			markdown, ok := entry.(map[string]any)
			if ok && markdown["tag"] == "markdown" {
				content, _ := markdown["content"].(string)
				visible.WriteString(content)
			}
		}
	}
	for n := 0; n < 100; n++ {
		for _, kind := range []string{"paragraph", "list", "quote", "code"} {
			marker := fmt.Sprintf("%s-%03d", kind, n)
			want := 1
			if kind == "list" {
				want = 2
			}
			if count := strings.Count(visible.String(), marker); count != want {
				t.Fatalf("visible body lost or duplicated %s: got %d want %d", marker, count, want)
			}
		}
	}
}

func TestSplitMessageMinimumRuneBudgetAcrossCounterWidth(t *testing.T) {
	body := strings.Repeat("😀", 30)
	limit := len(Message("AI", "Codex · 第 99 / 共 99 段", "😀"))
	parts := checkSegments(t, "AI", "Codex", body, limit)
	if len(parts) != 30 {
		t.Fatalf("minimum two-digit card budget should fit one rune, got %d cards", len(parts))
	}
	for _, part := range parts {
		if part.Raw != "😀" {
			t.Fatal("minimum card budget split or omitted a Unicode rune")
		}
	}
	if _, err := SplitMessage("AI", "Codex", body, limit-1); err == nil {
		t.Fatal("a budget unable to hold one rune with final counters must fail")
	}
}

func TestSplitMessageMinimumQuoteBudgetUsesActualRenderer(t *testing.T) {
	body := strings.Repeat("> line\n\n", 10)
	limit := len(Message("AI", "Codex · 第 99 / 共 99 段", ">"))
	parts := checkSegments(t, "AI", "Codex", body, limit)
	if parts[0].Raw != ">" {
		t.Fatal("minimum quote card should retain its first rune")
	}
}

func TestSplitMessageCompletedFileLinksFitActualByteBudget(t *testing.T) {
	var body strings.Builder
	for n := 0; n < 100; n++ {
		fmt.Fprintf(&body, "[file-%03d](/workspace/long/path/main.go:%d)\n\n", n, 1000+n)
	}
	parts := checkSegments(t, "AI", "Codex", body.String(), 800)
	if len(parts) < 2 {
		t.Fatal("many complete file links must split into bounded cards")
	}
}

func TestSplitMessageManyBlocksRetainMergedTail(t *testing.T) {
	var body strings.Builder
	for n := 0; n < 90; n++ {
		fmt.Fprintf(&body, "tail-marker-%03d\n\n", n)
	}
	parts := checkSegments(t, "AI", "Codex", body.String(), 0)
	if len(parts) != 1 {
		t.Fatal("short block collection should remain in one bounded card")
	}
	elements := humanSegmentElements(t, parts[0].Card)
	if len(elements) > 65 {
		t.Fatalf("native body exceeded 64 blocks plus footer: %d", len(elements))
	}
	var visible strings.Builder
	for _, entry := range elements[:len(elements)-1] {
		markdown, ok := entry.(map[string]any)
		if !ok || markdown["tag"] != "markdown" {
			t.Fatal("public body contains a non-Markdown element")
		}
		content, _ := markdown["content"].(string)
		visible.WriteString(content)
	}
	for n := 0; n < 90; n++ {
		if marker := fmt.Sprintf("tail-marker-%03d", n); strings.Count(visible.String(), marker) != 1 {
			t.Fatalf("merged tail lost or duplicated %s", marker)
		}
	}
}

func TestSplitMessageEightHundredCodeLines(t *testing.T) {
	body := "```go\n" + strings.Repeat("fmt.Println(\"中文😀\\\\\\\"\u2028\u2029\")\n", 850) + "```\n\nvisible final sentence"
	parts := checkSegments(t, "AI", "Codex", body, 900)
	if len(parts) < 10 || !strings.HasSuffix(parts[len(parts)-1].Body, "visible final sentence") {
		t.Fatal("long escaped code lost its final paragraph or failed to split")
	}
}
