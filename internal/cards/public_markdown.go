package cards

import (
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

const maxPublicBodyElements = 64

// SafeMarkdown retains only structural quote prefixes from the original
// Markdown. Native tags remain escaped, including tags inside quoted code.
func SafeMarkdown(body string) string {
	body = publicSafeImages(body)
	var out strings.Builder
	var fence *messageFence
	fenceQuoteDepth, inlineWidth := 0, 0
	lines := strings.SplitAfter(body, "\n")
	for i := 0; i < len(lines); {
		line := lines[i]
		// Feishu discards CommonMark quote paragraph boundaries. Explicit
		// breaks survive its conversion without fragmenting the quote rail.
		previousDepth := 0
		if i > 0 {
			_, _, previousDepth = publicQuotePrefix(lines[i-1], -1)
		}
		if fence == nil && inlineWidth == 0 && previousDepth == 0 {
			if quote, consumed := publicSimpleQuote(lines[i:]); consumed > 0 {
				out.WriteString(quote)
				i += consumed
				continue
			}
		}
		i++
		depthLimit := -1
		if fence != nil {
			depthLimit = fenceQuoteDepth
		}
		prefix, text, depth := publicQuotePrefix(line, depthLimit)
		if fence != nil {
			out.WriteString(prefix)
			out.WriteString(Safe(text))
			if closesMessageFence(text, fence) {
				fence = nil
			}
			continue
		}
		if opened := openingMessageFence(text); opened != nil {
			fence, fenceQuoteDepth, inlineWidth = opened, depth, 0
			out.WriteString(prefix)
			out.WriteString(Safe(text))
			continue
		}
		out.WriteString(prefix)
		if publicIndented(text) {
			out.WriteString(Safe(text))
			continue
		}
		out.WriteString(Safe(publicCodeReferences(text, &inlineWidth)))
	}
	return out.String()
}

type publicImage struct {
	node        *ast.Image
	start, stop int
}

// recordingImages observes only image nodes accepted by CommonMark. Code,
// escaped examples and malformed lookalikes therefore remain literal.
type recordingImages struct {
	parser.InlineParser
	source []byte
	images []publicImage
}

func (p *recordingImages) Parse(parent ast.Node, reader text.Reader, pc parser.Context) ast.Node {
	node := p.InlineParser.Parse(parent, reader, pc)
	image, ok := node.(*ast.Image)
	if !ok {
		return node
	}
	_, after := reader.Position()
	start, stop := image.Pos(), after.Start
	if start >= 0 && start < stop && stop <= len(p.source) && p.source[start] == '!' {
		p.images = append(p.images, publicImage{node: image, start: start, stop: stop})
	}
	return node
}

func (p *recordingImages) CloseBlock(parent ast.Node, reader text.Reader, pc parser.Context) {
	if closer, ok := p.InlineParser.(parser.CloseBlocker); ok {
		closer.CloseBlock(parent, reader, pc)
	}
}

type publicImageEdit struct {
	start, stop int
	value       string
}

// publicSafeImages prevents model-produced Markdown images from reaching a
// Feishu markdown component. Feishu image syntax expects an uploaded image key,
// not a local path or arbitrary URL. Web images become ordinary links; every
// other destination becomes path-free text. Reference definitions for unsafe
// images are removed as well so their destinations cannot leak in the card.
func publicSafeImages(body string) string {
	if !strings.Contains(body, "![") {
		return body
	}
	source := []byte(body)
	edits := publicSafeImageEdits(source)
	if len(edits) == 0 {
		return body
	}
	return applyPublicImageEdits(source, 0, len(source), edits)
}

func publicSafeImageEdits(source []byte) []publicImageEdit {
	inline := parser.DefaultInlineParsers()
	recorder := &recordingImages{InlineParser: parser.NewLinkParser(), source: source}
	for i, candidate := range inline {
		if candidate.Value == parser.NewLinkParser() {
			inline[i].Value = recorder
		}
	}
	p := parser.NewParser(parser.WithBlockParsers(parser.DefaultBlockParsers()...),
		parser.WithInlineParsers(inline...), parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...))
	document := goldmark.New(goldmark.WithParser(p)).Parser().Parse(text.NewReader(source))
	if len(recorder.images) == 0 {
		return nil
	}
	unsafeReferences := map[string]bool{}
	edits := make([]publicImageEdit, 0, len(recorder.images))
	for _, image := range recorder.images {
		if publicWebImage(image.node.Destination) {
			nested := false
			for parent := image.node.Parent(); parent != nil && parent != document; parent = parent.Parent() {
				if parent.Kind() == ast.KindLink {
					nested = true
					break
				}
			}
			if nested {
				edits = append(edits, publicImageEdit{start: image.start, stop: image.stop, value: "查看图片"})
			} else {
				// Removing only the image marker preserves the parser-accepted
				// destination, title, label and reference spelling as a link.
				edits = append(edits, publicImageEdit{start: image.start, stop: image.start + 1})
			}
			continue
		}
		edits = append(edits, publicImageEdit{start: image.start, stop: image.stop, value: "图片请回 Codex 查看"})
		if image.node.Reference != nil {
			unsafeReferences[util.ToLinkReference(image.node.Reference.Value)] = true
		}
	}
	if len(unsafeReferences) != 0 {
		_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
			definition, ok := node.(*ast.LinkReferenceDefinition)
			if !entering || !ok || !unsafeReferences[util.ToLinkReference(definition.Label)] || definition.Lines().Len() == 0 {
				return ast.WalkContinue, nil
			}
			first := definition.Lines().At(0)
			last := definition.Lines().At(definition.Lines().Len() - 1)
			replacement := ""
			if last.Stop > first.Start && source[last.Stop-1] == '\n' {
				replacement = "\n"
			}
			edits = append(edits, publicImageEdit{start: first.Start, stop: last.Stop, value: replacement})
			return ast.WalkContinue, nil
		})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	return edits
}

