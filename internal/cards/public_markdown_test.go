package cards

import (
	"strings"
	"testing"
)

func TestSafeMarkdownKeepsQuoteSyntaxWithoutActivatingTags(t *testing.T) {
	input := "> **引用标题**\n> <at id=\"all\"></at>\n>\n> 第二段\n\n普通比较 2 > 1\n    > literal code\n&gt; already literal"
	got := SafeMarkdown(input)
	if !strings.HasPrefix(got, "> **引用标题**<br>&lt;at id=\"all\"&gt;&lt;/at&gt;<br>第二段") ||
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

func TestSafeMarkdownDowngradesParsedImagesWithoutExposingLocalTargets(t *testing.T) {
	input := "web ![preview](https://example.invalid/image.png \"title\")\n" +
		"local ![secret](/private/work/review.png) relative ![draft](images/review.png)\n" +
		"file ![file](file:///private/work/review.png) data ![inline](data:image/png;base64,private)"
	got := SafeMarkdown(input)
	if !strings.Contains(got, "web [preview](https://example.invalid/image.png \"title\")") ||
		strings.Count(got, "图片请回 Codex 查看") != 4 || strings.Contains(got, "![") ||
		strings.Contains(got, "/private/") || strings.Contains(got, "images/review.png") || strings.Contains(got, "base64") {
		t.Fatalf("unsafe image destination reached public markdown: %q", got)
	}
}

func TestSafeMarkdownKeepsLiteralImageExamplesAndHandlesReferences(t *testing.T) {
	input := "`![inline](/private/inline.png)`\n\n" +
		"```md\n![fenced](/private/fenced.png)\n```\n\n" +
		"\\![escaped](/private/escaped.png)\n\n" +
		"![remote][web] and ![local][disk] and [ordinary][disk]\n\n" +
		"[web]: https://example.invalid/image.png\n[disk]: /private/reference.png\n"
	got := SafeMarkdown(input)
	for _, literal := range []string{"`![inline](/private/inline.png)`", "![fenced](/private/fenced.png)", `\![escaped](/private/escaped.png)`} {
		if !strings.Contains(got, literal) {
			t.Fatalf("literal image example changed: %q", got)
		}
	}
	if !strings.Contains(got, "[remote][web]") || !strings.Contains(got, "[web]: https://example.invalid/image.png") ||
		!strings.Contains(got, "图片请回 Codex 查看") || strings.Contains(got, "/private/reference.png") ||
		strings.Contains(got, "![remote]") || strings.Contains(got, "![local]") {
		t.Fatalf("reference image downgrade was unsafe: %q", got)
	}
	card := Message("AI 回复", "Codex", "![local][disk]\n\n[disk]: /private/card-reference.png")
	if strings.Contains(card, "![local]") || strings.Contains(card, "/private/card-reference.png") || !strings.Contains(card, "图片请回 Codex 查看") {
		t.Fatalf("block splitting bypassed reference image safety: %s", card)
	}
}

func TestSafeMarkdownDoesNotCreateNestedLinksFromLinkedImages(t *testing.T) {
	input := "[![preview](https://example.invalid/image.png)](https://example.invalid/report)"
	if got := SafeMarkdown(input); got != "[查看图片](https://example.invalid/report)" {
		t.Fatalf("linked image became nested markdown links: %q", got)
	}
}

func TestSplitMessageKeepsUnsafeImageReplacementAtomic(t *testing.T) {
	limit := len(Message("AI 回复", "Codex · 第 9 / 共 9 段", strings.Repeat("x", 700)))
	for name, body := range map[string]string{
		"inline":    strings.Repeat("a", 900) + " ![preview](/private/" + strings.Repeat("directory-", 180) + "review.png) " + strings.Repeat("b", 900),
		"reference": "![preview][disk]\n\n" + strings.Repeat("a", 1800) + "\n\n[disk]: /private/" + strings.Repeat("reference-", 180) + "review.png\n",
	} {
		t.Run(name, func(t *testing.T) {
			parts, err := SplitMessage("AI 回复", "Codex", body, limit)
			if err != nil || len(parts) < 2 {
				t.Fatalf("long image fixture was not split safely: parts=%d err=%v", len(parts), err)
			}
			var raw strings.Builder
			for _, part := range parts {
				raw.WriteString(part.Raw)
				if strings.Contains(part.Card, "/private/") || strings.Contains(part.Card, "directory-") ||
					strings.Contains(part.Card, "reference-") || strings.Contains(part.Card, "![preview]") {
					t.Fatalf("a segment exposed partial image syntax or its local target: %s", part.Card)
				}
			}
			if raw.String() != body {
				t.Fatal("safe display splitting changed the original Raw bytes")
			}
		})
	}
}

func TestMessageSeparatesParagraphsAndKeepsCompleteStructures(t *testing.T) {
	body := "开头\n\n1. 第一条\n\n   属于第一条的第二段\n\n2. 第二条\n\n" +
		"> **引用**\n>\n> 完整段落\n\n```text\n\n> literal\n\n```\n\n结束"
	elements := humanElements(t, Message("AI 回复", "Codex", body))
	if len(elements) != 6 {
		t.Fatal("paragraph spacing split a list, quote or code block")
	}
	contents := []string{"开头", "1. 第一条\n\n   属于第一条的第二段\n\n2. 第二条", "> **引用**<br>完整段落", "```text\n\n&gt; literal\n\n```", "结束"}
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

func TestSimpleQuoteKeepsCompactLineBreaksAndSafeInlineFormatting(t *testing.T) {
	input := "> **验证 · 已进入**  \n>\n> 本轮保留 `34` 条检查。\\\n>\n> [查看任务](/not-read/task.go:23)\n\n正文"
	want := "> **验证 · 已进入**<br>本轮保留 `34` 条检查。<br>[查看任务 · L23](/not-read/task.go:23)\n\n正文"
	if got := SafeMarkdown(input); got != want {
		t.Fatalf("quote title, body and link lost compact hard breaks: %q", got)
	}
	input = "开头\n\n  > first <at id=\"all\"></at>\r\n  > <br> and 2 > 1\r\n\n尾部"
	got := SafeMarkdown(input)
	if !strings.Contains(got, "  > first &lt;at id=\"all\"&gt;&lt;/at&gt;<br>&lt;br&gt; and 2 &gt; 1\n") || strings.Count(got, "<br>") != 1 || strings.Contains(got, "<at") {
		t.Fatal("quote hard breaks activated input markup or lost surrounding paragraphs")
	}
}

func TestSimpleQuoteDoesNotFlattenComplexChildrenOrLiteralCode(t *testing.T) {
	for _, body := range []string{
		"> first\n>> nested\n> third",
		"> first\n> - child\n> second",
		"> first\n>     code\n> second",
		"> first\n> # heading\n> second",
		"> first\n> | table |\n> second",
		"> first\n> ---\n> second",
		"> first `\n> code continuation`\n> third",
		"```text\n> first\n> second\n```",
	} {
		if got := SafeMarkdown(body); strings.Contains(got, "<br>") {
			t.Fatalf("complex quote or literal code gained line-break tags: %q", got)
		}
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
