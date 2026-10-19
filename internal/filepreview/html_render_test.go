package filepreview

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func htmlElements(root *html.Node, tag string) []*html.Node {
	var found []*html.Node
	if root.Type == html.ElementNode && strings.EqualFold(root.Data, tag) {
		found = append(found, root)
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		found = append(found, htmlElements(child, tag)...)
	}
	return found
}

func htmlAttribute(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val
		}
	}
	return ""
}

func htmlNodeText(node *html.Node) string {
	var text strings.Builder
	if node.Type == html.TextNode {
		text.WriteString(node.Data)
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		text.WriteString(htmlNodeText(child))
	}
	return text.String()
}

func assertChildDNSPolicy(t *testing.T, document *html.Node) {
	t.Helper()
	heads := htmlElements(document, "head")
	if len(heads) != 1 || heads[0].FirstChild == nil {
		t.Fatal("static HTML has no head policy")
	}
	first := heads[0].FirstChild
	if first.Type != html.ElementNode || first.Data != "meta" || htmlAttribute(first, "http-equiv") != "x-dns-prefetch-control" || htmlAttribute(first, "content") != "off" {
		t.Fatal("DNS prefetch off policy is not the first head node")
	}
	policies := 0
	for _, meta := range htmlElements(document, "meta") {
		if strings.EqualFold(strings.TrimSpace(htmlAttribute(meta, "http-equiv")), "x-dns-prefetch-control") {
			policies++
			if htmlAttribute(meta, "content") != "off" {
				t.Fatal("user HTML re-enabled DNS prefetch")
			}
		}
	}
	if policies != 1 {
		t.Fatal("static HTML retained a user DNS policy")
	}
}

func assertRenderedHTMLFrame(t *testing.T, page *renderedNode, name string) *html.Node {
	t.Helper()
	frames := renderedTags(page, "iframe")
	if len(frames) != 1 {
		t.Fatalf("HTML preview has %d frames, want 1", len(frames))
	}
	frame := frames[0]
	source, present := frame.attrs["srcdoc"]
	if !present || source == "" {
		t.Fatal("HTML preview has no child document")
	}
	if sandbox, present := frame.attrs["sandbox"]; !present || sandbox != "" {
		t.Fatal("HTML preview does not have an empty sandbox")
	}
	if frame.attrs["title"] != name+" 页面预览" {
		t.Fatal("HTML preview frame has no descriptive title")
	}
	if !strings.Contains(page.text.String(), "HTML 静态预览") {
		t.Fatal("HTML preview has no static preview label")
	}
	if _, present := frame.attrs["src"]; present {
		t.Fatal("HTML preview frame has a navigation source")
	}
	for _, node := range renderedNodes(page, func(*renderedNode) bool { return true }) {
		switch node.tag {
		case "script", "img", "object", "embed":
			t.Fatal("original HTML escaped into an outer active element")
		}
		for attr := range node.attrs {
			if strings.HasPrefix(attr, "on") || node == frame && strings.HasPrefix(attr, "allow") {
				t.Fatal("HTML preview acquired an outer event handler or frame permission")
			}
		}
	}
	if len(frame.children) != 0 {
		t.Fatal("original HTML escaped into iframe fallback elements")
	}
	document, err := html.ParseWithOptions(strings.NewReader(source), html.ParseOptionEnableScripting(false))
	if err != nil {
		t.Fatal("static HTML child document could not be parsed", err)
	}
	assertChildDNSPolicy(t, document)
	return document
}

func assertRenderedHTMLSource(t *testing.T, page *renderedNode, content string, wantOpen bool) *renderedNode {
	t.Helper()
	details := renderedTags(page, "details")
	if len(details) != 1 {
		t.Fatal("HTML source has no disclosure")
	}
	if _, open := details[0].attrs["open"]; open != wantOpen {
		t.Fatal("HTML source disclosure has the wrong open state")
	}
	summaries := renderedTags(details[0], "summary")
	if len(summaries) != 1 || summaries[0].text.String() != "查看 HTML 源码 · 含行号" {
		t.Fatal("HTML source disclosure has no descriptive summary")
	}
	if len(renderedTags(details[0], "iframe")) != 0 {
		t.Fatal("HTML preview is hidden inside the source disclosure")
	}
	assertRenderedSourceLines(t, details[0], strings.Split(strings.TrimSuffix(content, "\n"), "\n"))
	return details[0]
}

