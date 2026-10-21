package filepreview

import (
	"bytes"
	"context"
	"errors"
	"html"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"codex-feishu/internal/codexclient"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type sourceLink struct {
	node                    ast.Node
	destination             []byte
	closeLabel, start, stop int
	auto                    bool
}

// GFM's ordinary URL matcher requires a dotted hostname. The malformed local
// links we repair have a single path component in that position instead.
var localHTTPPattern = regexp.MustCompile("^http://[A-Za-z0-9_-]{1,256}/[^\\s<>\"`。！？；，]*")

// recordingLinks delegates CommonMark parsing to goldmark and records only the
// destination spans it actually accepted as inline links. This avoids rewriting
// fenced/indented code, code spans, escaped syntax, images and reference links.
type recordingLinks struct {
	parser.InlineParser
	source []byte
	links  []sourceLink
}

func (p *recordingLinks) Parse(parent ast.Node, reader text.Reader, pc parser.Context) ast.Node {
	line, segment := reader.PeekLine()
	closing := len(line) != 0 && line[0] == ']'
	node := p.InlineParser.Parse(parent, reader, pc)
	link, ok := node.(*ast.Link)
	if !closing || !ok || link.Reference != nil || len(link.Destination) == 0 {
		return node
	}
	at := segment.Start
	if at+2 > len(p.source) || !bytes.Equal(p.source[at:at+2], []byte("](")) {
		return node
	}
	start := at + 2
	for start < len(p.source) && (p.source[start] == ' ' || p.source[start] == '\t' || p.source[start] == '\r' || p.source[start] == '\n') {
		start++
	}
	angle := start < len(p.source) && p.source[start] == '<'
	if angle {
		start++
	}
	stop := start + len(link.Destination)
	if stop <= len(p.source) && bytes.Equal(p.source[start:stop], link.Destination) {
		// The replacement URL has no spaces. Remove optional angle delimiters so
		// downstream native Markdown escaping cannot turn them into URL content.
		if angle && stop < len(p.source) && p.source[stop] == '>' {
			start--
			stop++
		}
		p.links = append(p.links, sourceLink{node: link, destination: link.Destination, closeLabel: at, start: start, stop: stop})
	}
	return node
}

// Record autolinks at the parser boundary: its consumed source range is exact,
// unlike a global text search that could also match the same URL inside code.
type recordingAutoLinks struct {
	parser.InlineParser
	source []byte
	links  *[]sourceLink
	angle  bool
}

func (p *recordingAutoLinks) Parse(parent ast.Node, reader text.Reader, pc parser.Context) ast.Node {
	_, before := reader.PeekLine()
	node := p.InlineParser.Parse(parent, reader, pc)
	link, ok := node.(*ast.AutoLink)
	if !ok || link.AutoLinkType != ast.AutoLinkURL {
		return node
	}
	_, after := reader.Position()
	start, stop := before.Start, after.Start
	if start < 0 || stop <= start || stop > len(p.source) {
		return node
	}
	label := link.Label(p.source)
	if p.angle {
		if !bytes.Equal(p.source[start:stop], append(append([]byte{'<'}, label...), '>')) {
			return node
		}
	} else {
		offset := bytes.Index(p.source[start:stop], label)
		if offset < 0 {
			return node
		}
		start += offset
		stop = start + len(label)
	}
	*p.links = append(*p.links, sourceLink{node: link, destination: link.URL(p.source), start: start, stop: stop, auto: true})
	return node
}

func (p *recordingLinks) CloseBlock(parent ast.Node, reader text.Reader, pc parser.Context) {
	if closer, ok := p.InlineParser.(parser.CloseBlocker); ok {
		closer.CloseBlock(parent, reader, pc)
	}
}

func parsedLinks(source []byte) (ast.Node, []sourceLink) {
	inline := parser.DefaultInlineParsers()
	recorder := &recordingLinks{InlineParser: parser.NewLinkParser(), source: source}
	var automatic []sourceLink
	autoRecorder := &recordingAutoLinks{InlineParser: parser.NewAutoLinkParser(), source: source, links: &automatic, angle: true}
	for i, candidate := range inline {
		if candidate.Value == parser.NewLinkParser() {
			inline[i].Value = recorder
		} else if candidate.Value == parser.NewAutoLinkParser() {
			inline[i].Value = autoRecorder
		}
	}
	inline = append(inline, util.Prioritized(&recordingAutoLinks{InlineParser: extension.NewLinkifyParser(extension.WithLinkifyURLRegexp(localHTTPPattern)), source: source, links: &automatic}, 998))
	p := parser.NewParser(parser.WithBlockParsers(parser.DefaultBlockParsers()...),
		parser.WithInlineParsers(inline...), parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...))
	md := goldmark.New(goldmark.WithParser(p), goldmark.WithExtensions(extension.GFM))
	document := md.Parser().Parse(text.NewReader(source))
	return document, append(recorder.links, automatic...)
}

