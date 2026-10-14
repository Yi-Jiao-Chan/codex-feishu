package cards

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// A Markdown block needs two JSON-escaped newlines instead of one inline space.
const MaxMentionBlockExtraBytes = 3

// ValidMentionID accepts only Feishu open IDs, never @all or markup.
func ValidMentionID(id string) bool {
	if !strings.HasPrefix(id, "ou_") || len(id) <= 3 {
		return false
	}
	for _, char := range id[3:] {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

// WithMentions puts validated reminders at the beginning of the first public
// paragraph. Public text must already come from the Safe-based card renderers.
func WithMentions(base string, ids []string) (string, error) {
	if len(ids) == 0 {
		return base, nil
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(base), &card); err != nil {
		return "", errors.New("invalid base notification card")
	}
	body, ok := card["body"].(map[string]any)
	if !ok || card["schema"] != "2.0" {
		return "", errors.New("notification requires Card JSON 2.0")
	}
	elements, ok := body["elements"].([]any)
	if !ok {
		return "", errors.New("notification card requires body elements")
	}
	seen := map[string]bool{}
	var mentions []string
	for _, id := range ids {
		if !ValidMentionID(id) {
			return "", errors.New("notification target must be a Feishu open ID")
		}
		if !seen[id] {
			mentions = append(mentions, Mention(id))
			seen[id] = true
		}
	}
	if len(elements) == 0 {
		return "", errors.New("notification card requires a public text element")
	}
	first, ok := elements[0].(map[string]any)
	if !ok || first["tag"] != "markdown" {
		return "", errors.New("notification card requires leading public markdown")
	}
	content, ok := first["content"].(string)
	if !ok {
		return "", errors.New("notification card requires public markdown content")
	}
	first["content"] = mentionPrefix(strings.Join(mentions, " "), content)
	encoded, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	if len(encoded) > DefaultMaxCardBytes {
		return "", errors.New("notification card exceeds byte limit")
	}
	return string(encoded), nil
}

var inlineBlockPrefix = regexp.MustCompile(`^(#{1,6}[ \t]+|[-+*][ \t]+|[0-9]+[.)][ \t]+)`)

func mentionPrefix(mentions, content string) string {
	text := strings.TrimLeft(content, "\r\n")
	firstLine, _, _ := strings.Cut(text, "\n")
	_, _, quoteDepth := publicQuotePrefix(firstLine, -1)
	if openingMessageFence(firstLine) != nil || strings.HasPrefix(text, "|") || quoteDepth > 0 || strings.HasPrefix(text, "    ") || strings.HasPrefix(text, "\t") {
		// Code, tables and quoted blocks need their own line start. Keeping
		// the reminder in a paragraph preserves their original Markdown.
		return mentions + "\n\n" + content
	}
	if prefix := inlineBlockPrefix.FindString(text); prefix != "" {
		return prefix + mentions + " " + text[len(prefix):]
	}
	return mentions + " " + text
}
