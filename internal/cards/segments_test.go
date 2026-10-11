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
				Elements []struct{ Content string }
			}
		}
		if err := json.Unmarshal([]byte(part.Card), &card); err != nil {
			t.Fatal(err)
		}
		if len(card.Body.Elements) != 2 || card.Body.Elements[0].Content != Safe(part.Body) {
			t.Fatalf("segment %d body does not use existing safe rendering", i)
		}
		footer := "> " + Safe(role+" · "+source)
		if len(parts) > 1 {
			footer += fmt.Sprintf(" · 第 %d / 共 %d 段", i+1, len(parts))
		}
		if card.Body.Elements[1].Content != footer {
			t.Fatalf("segment %d incorrect footer: %q", i, card.Body.Elements[1].Content)
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

func TestMarkdownJSONBudgetMatchesRenderer(t *testing.T) {
	// Exercise all ASCII controls, HTML safety expansion and JSON's special
	// Unicode escapes against the real serializer, rather than a size estimate.
	var text strings.Builder
	for r := rune(0); r < 128; r++ {
		text.WriteRune(r)
	}
	text.WriteString("中文😀\u2028\u2029")
	body := text.String()
	got := len(Message("AI", "Codex", body)) - len(Message("AI", "Codex", ""))
	if want := markdownJSONBytes(body); got != want {
		t.Fatalf("encoded byte budget %d does not match serializer %d", want, got)
	}
}
