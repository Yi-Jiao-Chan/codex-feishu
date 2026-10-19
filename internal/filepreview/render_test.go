package filepreview

import (
	"bytes"
	"encoding/xml"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

type renderedNode struct {
	tag      string
	attrs    map[string]string
	text     strings.Builder
	children []*renderedNode
}

func renderedPage(t *testing.T, name, content string) *renderedNode {
	t.Helper()
	body, err := renderPreview(previewFile{name: name, content: []byte(content), modified: time.Unix(1800000000, 0)})
	if err != nil {
		t.Fatal("render failed", err)
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = false
	decoder.AutoClose = xml.HTMLAutoClose
	decoder.Entity = xml.HTMLEntity
	root := &renderedNode{}
	stack := []*renderedNode{root}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("rendered page could not be parsed", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			node := &renderedNode{tag: strings.ToLower(token.Name.Local), attrs: map[string]string{}}
			for _, attr := range token.Attr {
				node.attrs[strings.ToLower(attr.Name.Local)] = attr.Value
			}
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, node)
			stack = append(stack, node)
		case xml.EndElement:
			if len(stack) <= 1 {
				t.Fatal("rendered page had unbalanced elements")
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			for _, node := range stack {
				node.text.Write(token)
			}
		}
	}
	if len(stack) != 1 {
		t.Fatal("rendered page had unclosed elements")
	}
	return root
}

func renderedNodes(root *renderedNode, match func(*renderedNode) bool) []*renderedNode {
	var found []*renderedNode
	if match(root) {
		found = append(found, root)
	}
	for _, child := range root.children {
		found = append(found, renderedNodes(child, match)...)
	}
	return found
}

func renderedTags(root *renderedNode, tag string) []*renderedNode {
	return renderedNodes(root, func(node *renderedNode) bool { return node.tag == tag })
}

func renderedHasClass(node *renderedNode, class string) bool {
	for _, value := range strings.Fields(node.attrs["class"]) {
		if value == class {
			return true
		}
	}
	return false
}

func renderedTokenSpans(root *renderedNode) []*renderedNode {
	var found []*renderedNode
	var visit func(*renderedNode, bool)
	visit = func(node *renderedNode, inCode bool) {
		inCode = inCode || node.tag == "code" || renderedHasClass(node, "code-line")
		if inCode && node.tag == "span" && node.attrs["class"] != "" && !renderedHasClass(node, "code-line") {
			found = append(found, node)
		}
		for _, child := range node.children {
			visit(child, inCode)
		}
	}
	visit(root, false)
	return found
}

func assertRenderedHighlight(t *testing.T, page, code *renderedNode) {
	t.Helper()
	tokens := renderedTokenSpans(code)
	if len(tokens) == 0 {
		t.Fatal("recognized language has no highlighted token spans")
	}
	for _, style := range renderedTags(page, "style") {
		for _, token := range tokens {
			for _, class := range strings.Fields(token.attrs["class"]) {
				if strings.Contains(style.text.String(), "."+class) {
					return
				}
			}
		}
	}
	t.Fatal("highlighted tokens have no embedded stylesheet")
}

func assertRenderedSourceLines(t *testing.T, page *renderedNode, want []string) []*renderedNode {
	t.Helper()
	lines := renderedNodes(page, func(node *renderedNode) bool { return renderedHasClass(node, "code-line") })
	if len(lines) != len(want) {
		t.Fatalf("source has %d lines, want %d", len(lines), len(want))
	}
	for i, line := range lines {
		n := strconv.Itoa(i + 1)
		if line.attrs["id"] != "L"+n {
			t.Fatalf("incorrect source anchor on line %d", i+1)
		}
		anchors := renderedTags(line, "a")
		if len(anchors) != 1 || !renderedHasClass(anchors[0], "line-no") || anchors[0].attrs["href"] != "#L"+n || anchors[0].text.String() != n {
			t.Fatalf("incorrect line-number link on line %d", i+1)
		}
		if strings.TrimSuffix(line.text.String(), "\n") != n+want[i] {
			t.Fatalf("source text changed on line %d", i+1)
		}
		ids := renderedNodes(page, func(node *renderedNode) bool { return node.attrs["id"] == "L"+n })
		if len(ids) != 1 {
			t.Fatalf("duplicate source anchor on line %d", i+1)
		}
	}
	return lines
}

func assertNoRenderedActiveContent(t *testing.T, page *renderedNode) {
	t.Helper()
	for _, node := range renderedNodes(page, func(*renderedNode) bool { return true }) {
		switch node.tag {
		case "script", "img", "iframe", "object", "embed", "link":
			t.Fatalf("rendered page contains active or external-resource element %s", node.tag)
		}
		for name, value := range node.attrs {
			if strings.HasPrefix(name, "on") {
				t.Fatal("rendered page contains an event handler")
			}
			if name == "href" || name == "src" {
				lower := strings.ToLower(strings.TrimSpace(value))
				if strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "data:") || strings.HasPrefix(lower, "vbscript:") {
					t.Fatal("rendered page contains an executable URL")
				}
			}
		}
	}
}