func TestRenderPreviewHTMLDocumentAndSource(t *testing.T) {
	content := "<!doctype html>\n<html><head><title>Fixture &amp; demo</title></head>\n" +
		`<body><h1 title="'quoted'">Hello &amp; goodbye</h1></body></html>` + "\n"
	for _, name := range []string{"report.html", "report.htm", "report.xhtml", "report.HTML", "report.HtM", "report.XhTmL"} {
		t.Run(name, func(t *testing.T) {
			page := renderedPage(t, name, content)
			document := assertRenderedHTMLFrame(t, page, name)
			titles, headings := htmlElements(document, "title"), htmlElements(document, "h1")
			if len(titles) != 1 || htmlNodeText(titles[0]) != "Fixture & demo" || len(headings) != 1 || htmlNodeText(headings[0]) != "Hello & goodbye" || htmlAttribute(headings[0], "title") != "'quoted'" {
				t.Fatal("static HTML changed document text, entities or quoted attributes")
			}
			source := assertRenderedHTMLSource(t, page, content, false)
			assertRenderedHighlight(t, page, source)
		})
	}
}

func TestRenderPreviewHTMLClosingTagsAndQuotesStayInSrcdoc(t *testing.T) {
	content := "<!doctype html>\n" +
		`</iframe><script>window.fixture = "</iframe>";</script>` + "\n" +
		`<img src="https://tracker.invalid/pixel" onerror="alert('fixture')">` + "\n" +
		`<p title="'quoted' &amp; text">&lt;literal&gt; & raw text</p>` + "\n"
	name := `report"><img src=x onerror=alert(1)>.html`
	page := renderedPage(t, name, content)
	document := assertRenderedHTMLFrame(t, page, name)
	scripts, paragraphs := htmlElements(document, "script"), htmlElements(document, "p")
	if len(scripts) != 1 || htmlNodeText(scripts[0]) != `window.fixture = "</iframe>";` || len(paragraphs) != 1 || htmlNodeText(paragraphs[0]) != "<literal> & raw text" || htmlAttribute(paragraphs[0], "title") != "'quoted' & text" {
		t.Fatal("static HTML changed script text, entities or quoted attributes")
	}
	assertRenderedHTMLSource(t, page, content, false)
}

