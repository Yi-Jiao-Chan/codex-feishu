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
	bodyBytes := []byte(body)
	var imageEdits []publicImageEdit
	if strings.Contains(body, "![") {
		imageEdits = publicSafeImageEdits(bodyBytes)
	}
	boundaries := messageBoundaries(body, imageEdits)
	// Reserve both counters at the same width. If the resulting count needs
	// another digit, repeat with that width; the actual footer cannot be larger.
	for digits := 1; ; digits++ {
		counter := strings.Repeat("9", digits)
		budgetSource := source + " · 第 " + counter + " / 共 " + counter + " 段"
		render := func(body string) string { return Message(role, budgetSource, body) }
		parts, err := splitMessageParts(bodyBytes, boundaries, imageEdits, render, maxCardBytes)
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

func messageBoundaries(body string, imageEdits []publicImageEdit) []messageBoundary {
	points := []messageBoundary{{}}
	emails := publicEmail.FindAllStringIndex(body, -1)
	email, imageEdit, protectedUntil := 0, 0, 0
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
				for imageEdit < len(imageEdits) && imageEdits[imageEdit].start < offset {
					if imageEdits[imageEdit].stop > protectedUntil {
						protectedUntil = imageEdits[imageEdit].stop
					}
					imageEdit++
				}
				for email < len(emails) && offset >= emails[email][1] {
					email++
				}
				// Keep the whole address in one display segment so Safe can
				// mask it. Image replacements similarly remain atomic so a
				// split cannot turn a safe placeholder back into partial syntax.
				// Raw still reconstructs the original input in both cases.
				insideEmail := email < len(emails) && offset > emails[email][0] && offset < emails[email][1]
				if insideEmail || offset < protectedUntil {
					continue
				}
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

func splitMessageParts(source []byte, points []messageBoundary, imageEdits []publicImageEdit, render func(string) string, limit int) ([]Segment, error) {
	body := string(source)
	var parts []Segment
	overhead := len(render(""))
	for start := 0; start < len(points)-1; {
		prefix := ""
		if f := points[start].fence; f != nil {
			prefix = f.opener + "\n"
		}
		base := overhead + markdownJSONBytes(prefix)
		last := start
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
		}
		// The single-Markdown estimate is only a search window. Native body
		// blocks have their own JSON overhead, and safe quote syntax can cost
		// less than the estimate. Always size the real renderer, including at
		// least the next atomic boundary when the estimate finds no candidate.
		if last == start {
			last = start + 1
		}
		display := func(end int) string {
			return prefix + applyPublicImageEdits(source, points[start].offset, points[end].offset, imageEdits) + messageFenceSuffix(body, points[end])
		}
		fits := func(end int) bool { return len(render(display(end))) <= limit }
		if !fits(last) {
			// Refining a window uses logarithmically many complete renders,
			// rather than serializing the card once per rune.
			low, high, best := start+1, last-1, start
			for low <= high {
				mid := low + (high-low)/2
				if fits(mid) {
					best, low = mid, mid+1
				} else {
					high = mid - 1
				}
			}
			last = best
		}
		if last == start {
			return nil, fmt.Errorf("card byte limit %d cannot hold text or fence at byte %d", limit, points[start].offset)
		}
		end := last
		if last != len(points)-1 {
			// Keep paragraph/line preference, checking the selected boundary
			// too: a display-only fence wrapper changes its serialized cost.
			for _, kind := range []int{2, 1} {
				for candidate := last; candidate > start; candidate-- {
					if points[candidate].kind >= kind && fits(candidate) {
						end = candidate
						break
					}
				}
				if end != last || points[end].kind >= kind {
					break
				}
			}
		}
		raw := body[points[start].offset:points[end].offset]
		parts = append(parts, Segment{Raw: raw, Body: display(end)})
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
