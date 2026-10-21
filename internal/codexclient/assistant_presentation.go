package codexclient

import (
	"encoding/json"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

const VisualizationFallback = "请回 Codex 查看可视化。"

const (
	visualizationStart = "\ue200visualize"
	visualizationData  = "\ue202"
	visualizationEnd   = "\ue201"
)

// VisualizationSpan identifies a consumed App envelope in the original body's
// byte coordinates [Start, Stop). Path is empty for a fallback-only envelope.
type VisualizationSpan struct {
	Start, Stop int
	Path        string
}

// RewriteVisualizations prepares an assistant display copy without changing its
// native text. Only known App visualize envelopes outside literal Markdown are
// consumed. A nil renderer or invalid envelope uses the fallback. An empty
// rendered value removes the envelope; renderer failures return the original
// body, never a partial rewrite.
func RewriteVisualizations(body string, render func(path string) (string, error)) (string, error) {
	spans := ParseVisualizations(body)
	if len(spans) == 0 {
		return body, nil
	}
	var result strings.Builder
	previous := 0
	for _, span := range spans {
		replacement := VisualizationFallback
		if span.Path != "" && render != nil {
			value, err := render(span.Path)
			if err != nil {
				return body, err
			}
			replacement = value
		}
		result.WriteString(body[previous:span.Start])
		result.WriteString(replacement)
		previous = span.Stop
	}
	result.WriteString(body[previous:])
	return result.String(), nil
}

// ParseVisualizations finds known envelopes outside literal Markdown without
// reading referenced files. Spans are ordered and non-overlapping; incomplete
// or malformed envelopes use conservative boundaries and an empty Path. Unknown
// marker names, code and escaped examples are left outside the returned spans.
func ParseVisualizations(body string) []VisualizationSpan {
	if !strings.Contains(body, visualizationStart) {
		return nil
	}
	protected := visualizationCodeRanges([]byte(body))
	var spans []VisualizationSpan
	scan, code := 0, 0
	for scan < len(body) {
		at := strings.Index(body[scan:], visualizationStart)
		if at < 0 {
			break
		}
		start := scan + at
		headerEnd := start + len(visualizationStart)
		scan = headerEnd
		// A different marker name with this prefix is not a visualize envelope.
		if headerEnd < len(body) {
			r, _ := utf8.DecodeRuneInString(body[headerEnd:])
			if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_-./:", r) {
				continue
			}
		}
		for code < len(protected) && protected[code].stop <= start {
			code++
		}
		if code < len(protected) && protected[code].start <= start || visualizationEscaped(body, start) {
			continue
		}
		// A fresh marker is a hard boundary: an incomplete envelope must not
		// swallow another visual or an unrelated App marker. Without a closing
		// marker consume only the current line, preserving subsequent prose.
		rest := body[headerEnd:]
		boundary := strings.IndexAny(rest, "\ue200\ue201")
		complete := boundary >= 0 && strings.HasPrefix(rest[boundary:], visualizationEnd)
		payload := ""
		if complete {
			payload = rest[:boundary]
			closing := headerEnd + boundary
			endCode := code
			for endCode < len(protected) && protected[endCode].stop <= closing {
				endCode++
			}
			if endCode < len(protected) && protected[endCode].start <= closing {
				complete = false
			}
			// Only a structurally valid path object can own multiple lines.
			// A stray closing marker in later prose is not enough evidence to
			// consume that prose as part of a damaged envelope.
			if strings.ContainsAny(payload, "\r\n") {
				_, validJSON := visualizationJSONPath(payload)
				complete = complete && validJSON
			}
		}
		stop := headerEnd
		if complete {
			stop += boundary + len(visualizationEnd)
		} else {
			length := len(rest)
			if boundary >= 0 {
				length = boundary
			}
			if newline := strings.IndexAny(rest[:length], "\r\n"); newline >= 0 {
				length = newline
			}
			if code < len(protected) && protected[code].start < headerEnd+length {
				length = protected[code].start - headerEnd
			}
			stop += length
		}
		path := ""
		if complete {
			path, _ = visualizationPath(payload)
		}
		spans = append(spans, VisualizationSpan{Start: start, Stop: stop, Path: path})
		scan = stop
	}
	return spans
}

func visualizationPath(payload string) (string, bool) {
	path, ok := visualizationJSONPath(payload)
	if !ok || !utf8.ValidString(path) || !filepath.IsAbs(path) || strings.Contains(path, "\\") ||
		strings.ContainsFunc(path, unicode.IsControl) {
		return "", false
	}
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return "", false
		}
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm", ".xhtml":
		return path, true
	default:
		return "", false
	}
}

func visualizationJSONPath(payload string) (string, bool) {
	if !utf8.ValidString(payload) || !strings.HasPrefix(payload, visualizationData) {
		return "", false
	}
	d := json.NewDecoder(strings.NewReader(strings.TrimPrefix(payload, visualizationData)))
	open, err := d.Token()
	if err != nil || open != json.Delim('{') {
		return "", false
	}
	key, err := d.Token()
	if err != nil || key != "path" {
		return "", false
	}
	var path string
	if d.Decode(&path) != nil || d.More() {
		return "", false
	}
	close, err := d.Token()
	if err != nil || close != json.Delim('}') {
		return "", false
	}
	if _, err := d.Token(); err != io.EOF {
		return "", false
	}
	return path, true
}

func visualizationEscaped(body string, start int) bool {
	backslashes := 0
	for start > 0 && body[start-1] == '\\' {
		backslashes++
		start--
	}
	return backslashes%2 != 0
}

type visualizationCodeRange struct{ start, stop int }

// Let the existing CommonMark parser recognize code, including quoted/list code
// blocks and variable-width backtick spans, rather than approximating Markdown.
func visualizationCodeRanges(source []byte) []visualizationCodeRange {
	document := goldmark.New().Parser().Parse(text.NewReader(source))
	var ranges []visualizationCodeRange
	add := func(segment text.Segment) {
		ranges = append(ranges, visualizationCodeRange{segment.Start, segment.Stop})
	}
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *ast.CodeBlock, *ast.FencedCodeBlock:
			for n := 0; n < node.Lines().Len(); n++ {
				add(node.Lines().At(n))
			}
			if fenced, ok := node.(*ast.FencedCodeBlock); ok && fenced.Info != nil {
				add(fenced.Info.Segment)
			}
			return ast.WalkSkipChildren, nil
		case *ast.CodeSpan:
			first, last := node.FirstChild().(*ast.Text), node.LastChild().(*ast.Text)
			start, stop := first.Segment.Start, last.Segment.Stop
			// Goldmark may trim one surrounding space/newline. Include that
			// padding and the delimiters so a damaged preceding envelope cannot
			// consume the opening backticks while preserving only code content.
			if start > 0 && stop < len(source) &&
				(source[start-1] == ' ' || source[start-1] == '\n') &&
				(source[stop] == ' ' || source[stop] == '\n') {
				start--
				stop++
			}
			for start > 0 && source[start-1] == '`' {
				start--
			}
			for stop < len(source) && source[stop] == '`' {
				stop++
			}
			ranges = append(ranges, visualizationCodeRange{start, stop})
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	return ranges
}