func TestRenderPreviewHighlightsCommonCodeFiles(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"main.go", "package fixture\n\nfunc main() { println(42) }\n"},
		{"sample.json", "{\n  \"enabled\": true,\n  \"count\": 42\n}\n"},
		{"main.py", "def greet(name):\n    return f\"hello {name}\"\n"},
		{"main.ts", "const count: number = 42;\nexport function value(): number { return count; }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := renderedPage(t, tc.name, tc.content)
			assertRenderedHighlight(t, page, page)
			assertRenderedSourceLines(t, page, strings.Split(strings.TrimSuffix(tc.content, "\n"), "\n"))
			assertNoRenderedActiveContent(t, page)
		})
	}
}

func TestRenderPreviewMultilineCommentsKeepSourceAnchors(t *testing.T) {
	content := "package fixture\n/* first comment\nsecond comment */\nvar total = 2\n"
	page := renderedPage(t, "main.go", content)
	lines := assertRenderedSourceLines(t, page, strings.Split(strings.TrimSuffix(content, "\n"), "\n"))
	first, second := renderedTokenSpans(lines[1]), renderedTokenSpans(lines[2])
	if len(first) == 0 || len(second) == 0 || first[0].attrs["class"] != second[0].attrs["class"] {
		t.Fatal("multiline comment highlighting lost its token state across lines")
	}
}

func TestRenderPreviewMarkdownReadsGFMBeforeCollapsedSource(t *testing.T) {
	content := "# Fixture report\n\n## Examples\n\n- first item\n- second item\n\n- [x] finished\n- [ ] pending\n\n" +
		"| Name | Value |\n| --- | --- |\n| count | 42 |\n\nUse `value()` and **strong text**.\n"
	for _, ext := range []string{".md", ".markdown", ".mdown"} {
		t.Run(ext, func(t *testing.T) {
			page := renderedPage(t, "report"+ext, content)
			articles := renderedTags(page, "article")
			if len(articles) != 1 {
				t.Fatal("Markdown has no reading article")
			}
			article := articles[0]
			for _, tag := range []string{"h1", "h2", "ul", "li", "table", "thead", "td", "strong"} {
				if len(renderedTags(article, tag)) == 0 {
					t.Fatalf("GFM reading view is missing %s", tag)
				}
			}
			codes := renderedTags(article, "code")
			if len(codes) != 1 || codes[0].text.String() != "value()" {
				t.Fatal("inline code was not rendered as code")
			}
			inputs := renderedTags(article, "input")
			if len(inputs) != 2 {
				t.Fatal("GFM task list was not rendered")
			}
			for _, input := range inputs {
				if _, disabled := input.attrs["disabled"]; input.attrs["type"] != "checkbox" || !disabled {
					t.Fatal("GFM task checkbox is not read-only")
				}
			}
			if _, checked := inputs[0].attrs["checked"]; !checked {
				t.Fatal("completed task state was lost")
			}
			details := renderedTags(page, "details")
			if len(details) != 1 {
				t.Fatal("Markdown source has no disclosure")
			}
			if _, open := details[0].attrs["open"]; open {
				t.Fatal("Markdown source is expanded by default")
			}
			if len(renderedTags(details[0], "article")) != 0 {
				t.Fatal("Markdown reading article is hidden inside the source disclosure")
			}
			assertRenderedSourceLines(t, details[0], strings.Split(strings.TrimSuffix(content, "\n"), "\n"))
			assertNoRenderedActiveContent(t, page)
		})
	}
}

func TestRenderPreviewHighlightsMarkdownFences(t *testing.T) {
	for _, tc := range []struct{ language, code string }{
		{"go", "package fixture\nvar count = 42"},
		{"json", "{\"enabled\": true}"},
		{"python", "def value():\n    return 42"},
		{"typescript", "const count: number = 42;"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			content := "# Examples\n\n```" + tc.language + "\n" + tc.code + "\n```\n"
			page := renderedPage(t, "report.md", content)
			articles := renderedTags(page, "article")
			if len(articles) != 1 {
				t.Fatal("Markdown fence has no reading article")
			}
			assertRenderedHighlight(t, page, articles[0])
			codes := renderedTags(articles[0], "code")
			if len(codes) != 1 || strings.TrimSuffix(codes[0].text.String(), "\n") != tc.code {
				t.Fatal("highlighting changed fenced code text")
			}
			assertRenderedSourceLines(t, page, strings.Split(strings.TrimSuffix(content, "\n"), "\n"))
			assertNoRenderedActiveContent(t, page)
		})
	}
}