type sourceEdit struct {
	start, stop int
	value       string
}

// Rewrite returns a display copy with eligible file links and App visualizations
// replaced by durable preview URLs. Inaccessible ordinary links stay unchanged;
// visualizations receive a readable fallback. Registry failures abort the copy.
func (s *Service) Rewrite(ctx context.Context, body string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return body, errClosed
	}
	if err := ctx.Err(); err != nil {
		return body, err
	}
	if !strings.Contains(body, "](") && !strings.Contains(body, "http://") && !strings.Contains(body, "\ue200visualize") {
		return body, nil
	}
	original := body
	source := []byte(body)
	visualizations := codexclient.ParseVisualizations(body)
	document, links := parsedLinks(source)
	var edits []sourceEdit
	linked := make(map[string]bool)
	for _, link := range links {
		if err := ctx.Err(); err != nil {
			return body, err
		}
		// Native embed payloads are structured data, even when a filename or
		// malformed field contains Markdown. Only links that survive consuming
		// the envelope may be rewritten or suppress a visualization entry.
		at := sort.Search(len(visualizations), func(n int) bool { return visualizations[n].Stop > link.start })
		if at < len(visualizations) && visualizations[at].Start < link.stop {
			continue
		}
		// Images can contain link-like alt content; never alter their contents.
		current := ast.Node(link.node)
		eligible := true
		for current != nil && current != document {
			if current.Kind() == ast.KindImage {
				eligible = false
				break
			}
			current = current.Parent()
		}
		if !eligible || current != document {
			continue
		}
		if destination, _, _ := strings.Cut(string(link.destination), "#"); strings.HasPrefix(destination, s.baseURL+"/f/") {
			linked[destination] = true
		}
		path, line, ok := localDestination(link.destination)
		if !ok {
			continue
		}
		root, relative := s.rootForPath(path)
		if root == nil {
			continue
		}
		if _, err := s.readFile(root, relative); err != nil {
			if errors.Is(err, errNotPreviewable) {
				continue
			}
			return body, err
		}
		id, err := s.persistRecord(fileRecord{Version: 1, Root: root.path, Relative: relative})
		if err != nil {
			return body, err
		}
		target := s.baseURL + "/f/" + id
		linked[target] = true
		if line != "" {
			target += "#L" + line
			if !link.auto && !labelHasLine(string(link.node.Text(source)), line) {
				edits = append(edits, sourceEdit{start: link.closeLabel, stop: link.closeLabel, value: " · L" + line})
			}
		}
		if link.auto {
			label := filepath.Base(path)
			label = strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "`", "\\`", "*", "\\*", "_", "\\_", "<", "&lt;", ">", "&gt;").Replace(label)
			if line != "" {
				label += " · L" + line
			}
			target = "[" + label + "](" + target + ")"
		}
		edits = append(edits, sourceEdit{start: link.start, stop: link.stop, value: target})
	}
	if len(edits) > 0 {
		sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
		var result strings.Builder
		result.Grow(len(body))
		previous := 0
		for _, edit := range edits {
			result.WriteString(body[previous:edit.start])
			result.WriteString(edit.value)
			previous = edit.stop
		}
		result.WriteString(body[previous:])
		body = result.String()
	}
	result, err := codexclient.RewriteVisualizations(body, func(path string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		root, relative := s.rootForPath(path)
		if root == nil {
			return codexclient.VisualizationFallback, nil
		}
		if _, err := s.readFile(root, relative); err != nil {
			if errors.Is(err, errNotPreviewable) {
				return codexclient.VisualizationFallback, nil
			}
			return "", err
		}
		id, err := s.persistRecord(fileRecord{Version: 1, Root: root.path, Relative: relative})
		if err != nil {
			return "", err
		}
		target := s.baseURL + "/f/" + id
		if linked[target] {
			return "", nil
		}
		linked[target] = true
		return "[查看可视化 · 静态预览](" + target + ")（交互版请回 Codex 查看）", nil
	})
	if err != nil {
		return original, err
	}
	return result, nil
}

