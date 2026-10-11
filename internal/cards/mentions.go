package cards

import (
	"encoding/json"
	"errors"
	"strings"
)

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

// WithMentions appends a bridge-owned mention element to a base Card JSON 2.0
// document. Public text must already come from the Safe-based card renderers.
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
	body["elements"] = append(elements, element{"tag": "markdown", "content": strings.Join(mentions, " "), "text_size": "notation"})
	encoded, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	if len(encoded) > DefaultMaxCardBytes {
		return "", errors.New("notification card exceeds byte limit")
	}
	return string(encoded), nil
}
