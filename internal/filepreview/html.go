package filepreview

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// staticHTMLDocument prepares the isolated, script-free view. CSP blocks
// resource loads, but browsers can still act on connection hints. Remove those
// hints and nested browsing contexts before serializing the child document.
// The original bytes remain available in the numbered source view.
func staticHTMLDocument(content []byte) string {
	document, err := html.ParseWithOptions(bytes.NewReader(content), html.ParseOptionEnableScripting(false))
	if err != nil {
		return ""
	}
	var head *html.Node
	var clean func(*html.Node)
	clean = func(parent *html.Node) {
		for node := parent.FirstChild; node != nil; {
			next := node.NextSibling
			if node.Type == html.ElementNode {
				switch node.Data {
				case "link", "iframe", "frame":
					parent.RemoveChild(node)
					node = next
					continue
				case "meta":
					for _, attr := range node.Attr {
						if attr.Key == "http-equiv" && strings.EqualFold(strings.TrimSpace(attr.Val), "x-dns-prefetch-control") {
							parent.RemoveChild(node)
							break
						}
					}
				case "noscript":
					// Render treats noscript text as raw HTML even though this
					// document was parsed with scripting disabled. A normal div
					// preserves the fallback content and escapes literal markup.
					node.Data, node.DataAtom = "div", atom.Div
				case "head":
					if node.Namespace == "" {
						head = node
					}
				case "a", "area":
					localFragment := false
					for i := range node.Attr {
						attr := &node.Attr[i]
						href := strings.TrimSpace(attr.Val)
						if attr.Key == "href" && strings.HasPrefix(href, "#") {
							// srcdoc inherits the outer preview's base URL. Keep
							// directory links in this document instead of loading
							// the outer /f/ URL inside the frame (blocked by CSP).
							attr.Val = "about:srcdoc" + href
							localFragment = true
						}
					}
					if localFragment {
						hasTarget := false
						for i := range node.Attr {
							if node.Attr[i].Key == "target" {
								node.Attr[i].Val = "_self"
								hasTarget = true
							}
						}
						if !hasTarget {
							node.Attr = append(node.Attr, html.Attribute{Key: "target", Val: "_self"})
						}
					}
				}
			}
			clean(node)
			node = next
		}
	}
	clean(document)
	if head == nil {
		return ""
	}
	// Set the child's own DNS policy before any user content. An off policy
	// inherited from the outer response alone can be re-enabled by child meta.
	head.InsertBefore(&html.Node{Type: html.ElementNode, Data: "meta", DataAtom: atom.Meta, Attr: []html.Attribute{
		{Key: "http-equiv", Val: "x-dns-prefetch-control"}, {Key: "content", Val: "off"},
	}}, head.FirstChild)
	var rendered bytes.Buffer
	if html.Render(&rendered, document) != nil {
		return ""
	}
	return rendered.String()
}