func labelHasLine(label, line string) bool {
	for _, prefix := range []string{"L", ":"} {
		needle := prefix + line
		for start := 0; start < len(label); {
			at := strings.Index(label[start:], needle)
			if at < 0 {
				break
			}
			end := start + at + len(needle)
			if end == len(label) || label[end] < '0' || label[end] > '9' {
				return true
			}
			start = end
		}
	}
	return false
}

func localDestination(destination []byte) (path, line string, ok bool) {
	value := html.UnescapeString(string(util.UnescapePunctuations(destination)))
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" {
		return "", "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "":
		if u.Host != "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
			return "", "", false
		}
		path = u.Path
	case "file":
		if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
			return "", "", false
		}
		path = u.Path
	case "http":
		// Repair the observed http://data00/home/... typo only if the caller can
		// subsequently prove that this absolute path is in an explicit local root.
		// Hosts with dots or ports are ordinary web links and are never repaired.
		if u.Host == "" || strings.ContainsAny(u.Host, ".:%[]\\") {
			return "", "", false
		}
		path = "/" + u.Host + u.Path
	default:
		return "", "", false
	}
	path, line = splitLocation(path)
	if u.Fragment != "" {
		// Codex also emits #L12 and #L12C5 locations. The preview has line
		// anchors only; reject other fragments and mixed location syntaxes.
		if line != "" || !strings.HasPrefix(u.Fragment, "L") {
			return "", "", false
		}
		fragmentLine, column, hasColumn := strings.Cut(u.Fragment[1:], "C")
		if !positiveLine(fragmentLine) || hasColumn && !positiveLine(column) {
			return "", "", false
		}
		value, _ := strconv.ParseUint(fragmentLine, 10, 32)
		line = strconv.FormatUint(value, 10)
	}
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\\\x00\r\n") {
		return "", "", false
	}
	for _, component := range strings.Split(path, "/") {
		if component == ".." {
			return "", "", false
		}
	}
	return filepath.Clean(path), line, true
}

func splitLocation(path string) (string, string) {
	at := strings.LastIndexByte(path, ':')
	if at < 0 || !positiveLine(path[at+1:]) {
		return path, ""
	}
	line := path[at+1:]
	path = path[:at]
	if prior := strings.LastIndexByte(path, ':'); prior >= 0 && positiveLine(path[prior+1:]) {
		line, path = path[prior+1:], path[:prior]
	}
	value, _ := strconv.ParseUint(line, 10, 32)
	return path, strconv.FormatUint(value, 10)
}

func positiveLine(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(value, 10, 32)
	return err == nil && n > 0
}

func (s *Service) rootForPath(path string) (*allowedRoot, string) {
	var found *allowedRoot
	var relative string
	longest := -1
	for _, root := range s.roots {
		for _, alias := range root.aliases {
			rel, err := filepath.Rel(alias, path)
			if err == nil && len(alias) > longest && allowedRelative(rel) {
				found, relative, longest = root, rel, len(alias)
			}
		}
	}
	return found, relative
}