func applyPublicImageEdits(source []byte, start, stop int, edits []publicImageEdit) string {
	if len(edits) == 0 {
		return string(source[start:stop])
	}
	var out strings.Builder
	out.Grow(stop - start)
	previous := start
	for _, edit := range edits {
		if edit.stop <= start {
			continue
		}
		if edit.start >= stop {
			break
		}
		// SplitMessage keeps multi-byte edit ranges atomic. Ignore a partial
		// range defensively instead of dropping bytes at a segment boundary.
		if edit.start < previous || edit.start < start || edit.stop > stop || edit.stop < edit.start || edit.stop > len(source) {
			continue
		}
		out.Write(source[previous:edit.start])
		out.WriteString(edit.value)
		previous = edit.stop
	}
	out.Write(source[previous:stop])
	return out.String()
}

func publicWebImage(destination []byte) bool {
	raw := html.UnescapeString(string(util.UnescapePunctuations(destination)))
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme == "http" || scheme == "https"
}

// Only ordinary single-layer quotes receive compact explicit line breaks.
// Complex child blocks keep their Markdown: flattening a code block, nested
// quote or list would change its meaning. Raw <br> and other input tags are
// escaped before the renderer inserts its own fixed, trusted break tags.
func publicSimpleQuote(lines []string) (string, int) {
	var text []string
	count, inlineWidth := 0, 0
	for _, line := range lines {
		_, content, depth := publicQuotePrefix(line, -1)
		if depth == 0 {
			break
		}
		if depth != 1 || publicIndented(content) || openingMessageFence(content) != nil ||
			publicListStart.MatchString(content) || strings.HasPrefix(content, "|") ||
			strings.HasPrefix(strings.TrimLeft(content, " "), "#") {
			return "", 0
		}
		count++
		content = strings.TrimRight(content, " \t\r\n")
		if content == "" {
			continue
		}
		if strings.Trim(content, "-*_= \t") == "" {
			return "", 0
		}
		// A Markdown hard-break backslash is presentation, not quoted text.
		backslashes := len(content) - len(strings.TrimRight(content, "\\"))
		if backslashes%2 != 0 {
			content = strings.TrimSuffix(content, "\\")
		}
		content = publicCodeReferences(content, &inlineWidth)
		if inlineWidth != 0 {
			return "", 0
		}
		text = append(text, Safe(content))
	}
	if len(text) < 2 {
		return "", 0
	}
	// Quoted empty paragraphs do not add empty display rows. The body stays
	// in one native quote with adjacent title, text and link lines.
	indent := len(lines[0]) - len(strings.TrimLeft(lines[0], " "))
	quote := lines[0][:indent] + "> " + strings.Join(text, "<br>")
	if strings.HasSuffix(lines[count-1], "\n") {
		quote += "\n"
	}
	return quote, count
}

