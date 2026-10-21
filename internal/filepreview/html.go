package filepreview

import (
	"bytes"
	"path/filepath"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

func isHTMLFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".html" || ext == ".htm" || ext == ".xhtml"
}

// interactiveHTMLDocument prepares a self-contained report for a script-enabled,
// opaque-origin sandbox. CSP restricts resource loads; remove static connection
// hints, navigation and nested contexts as well. This is not a sanitizer for
// arbitrary JavaScript: reports must still come from a trusted source.
// The original bytes remain available in the numbered source view.
func interactiveHTMLDocument(content []byte) string {
	document, err := html.Parse(bytes.NewReader(content))
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
				case "link", "iframe", "frame", "base", "object", "embed":
					parent.RemoveChild(node)
					node = next
					continue
				case "script":
					external := false
					for _, attr := range node.Attr {
						external = external || attr.Key == "src" || attr.Key == "href"
					}
					if external {
						parent.RemoveChild(node)
						node = next
						continue
					}
				case "meta":
					for _, attr := range node.Attr {
						if attr.Key == "http-equiv" && (strings.EqualFold(strings.TrimSpace(attr.Val), "x-dns-prefetch-control") || strings.EqualFold(strings.TrimSpace(attr.Val), "refresh")) {
							parent.RemoveChild(node)
							break
						}
					}
				case "head":
					if node.Namespace == "" {
						head = node
					}
				case "a", "area":
					localFragment := false
					attrs := node.Attr[:0]
					for _, attr := range node.Attr {
						if attr.Key == "href" {
							href := strings.TrimSpace(attr.Val)
							if !strings.HasPrefix(href, "#") {
								continue
							}
							// Preserve fragment selectors used by report scripts.
							// The trusted base also keeps dynamic links in srcdoc.
							attr.Val = href
							localFragment = true
						}
						attrs = append(attrs, attr)
					}
					node.Attr = attrs
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
				attrs := node.Attr[:0]
				for _, attr := range node.Attr {
					if !strings.HasPrefix(attr.Key, "on") && attr.Key != "ping" {
						attrs = append(attrs, attr)
					}
				}
				node.Attr = attrs
			}
			clean(node)
			node = next
		}
	}
	clean(document)
	if head == nil {
		return ""
	}
	// srcdoc otherwise inherits the wrapper's URL. A fixed local base keeps
	// both static and dynamically created #links in this document without
	// rewriting href values that scripts use as querySelector inputs.
	head.InsertBefore(&html.Node{Type: html.ElementNode, Data: "base", DataAtom: atom.Base, Attr: []html.Attribute{
		{Key: "href", Val: "about:srcdoc"}, {Key: "target", Val: "_self"},
	}}, head.FirstChild)
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