func TestRenderPreviewHTMLDirectoryLinksStayInChildDocument(t *testing.T) {
	content := `<html><head><base href="https://outside.invalid/" target="_top"></head><body>
<a id="ordinary" href="#chapter">Chapter</a>
<a id="unicode" href="#中文标题">Unicode</a>
<a id="encoded" href="#%E4%B8%AD%E6%96%87">Encoded</a>
<a id="top" href="#">Top</a>
<a id="space" href="  #chapter  ">Whitespace</a>
<a id="explicit-top" href="#chapter" target="_top">Top target</a>
<a id="explicit-blank" href="#chapter" target="_blank">Blank target</a>
<map><area id="area" href="#chapter"></map>
<svg><a id="svg" xlink:href="#chapter"><text>SVG</text></a></svg>
<a id="relative" href="other.html#chapter" target="_blank">Other file</a>
<a id="remote" href="https://outside.invalid/#chapter" target="_blank">Remote</a>
<a id="script" href="javascript:alert(1)">Script</a>
<h2 id="chapter">Chapter</h2></body></html>`
	page := renderedPage(t, "directory.html", content)
	document := assertRenderedHTMLFrame(t, page, "directory.html")
	want := map[string][2]string{
		"ordinary":       {"about:srcdoc#chapter", "_self"},
		"unicode":        {"about:srcdoc#中文标题", "_self"},
		"encoded":        {"about:srcdoc#%E4%B8%AD%E6%96%87", "_self"},
		"top":            {"about:srcdoc#", "_self"},
		"space":          {"about:srcdoc#chapter", "_self"},
		"explicit-top":   {"about:srcdoc#chapter", "_self"},
		"explicit-blank": {"about:srcdoc#chapter", "_self"},
		"area":           {"about:srcdoc#chapter", "_self"},
		"svg":            {"about:srcdoc#chapter", "_self"},
		"relative":       {"other.html#chapter", "_blank"},
		"remote":         {"https://outside.invalid/#chapter", "_blank"},
		"script":         {"javascript:alert(1)", ""},
	}
	for _, node := range append(htmlElements(document, "a"), htmlElements(document, "area")...) {
		id := htmlAttribute(node, "id")
		expected, ok := want[id]
		if !ok || htmlAttribute(node, "href") != expected[0] || htmlAttribute(node, "target") != expected[1] {
			t.Fatalf("directory link %q does not keep the expected document and target", id)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatal("HTML preview lost directory links")
	}
	assertRenderedHTMLSource(t, page, content, false)
}

func TestRenderPreviewEmptyHTMLKeepsFrameAndSource(t *testing.T) {
	page := renderedPage(t, "empty.html", "")
	assertRenderedHTMLFrame(t, page, "empty.html")
	assertRenderedHTMLSource(t, page, "", false)
}

func TestRenderPreviewHTMLRemovesHintsAndNestedContexts(t *testing.T) {
	for _, tc := range []struct{ name, content, retainedText, container, namespace string }{
		{"resource-hints", `<head><LINK REL="preconnect DNS-PREFETCH" HREF="https://hint.invalid/"><link rel="prefetch preload stylesheet" href="https://hint.invalid/asset"></head><body><p>retained body</p></body>`, "retained body", "", ""},
		{"nested-srcdoc", `<body><iframe srcdoc="&lt;iframe srcdoc='&amp;lt;link rel=preconnect href=https://hint.invalid/&amp;gt;'&gt;&lt;/iframe&gt;">nested fallback to remove</iframe><p>outer body</p></body>`, "outer body", "", ""},
		{"frameset", `<html><frameset><frame src="https://hint.invalid/child"></frameset></html>`, "", "", ""},
		{"template", `<template><link rel="preconnect" href="https://hint.invalid/"><p>template text survives</p></template>`, "template text survives", "template", ""},
		{"svg", `<svg><link rel="preconnect" href="https://hint.invalid/"/><text>svg text survives</text></svg>`, "svg text survives", "svg", "svg"},
		{"mathml", `<math><link rel="preconnect" href="https://hint.invalid/"/><mtext>math text survives</mtext></math>`, "math text survives", "math", "math"},
		{"user-dns-on", `<head><meta HTTP-EQUIV=" X-DNS-PREFETCH-CONTROL " content="on"><meta http-equiv="x-dns-prefetch-control" content="on"></head><body><template><meta http-equiv="x-dns-prefetch-control" content="on"></template><p>policy body survives</p></body>`, "policy body survives", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.container != "" {
				raw, err := html.ParseWithOptions(strings.NewReader(tc.content), html.ParseOptionEnableScripting(false))
				if err != nil || len(htmlElements(raw, tc.container)) == 0 || len(htmlElements(raw, "link")) == 0 {
					t.Fatal("fixture did not create the intended nested link")
				}
				if tc.namespace != "" && htmlElements(raw, "link")[0].Namespace != tc.namespace {
					t.Fatal("fixture did not create the intended foreign link")
				}
			}
			page := renderedPage(t, "report.html", tc.content)
			document := assertRenderedHTMLFrame(t, page, "report.html")
			for _, tag := range []string{"link", "iframe", "frame"} {
				if len(htmlElements(document, tag)) != 0 {
					t.Fatalf("static HTML retained a %s element", tag)
				}
			}
			if !strings.Contains(htmlNodeText(document), tc.retainedText) || strings.Contains(htmlNodeText(document), "nested fallback to remove") {
				t.Fatal("static HTML removed unrelated content or retained a nested context subtree")
			}
			assertRenderedHTMLSource(t, page, tc.content, false)
		})
	}
}

