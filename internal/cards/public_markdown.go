package cards

import (
	"regexp"
	"strconv"
	"strings"
)

const maxPublicBodyElements = 64

// SafeMarkdown retains only structural quote prefixes from the original
// Markdown. Native tags remain escaped, including tags inside quoted code.
func SafeMarkdown(body string) string {
	var out strings.Builder
	var fence *messageFence
	fenceQuoteDepth, inlineWidth := 0, 0
	for _, line := range strings.SplitAfter(body, "\n") {
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
