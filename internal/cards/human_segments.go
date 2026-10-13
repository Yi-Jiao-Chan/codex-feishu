package cards

import (
	"fmt"
)

// SplitHuman preserves the request text while placing bounded attachment groups
// in the same human cards. Resource keys never become user-facing URLs.
func SplitHuman(source, body string, attachments []AttachmentView, limit int) ([]Segment, error) {
	if len(attachments) == 0 {
		return SplitMessage("人类发送", source, body, limit)
	}
	if limit == 0 {
		limit = DefaultMaxCardBytes
	}
	if limit < 0 {
		return nil, fmt.Errorf("human card limit must be positive")
	}
	var groups [][]AttachmentView
	for _, attachment := range attachments {
		if len(Human(source, "", []AttachmentView{attachment})) > limit {
			name := []rune(attachment.Name)
			if len(name) > 64 {
				name = name[:64]
			}
			attachment.Name = string(name) + "…（详情请回 Codex App 查看）"
			attachment.Path, attachment.ImageKey = "", ""
			if len(Human(source, "", []AttachmentView{attachment})) > limit {
				return nil, fmt.Errorf("card limit cannot hold attachment notice")
			}
		}
		if len(groups) == 0 {
			groups = append(groups, nil)
		}
		n := len(groups) - 1
		candidate := append(append([]AttachmentView(nil), groups[n]...), attachment)
		if len(candidate) > 32 || len(Human(source, "", candidate)) > limit {
			groups = append(groups, []AttachmentView{attachment})
		} else {
			groups[n] = candidate
		}
	}
	// Reserve the first group's complete wrapper before splitting the body. The
	// empty-body caption makes this conservative for a normal text paragraph.
	reserve := len(Human(source, "", groups[0])) - len(Message("人类发送", source, ""))
	parts, err := SplitMessage("人类发送", source, body, limit-reserve)
	if err != nil {
		// An attachment group may leave too little room for text. Keep the full
		// text first, then place these attachments on their own bounded cards.
		parts, err = SplitMessage("人类发送", source, body, limit)
		if err != nil {
			return nil, err
		}
		for _, group := range groups {
			parts = append(parts, Segment{Card: Human(source, "", group)})
		}
	} else {
		firstSource := source
		if len(parts) > 1 {
			firstSource += fmt.Sprintf(" · 第 1 / 共 %d 段", len(parts))
		}
		parts[0].Card = Human(firstSource, parts[0].Body, groups[0])
		for _, group := range groups[1:] {
			parts = append(parts, Segment{Card: Human(source, "", group)})
		}
	}
	for _, part := range parts {
		if len(part.Card) > limit {
			return nil, fmt.Errorf("human card exceeds byte limit")
		}
	}
	return parts, nil
}
