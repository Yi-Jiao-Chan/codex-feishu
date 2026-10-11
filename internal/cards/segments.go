package cards

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DefaultMaxCardBytes bounds the complete serialized card, not just its text.
const DefaultMaxCardBytes = 20000

// Segment keeps the original bytes separate from display-only fence wrappers.
type Segment struct {
	Raw  string
	Body string
	Card string
}

// SplitMessage splits public message text into stable, byte-bounded cards.
// A zero limit uses DefaultMaxCardBytes. Concatenating Raw reconstructs body.
// Fence delimiter lines are atomic; a limit unable to hold one returns an error.
func SplitMessage(role, source, body string, maxCardBytes int) ([]Segment, error) {
	if maxCardBytes == 0 {
		maxCardBytes = DefaultMaxCardBytes
	}
	if maxCardBytes < 0 {
		return nil, fmt.Errorf("card byte limit must be positive")
	}
	if !utf8.ValidString(role) || !utf8.ValidString(source) || !utf8.ValidString(body) {
		return nil, fmt.Errorf("message must contain valid UTF-8")
	}
	card := Message(role, source, body)
	if len(card) <= maxCardBytes {
		return []Segment{{Raw: body, Body: body, Card: card}}, nil
	}
	boundaries := messageBoundaries(body)
	// Reserve both counters at the same width. If the resulting count needs
	// another digit, repeat with that width; the actual footer cannot be larger.
	for digits := 1; ; digits++ {
		counter := strings.Repeat("9", digits)
		budgetSource := source + " · 第 " + counter + " / 共 " + counter + " 段"
		parts, err := splitMessageParts(body, boundaries, len(Message(role, budgetSource, "")), maxCardBytes)
		if err != nil {
			return nil, err
		}
		if len(strconv.Itoa(len(parts))) > digits {
			continue
		}
		for i := range parts {
			segmentSource := source + fmt.Sprintf(" · 第 %d / 共 %d 段", i+1, len(parts))
			parts[i].Card = Message(role, segmentSource, parts[i].Body)
			if len(parts[i].Card) > maxCardBytes {
				return nil, fmt.Errorf("serialized segment exceeds card byte limit")
			}
		}
		return parts, nil
	}
}

type messageFence struct {
	marker byte
	width  int
	opener string
}

type messageBoundary struct {
	offset int
	bytes  int // Cumulative Safe + JSON string encoding cost, without quotes.
	fence  *messageFence
	kind   int // 0: rune; 1: line; 2: paragraph.
}

func messageBoundaries(body string) []messageBoundary {
	points := []messageBoundary{{}}
	var fence *messageFence
	encoded := 0
	for start := 0; start < len(body); {
		end := len(body)
		if n := strings.IndexByte(body[start:], '\n'); n >= 0 {
			end = start + n + 1
		}
		line := body[start:end]
		atomic := false
		if fence == nil {
			if opened := openingMessageFence(line); opened != nil {
				fence, atomic = opened, true
			}
		} else if closesMessageFence(line, fence) {
			fence, atomic = nil, true
		}
		kind := 1
		if strings.Trim(line, " \t\r\n") == "" {
			kind = 2
		}
		if atomic {
			encoded += markdownJSONBytes(line)
			points = append(points, messageBoundary{end, encoded, fence, kind})
		} else {
			for pos, r := range line {
				encoded += markdownJSONRuneBytes(r)
				offset := start + pos + utf8.RuneLen(r)
				// Reopening a block puts a mid-line continuation at column
				// zero. Do not make a code suffix look like a closing fence.
				if offset < end && fence != nil && closesMessageFence(line[offset-start:], fence) {
					continue
				}
				pointKind := 0
				if offset == end {
					pointKind = kind
				}
				points = append(points, messageBoundary{offset, encoded, fence, pointKind})
			}
		}
		start = end
	}
	return points
}

func splitMessageParts(body string, points []messageBoundary, overhead, limit int) ([]Segment, error) {
	var parts []Segment
	for start := 0; start < len(points)-1; {
		prefix := ""
		if f := points[start].fence; f != nil {
			prefix = f.opener + "\n"
		}
		base := overhead + markdownJSONBytes(prefix)
		last, line, paragraph := start, start, start
		for end := start + 1; end < len(points); end++ {
			size := base + points[end].bytes - points[start].bytes
			if size > limit {
				break
			}
			suffix := messageFenceSuffix(body, points[end])
			if size+markdownJSONBytes(suffix) > limit {
				// The real closing delimiter may remove the synthetic
				// suffix and allow a later boundary to fit.
				continue
			}
			last = end
			if points[end].kind >= 1 {
				line = end
			}
			if points[end].kind == 2 {
				paragraph = end
			}
		}
		if last == start {
			return nil, fmt.Errorf("card byte limit %d cannot hold text or fence at byte %d", limit, points[start].offset)
		}
		end := last
		if last != len(points)-1 {
			if paragraph > start {
				end = paragraph
			} else if line > start {
				end = line
			}
		}
		raw := body[points[start].offset:points[end].offset]
		parts = append(parts, Segment{Raw: raw, Body: prefix + raw + messageFenceSuffix(body, points[end])})
		start = end
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("card byte limit %d cannot hold message footer", limit)
	}
	return parts, nil
}

func messageFenceSuffix(body string, point messageBoundary) string {
	if point.fence == nil {
		return ""
	}
	separator := ""
	if point.offset == 0 || body[point.offset-1] != '\n' {
		separator = "\n"
	}
	return separator + strings.Repeat(string(point.fence.marker), point.fence.width)
}

// Fences follow Markdown's up-to-three-space indentation and matching marker
// rules. Shorter runs and runs with trailing code remain inside the block.
func messageFenceLine(line string) (rest string, marker byte, width int) {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	indent := 0
	for indent < len(line) && line[indent] == ' ' {
		indent++
	}
	if indent > 3 || indent == len(line) {
		return "", 0, 0
	}
	marker = line[indent]
	if marker != '`' && marker != '~' {
		return "", 0, 0
	}
	end := indent
	for end < len(line) && line[end] == marker {
		end++
	}
	return line[end:], marker, end - indent
}

func openingMessageFence(line string) *messageFence {
	rest, marker, width := messageFenceLine(line)
	if width < 3 || (marker == '`' && strings.ContainsRune(rest, '`')) {
		return nil
	}
	return &messageFence{marker: marker, width: width, opener: strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")}
}

func closesMessageFence(line string, fence *messageFence) bool {
	rest, marker, width := messageFenceLine(line)
	return marker == fence.marker && width >= fence.width && strings.Trim(rest, " \t") == ""
}

func markdownJSONBytes(s string) int {
	n := 0
	for _, r := range s {
		n += markdownJSONRuneBytes(r)
	}
	return n
}

// encoding/json escapes HTML and U+2028/U+2029 by default. Safe first turns
// angle brackets into entities, whose ampersands JSON then escapes as well.
func markdownJSONRuneBytes(r rune) int {
	switch r {
	case '<', '>':
		return 9 // \u0026lt; or \u0026gt;
	case '&', '\u2028', '\u2029':
		return 6
	case '"', '\\', '\b', '\f', '\n', '\r', '\t':
		return 2
	default:
		if r < 0x20 {
			return 6
		}
		return utf8.RuneLen(r)
	}
}
