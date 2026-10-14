package cards

import (
	"strings"
	"testing"
)

func TestSafeMarkdownKeepsQuoteSyntaxWithoutActivatingTags(t *testing.T) {
	input := "> **引用标题**\n> <at id=\"all\"></at>\n>\n> 第二段\n\n普通比较 2 > 1\n    > literal code\n&gt; already literal"
	got := SafeMarkdown(input)
	if !strings.HasPrefix(got, "> **引用标题**\n> &lt;at id=\"all\"&gt;&lt;/at&gt;\n>\n> 第二段") ||
		!strings.Contains(got, "普通比较 2 &gt; 1") || !strings.Contains(got, "    &gt; literal code") ||
		!strings.HasSuffix(got, "&gt; already literal") || strings.Contains(got, "<at") {
		t.Fatal("quote syntax or native-tag safety changed")
	}
	if Safe("> <at>") != "&gt; &lt;at&gt;" {
		t.Fatal("plain metadata safety changed with Markdown presentation")
	}
}

func TestSafeMarkdownPreservesLiteralCodeAndQuotedFenceLayers(t *testing.T) {
	input := "```text\n> literal\n[code](/not-read/example.go:22)\n<at id=\"all\">\n```\n\n> ```text\n> > literal inside quote\n> [code](/not-read/example.go:33)\n> ```\n> [outside](/not-read/example.go:44)"
	got := SafeMarkdown(input)
	if !strings.Contains(got, "\n&gt; literal\n") || !strings.Contains(got, "\n> &gt; literal inside quote\n") ||
		strings.Contains(got, "L22") || strings.Contains(got, "L33") || !strings.Contains(got, "[outside · L44](/not-read/example.go:44)") || strings.Contains(got, "<at") {
		t.Fatal("code acquired quote syntax, line decoration, or native tags")
	}
}

func TestCodeReferenceLabelsKeepTargetsAndLiteralExamples(t *testing.T) {
	input := "[对应代码](/not-read/file.go:1592) and [other](/not-read/file.go:23:4)\n" +
		"`[literal](/not-read/file.go:33)` and \\[escaped](/not-read/file.go:55)\n" +
		"![image](/not-read/file.png:66) [web](https://example.invalid/file:77) [already · L88](/not-read/file.go:88)"
	got := SafeMarkdown(input)
	if !strings.Contains(got, "[对应代码 · L1592](/not-read/file.go:1592)") || !strings.Contains(got, "[other · L23](/not-read/file.go:23:4)") ||
		strings.Contains(got, "L33") || strings.Contains(got, "L55") || strings.Contains(got, "L66") || strings.Contains(got, "L77") || strings.Count(got, "L88") != 1 {
		t.Fatal("file line decoration modified targets or literal examples")
	}
}

func TestMessageSeparatesParagraphsAndKeepsCompleteStructures(t *testing.T) {
	body := "开头\n\n1. 第一条\n\n   属于第一条的第二段\n\n2. 第二条\n\n" +
		"> **引用**\n>\n> 完整段落\n\n```text\n\n> literal\n\n```\n\n结束"
	elements := humanElements(t, Message("AI 回复", "Codex", body))
	if len(elements) != 6 {
		t.Fatal("paragraph spacing split a list, quote or code block")
	}
	contents := []string{"开头", "1. 第一条\n\n   属于第一条的第二段\n\n2. 第二条", "> **引用**\n>\n> 完整段落", "```text\n\n&gt; literal\n\n```", "结束"}
	for n, want := range contents {
		entry := elements[n].(map[string]any)
		if entry["tag"] != "markdown" || entry["content"] != want || entry["text_size"] != "normal" || entry["margin"] != "0px" {
			t.Fatalf("public block %d lost its text or typography", n)
		}
	}
	if elements[len(elements)-1].(map[string]any)["content"] != "> AI 回复 · Codex" {
		t.Fatal("source moved into the public reply")
	}
}

func TestPublicBodyBlockLimitKeepsTailAndEmptyMentionAnchor(t *testing.T) {
	var paragraphs []string
	for n := 0; n < 90; n++ {
		paragraphs = append(paragraphs, strings.Repeat("x", n+1))
	}
	body := strings.Join(paragraphs, "\n\n")
	blocks := publicBodyElements(body, "normal")
	if len(blocks) != 64 || !strings.HasSuffix(blocks[63]["content"].(string), paragraphs[89]) ||
		strings.Count(blocks[63]["content"].(string), "\n\n") != 26 {
		t.Fatal("component bound dropped the tail instead of merging it")
	}
	if empty := publicBodyElements("", "normal"); len(empty) != 1 || empty[0]["tag"] != "markdown" || empty[0]["content"] != "" {
		t.Fatal("empty body lost its leading Markdown marker/mention anchor")
	}
}

func TestFlatListSpacingPreservesExplicitAndAutomaticNumbering(t *testing.T) {
	for _, body := range []string{"1. first\n\n2. second\n\n3. third", "- first\n- second\n- third"} {
		blocks := publicBodyElements(body, "normal")
		if len(blocks) != 3 || !strings.Contains(blocks[0]["content"].(string), "first") || !strings.Contains(blocks[2]["content"].(string), "third") {
			t.Fatal("simple items did not receive native spacing")
		}
	}
	for _, body := range []string{"1. first\n1. automatic second", "1. first\n3. auto numbered second", "1. first\n   continued\n2. second", "- first\n  - nested"} {
		if blocks := publicBodyElements(body, "normal"); len(blocks) != 1 || blocks[0]["content"] != body {
			t.Fatal("list splitting changed automatic numbering or nested structure")
		}
	}
}