// limit==0 keeps every '>' literal; a positive limit preserves only the outer
// quote layers around a fenced code block. Up to three leading spaces are
// Markdown indentation; four spaces belong to literal indented code instead.
func publicQuotePrefix(line string, limit int) (string, string, int) {
	end, depth := 0, 0
	for limit < 0 || depth < limit {
		pos := end
		for pos < len(line) && line[pos] == ' ' && pos-end < 3 {
			pos++
		}
		if pos >= len(line) || line[pos] != '>' {
			break
		}
		end, depth = pos+1, depth+1
		if end < len(line) && (line[end] == ' ' || line[end] == '\t') {
			end++
		}
	}
	return line[:end], line[end:], depth
}

var publicListStart = regexp.MustCompile(`^( {0,3})([-+*]|[0-9]{1,9}[.)])[ \t]+`)

func publicIndented(line string) bool {
	return strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
}

func publicBlockKind(line string) string {
	if publicIndented(line) {
		return "indented"
	}
	if _, _, depth := publicQuotePrefix(line, -1); depth > 0 {
		return "quote"
	}
	if publicListStart.MatchString(line) {
		return "list"
	}
	return "paragraph"
}

// Only unambiguous blank paragraph boundaries become separate components.
// Complex lists, quotes and code retain their original internal layout.
func publicBodyElements(body, size string) []element {
	body = publicSafeImages(body)
	var blocks []string
	start, offset, kind := 0, 0, ""
	pendingBlank := false
	var fence *messageFence
	for _, line := range strings.SplitAfter(body, "\n") {
		blank := strings.Trim(line, " \t\r\n") == ""
		if fence == nil && !blank {
			keep := kind == "list" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || publicListStart.MatchString(line)) ||
				kind == "quote" && publicBlockKind(line) == "quote" ||
				kind == "indented" && publicIndented(line)
			if pendingBlank && kind != "" && !keep {
				blocks = append(blocks, body[start:offset])
				start, kind = offset, ""
			}
			if kind == "" {
				kind = publicBlockKind(line)
			}
			pendingBlank = false
		}
		if fence == nil {
			if opened := openingMessageFence(line); opened != nil {
				fence = opened
			} else if blank {
				pendingBlank = true
			}
		} else if closesMessageFence(line, fence) {
			fence = nil
		}
		offset += len(line)
	}
	blocks = append(blocks, body[start:])
	var spaced []string
	for _, block := range blocks {
		spaced = append(spaced, publicFlatListItems(block)...)
	}
	blocks = spaced
	if len(blocks) > maxPublicBodyElements {
		blocks[maxPublicBodyElements-1] = strings.Join(blocks[maxPublicBodyElements-1:], "")
		blocks = blocks[:maxPublicBodyElements]
	}
	result := make([]element, 0, len(blocks))
	for _, block := range blocks {
		result = append(result, element{"tag": "markdown", "content": SafeMarkdown(strings.Trim(block, "\r\n")),
			"text_size": size, "margin": "0px"})
	}
	return result
}