func TestRenderPreviewHTMLNoscriptKeepsFallbackAndLiteralMarkup(t *testing.T) {
	for _, tc := range []struct{ name, content, expectedText string }{
		{"literal-markup", `<body><noscript>&lt;link rel="preconnect" href="https://hint.invalid/"&gt;&lt;iframe srcdoc="child"&gt;&lt;/iframe&gt;</noscript></body>`, `<link rel="preconnect" href="https://hint.invalid/"><iframe srcdoc="child"></iframe>`},
		{"ordinary-fallback", `<body><noscript><p id="fallback">JavaScript disabled fallback &amp; text</p></noscript></body>`, "JavaScript disabled fallback & text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := renderedPage(t, "report.html", tc.content)
			document := assertRenderedHTMLFrame(t, page, "report.html")
			for _, tag := range []string{"noscript", "link", "iframe"} {
				if len(htmlElements(document, tag)) != 0 {
					t.Fatalf("static noscript fallback materialized a %s element", tag)
				}
			}
			if !strings.Contains(htmlNodeText(document), tc.expectedText) {
				t.Fatal("static HTML lost noscript fallback text or changed its literal markup")
			}
			if tc.name == "ordinary-fallback" {
				paragraphs := htmlElements(document, "p")
				if len(paragraphs) != 1 || htmlAttribute(paragraphs[0], "id") != "fallback" {
					t.Fatal("ordinary noscript fallback lost its element structure")
				}
			}
			assertRenderedHTMLSource(t, page, tc.content, false)
		})
	}
}

func TestRenderPreviewHTMLParseFailureKeepsCompleteOpenSource(t *testing.T) {
	content := strings.Repeat("<div>", 600) + "deep fixture" + strings.Repeat("</div>", 600)
	page := renderedPage(t, "deep.html", content)
	if len(renderedTags(page, "iframe")) != 0 {
		t.Fatal("failed HTML parsing still created a preview frame")
	}
	if !strings.Contains(page.text.String(), "无法生成 HTML 页面预览") {
		t.Fatal("failed HTML parsing did not explain the source fallback")
	}
	assertRenderedHTMLSource(t, page, content, true)
	assertNoRenderedActiveContent(t, page)
}

func TestHTMLPreviewServiceRequiresAuthentication(t *testing.T) {
	for _, name := range []string{"report.html", "report.HTM", "report.xhtml"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			path := writeFixture(t, f.root, name, "<!doctype html>\n<h1>service-html-fixture</h1>\n")
			target := registeredURL(t, f.s, path)
			w := request(t, f.s, http.MethodGet, target, nil)
			if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "<iframe") || strings.Contains(w.Body.String(), "service-html-fixture") {
				t.Fatal("unauthenticated HTML preview exposed a frame or document")
			}
			w = request(t, f.s, http.MethodGet, target, loggedIn(t, f.s))
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<iframe") || !strings.Contains(w.Body.String(), "srcdoc=") || !strings.Contains(w.Body.String(), "service-html-fixture") {
				t.Fatal("authenticated HTML preview did not render a registered document")
			}
			if w.Header().Get("X-DNS-Prefetch-Control") != "off" {
				t.Fatal("HTML preview response did not disable DNS prefetch")
			}
			csp := map[string]string{}
			for _, directive := range strings.Split(w.Header().Get("Content-Security-Policy"), ";") {
				name, value, _ := strings.Cut(strings.TrimSpace(directive), " ")
				csp[name] = strings.TrimSpace(value)
			}
			if csp["default-src"] != "'none'" || csp["script-src"] != "'none'" {
				t.Fatal("HTML preview response relaxed resource or script CSP")
			}
		})
	}
}

func TestRenderPreviewNonHTMLDoesNotCreateFrame(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"report.md", "# Reading\n\n```html\n<iframe srcdoc='fixture'></iframe>\n```\n"},
		{"main.go", "package fixture\nvar markup = \"<iframe></iframe>\"\n"},
		{"notes.txt", "<iframe srcdoc='fixture'></iframe>\n"},
		{"unknown.fixture", "<!doctype html><html><body>fixture</body></html>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := renderedPage(t, tc.name, tc.content)
			if len(renderedTags(page, "iframe")) != 0 {
				t.Fatal("non-HTML preview created a frame")
			}
			assertNoRenderedActiveContent(t, page)
		})
	}
}