func TestRenderPreviewFenceMetadataUsesOnlyBoundedLanguageNames(t *testing.T) {
	code := "package fixture\n// <script>alert(1)</script>\nvar tag = \"<img src=x onerror=alert(1)>\""
	for _, tc := range []struct{ name, language string }{
		{"filename", "main.go"},
		{"long-info", strings.Repeat("a", 65) + ".go"},
		{"long-untrusted-info", strings.Repeat("a", 65) + `"><img/src=x/onerror=alert(1)>.go`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := "# Plain fence\n\n```" + tc.language + "\n" + code + "\n```\n"
			page := renderedPage(t, "report.md", content)
			articles := renderedTags(page, "article")
			if len(articles) != 1 {
				t.Fatal("unsupported fence metadata lost its reading article")
			}
			codes := renderedTags(articles[0], "code")
			if len(codes) != 1 || strings.TrimSuffix(codes[0].text.String(), "\n") != code {
				t.Fatal("plain fallback changed or truncated fenced source")
			}
			if len(renderedTokenSpans(codes[0])) != 0 {
				t.Fatal("fence metadata was inferred as a filename or accepted beyond the language length limit")
			}
			if len(tc.language) > 64 && codes[0].attrs["class"] != "" {
				t.Fatal("oversized fence metadata was copied to an HTML class")
			}
			assertRenderedSourceLines(t, page, strings.Split(strings.TrimSuffix(content, "\n"), "\n"))
			assertNoRenderedActiveContent(t, page)
		})
	}
}

func TestRenderPreviewUntrustedSourceAndFenceMetadataStayInert(t *testing.T) {
	payload := `<script>alert("fixture")</script>` + "\n" + `<img src=x onerror=alert(1)>`
	for _, name := range []string{"notes.txt", `source"><img src=x onerror=alert(1)>.ts`} {
		page := renderedPage(t, name, payload)
		assertRenderedSourceLines(t, page, strings.Split(payload, "\n"))
		assertNoRenderedActiveContent(t, page)
		if name == "notes.txt" && len(renderedTokenSpans(page)) != 0 {
			t.Fatal("unrecognized text file was not rendered as plain text")
		}
	}
	for _, language := range []string{"javascript", "codex-unlisted-language", `go"><img/src=x/onerror=alert(1)>`} {
		content := "# Safe reading\n\n<script>alert('raw')</script>\n\n![image](https://tracker.invalid/pixel)\n\n```" + language + "\n" + payload + "\n```\n"
		page := renderedPage(t, "report.md", content)
		assertNoRenderedActiveContent(t, page)
		articles := renderedTags(page, "article")
		if len(articles) != 1 {
			t.Fatal("untrusted Markdown has no reading article")
		}
		codes := renderedTags(articles[0], "code")
		if len(codes) != 1 || strings.TrimSuffix(codes[0].text.String(), "\n") != payload {
			t.Fatal("escaping changed fenced source text")
		}
		if language == "codex-unlisted-language" && len(renderedTokenSpans(articles[0])) != 0 {
			t.Fatal("unknown fenced language was not rendered as plain text")
		}
	}
}

func TestRenderPreviewEmptyAndCRLFSource(t *testing.T) {
	for _, name := range []string{"empty.go", "empty.md", "empty.txt"} {
		t.Run(name, func(t *testing.T) {
			page := renderedPage(t, name, "")
			assertRenderedSourceLines(t, page, []string{""})
			if name == "empty.md" && len(renderedTags(page, "article")) != 1 {
				t.Fatal("empty Markdown lost its reading view")
			}
		})
	}
	page := renderedPage(t, "main.go", "package fixture\r\n\r\n/* first comment\r\nsecond comment */\r\n")
	assertRenderedSourceLines(t, page, []string{"package fixture", "", "/* first comment", "second comment */"})
	assertRenderedHighlight(t, page, page)
}

func TestRenderPreviewLargeContentFallsBackWithoutTruncation(t *testing.T) {
	content := "package fixture\n// " + strings.Repeat("x", 256<<10) + "\n"
	page := renderedPage(t, "main.go", content)
	assertRenderedSourceLines(t, page, strings.Split(strings.TrimSuffix(content, "\n"), "\n"))
	if len(renderedTokenSpans(page)) != 0 {
		t.Fatal("oversized source did not fall back to plain text")
	}
	code := "package fixture\n// " + strings.Repeat("x", 140<<10)
	markdown := "# Large fences\n\n```go\n" + code + "\n```\n\n```go\n" + code + "\n```\n"
	page = renderedPage(t, "large.md", markdown)
	articles := renderedTags(page, "article")
	if len(articles) != 1 {
		t.Fatal("large Markdown lost its reading article")
	}
	codes := renderedTags(articles[0], "code")
	if len(codes) != 2 {
		t.Fatal("large Markdown lost a fenced block")
	}
	for i, block := range codes {
		if strings.TrimSuffix(block.text.String(), "\n") != code {
			t.Fatalf("large fenced block %d was truncated", i+1)
		}
	}
	assertRenderedHighlight(t, page, codes[0])
	if len(renderedTokenSpans(codes[1])) != 0 {
		t.Fatal("cumulative Markdown highlight budget did not fall back to plain text")
	}
	assertRenderedSourceLines(t, page, strings.Split(strings.TrimSuffix(markdown, "\n"), "\n"))
	assertNoRenderedActiveContent(t, page)
}