var publicFlatListStart = regexp.MustCompile(`^([-+*]|([0-9]{1,9})([.)]))[ \t]+`)

// One-line, flat items can receive predictable native component spacing. Keep
// automatic numbering, continuations and nested structures in one Markdown
// component so splitting cannot change CommonMark's list semantics.
func publicFlatListItems(block string) []string {
	var items []string
	start, offset, firstNumber := 0, 0, 0
	marker := ""
	for _, line := range strings.SplitAfter(block, "\n") {
		if strings.Trim(line, " \t\r\n") != "" {
			match := publicFlatListStart.FindStringSubmatch(line)
			if match == nil {
				return []string{block}
			}
			family := match[1]
			if match[2] != "" {
				number, _ := strconv.Atoi(match[2])
				family = match[3]
				if marker == "" {
					firstNumber = number
				} else if number != firstNumber+len(items)+1 {
					return []string{block}
				}
			}
			if marker != "" {
				if marker != family {
					return []string{block}
				}
				items = append(items, block[start:offset])
				start = offset
			}
			marker = family
		}
		offset += len(line)
	}
	if len(items) == 0 {
		return []string{block}
	}
	return append(items, block[start:])
}

var publicLocalLink = regexp.MustCompile(`\[([^\]\r\n]+)\]\((/[^)\r\n]+)\)`)

// Local code references keep their exact destination. App file-link metadata
// is represented in the label so readers can see the cited line in Feishu.
func publicCodeReferences(text string, inlineWidth *int) string {
	var out strings.Builder
	for len(text) > 0 {
		index := strings.IndexByte(text, '`')
		if index < 0 {
			if *inlineWidth == 0 {
				out.WriteString(publicCodeLinkLabels(text))
			} else {
				out.WriteString(text)
			}
			break
		}
		if *inlineWidth == 0 {
			out.WriteString(publicCodeLinkLabels(text[:index]))
		} else {
			out.WriteString(text[:index])
		}
		end := index
		for end < len(text) && text[end] == '`' {
			end++
		}
		width := end - index
		out.WriteString(text[index:end])
		if *inlineWidth == 0 {
			*inlineWidth = width
		} else if *inlineWidth == width {
			*inlineWidth = 0
		}
		text = text[end:]
	}
	return out.String()
}

func publicCodeLinkLabels(text string) string {
	indices := publicLocalLink.FindAllStringSubmatchIndex(text, -1)
	if len(indices) == 0 {
		return text
	}
	var out strings.Builder
	last := 0
	for _, match := range indices {
		label, target := text[match[2]:match[3]], text[match[4]:match[5]]
		line := publicCodeLine(target)
		if line == "" || publicLinkEscaped(text, match[0]) || match[0] > 0 && text[match[0]-1] == '!' ||
			strings.Contains(label, "L"+line) || strings.Contains(label, "line "+line) || strings.HasSuffix(label, ":"+line) {
			continue
		}
		out.WriteString(text[last:match[3]])
		out.WriteString(" · L" + line)
		last = match[3]
	}
	out.WriteString(text[last:])
	return out.String()
}

func publicLinkEscaped(text string, offset int) bool {
	backslashes := 0
	for offset > 0 && text[offset-1] == '\\' {
		backslashes++
		offset--
	}
	return backslashes%2 != 0
}

func publicCodeLine(target string) string {
	pos := strings.LastIndexByte(target, ':')
	if pos < 0 {
		return ""
	}
	line := target[pos+1:]
	value, err := strconv.Atoi(line)
	if err != nil || value <= 0 {
		return ""
	}
	if columnPos := strings.LastIndexByte(target[:pos], ':'); columnPos >= 0 {
		candidate := target[columnPos+1 : pos]
		if value, err := strconv.Atoi(candidate); err == nil && value > 0 {
			line = candidate
		}
	}
	return line
}
