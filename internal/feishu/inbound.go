package feishu

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// InboundPart preserves the order of text and structured message resources.
// ResourceKey is an opaque Feishu key, never a URL or a local filename.
type InboundPart struct {
	Type        string
	Text        string
	ResourceKey string
	Name        string
}

type InboundMessage struct {
	Parts []InboundPart
	Text  string
}

var (
	ErrInvalidInbound     = errors.New("invalid Feishu inbound message content")
	ErrUnsupportedInbound = errors.New("unsupported Feishu inbound message content")
)

const maxInboundContent = 1 << 20
const maxInboundNodes = 4096

type inboundMention struct {
	name string
	bot  bool
}

var inboundMentionPlaceholder = regexp.MustCompile(`@_user_[0-9]+`)

// ParseInbound normalizes content only. Identity, structured @ authorization,
// binding and conversation checks remain the caller's responsibility.
func ParseInbound(msg *im.EventMessage, bot string) (InboundMessage, error) {
	if msg == nil || msg.MessageType == nil || msg.Content == nil || len(*msg.Content) > maxInboundContent {
		return InboundMessage{}, ErrInvalidInbound
	}
	mentions := map[string]inboundMention{}
	if len(msg.Mentions) > maxInboundNodes {
		return InboundMessage{}, ErrInvalidInbound
	}
	for _, m := range msg.Mentions {
		if m == nil || m.Id == nil || m.Id.OpenId == nil {
			continue
		}
		name := *m.Id.OpenId
		if m.Name != nil && *m.Name != "" {
			name = *m.Name
		}
		if len(name) > 4096 || len(*m.Id.OpenId) > 512 || m.Key != nil && len(*m.Key) > 512 {
			return InboundMessage{}, ErrInvalidInbound
		}
		v := inboundMention{name: name, bot: bot != "" && *m.Id.OpenId == bot}
		mentions[*m.Id.OpenId] = v
		if m.Key != nil && *m.Key != "" {
			mentions[*m.Key] = v
		}
	}
	var out InboundMessage
	var normalizationErr error
	var allText, partText strings.Builder
	flushText := func() {
		if partText.Len() > 0 {
			out.Parts = append(out.Parts, InboundPart{Type: "text", Text: partText.String()})
			partText.Reset()
		}
	}
	appendText := func(text string) {
		if normalizationErr != nil {
			return
		}
		if len(text) > maxInboundContent-allText.Len() {
			normalizationErr = ErrInvalidInbound
			return
		}
		if text == "" {
			return
		}
		allText.WriteString(text)
		partText.WriteString(text)
	}
	appendMentionText := func(text string) {
		text, err := replaceInboundMentions(text, mentions)
		if err != nil {
			normalizationErr = err
			return
		}
		appendText(text)
	}
	switch *msg.MessageType {
	case "text":
		var c struct {
			Text *string `json:"text"`
		}
		if json.Unmarshal([]byte(*msg.Content), &c) != nil || c.Text == nil {
			return InboundMessage{}, ErrInvalidInbound
		}
		appendMentionText(*c.Text)
	case "image", "file":
		var c struct {
			ImageKey string `json:"image_key"`
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		if json.Unmarshal([]byte(*msg.Content), &c) != nil {
			return InboundMessage{}, ErrInvalidInbound
		}
		key := c.ImageKey
		if *msg.MessageType == "file" {
			key = c.FileKey
		}
		if *msg.MessageType == "file" && c.ImageKey != "" || *msg.MessageType == "image" && c.FileKey != "" {
			return InboundMessage{}, ErrUnsupportedInbound
		}
		if !validResourceIdentifier(key) {
			return InboundMessage{}, ErrInvalidInbound
		}
		out.Parts = append(out.Parts, InboundPart{Type: *msg.MessageType, ResourceKey: key, Name: c.FileName})
	case "post":
		var c struct {
			Title     string          `json:"title"`
			Content   json.RawMessage `json:"content"`
			ContentV2 json.RawMessage `json:"content_v2"`
			Files     json.RawMessage `json:"files"`
		}
		if json.Unmarshal([]byte(*msg.Content), &c) != nil {
			return InboundMessage{}, ErrInvalidInbound
		}
		content := c.Content
		if len(content) == 0 || string(content) == "null" {
			content = c.ContentV2
		}
		var rows [][]json.RawMessage
		if len(content) == 0 || json.Unmarshal(content, &rows) != nil || rows == nil {
			return InboundMessage{}, ErrInvalidInbound
		}
		if len(rows) > maxInboundNodes {
			return InboundMessage{}, ErrInvalidInbound
		}
		if c.Title != "" {
			appendText(c.Title)
			if len(rows) > 0 {
				appendText("\n")
			}
		}
		nodes := 0
		for rowIndex, row := range rows {
			if rowIndex > 0 {
				appendText("\n")
			}
			for _, raw := range row {
				nodes++
				if nodes > maxInboundNodes {
					return InboundMessage{}, ErrInvalidInbound
				}
				var node struct {
					Tag      string `json:"tag"`
					Text     string `json:"text"`
					Href     string `json:"href"`
					UserID   string `json:"user_id"`
					UserName string `json:"user_name"`
					ImageKey string `json:"image_key"`
					FileKey  string `json:"file_key"`
					Language string `json:"language"`
				}
				if json.Unmarshal(raw, &node) != nil || node.Tag == "" {
					return InboundMessage{}, ErrInvalidInbound
				}
				if node.FileKey != "" || node.ImageKey != "" && node.Tag != "img" {
					return InboundMessage{}, ErrUnsupportedInbound
				}
				switch node.Tag {
				case "text", "md":
					appendMentionText(node.Text)
				case "a":
					appendText(node.Text)
					if node.Href != "" && node.Href != node.Text {
						appendText(" (" + node.Href + ")")
					}
				case "at":
					m, known := mentions[node.UserID]
					if known && m.bot {
						continue
					}
					name := node.UserName
					if known {
						name = m.name
					} else if name == "" {
						name = node.UserID
					}
					if name == "" {
						return InboundMessage{}, ErrInvalidInbound
					}
					appendText("@" + name)
				case "img":
					if !validResourceIdentifier(node.ImageKey) {
						return InboundMessage{}, ErrInvalidInbound
					}
					flushText()
					out.Parts = append(out.Parts, InboundPart{Type: "image", ResourceKey: node.ImageKey})
				case "code_block":
					// A fence longer than any run in the source keeps embedded fences intact.
					longest, run := 0, 0
					for _, char := range node.Text {
						if char == '`' {
							run++
							longest = max(longest, run)
						} else {
							run = 0
						}
					}
					fence := strings.Repeat("`", max(3, longest+1))
					language := node.Language
					if strings.ContainsAny(language, "\r\n`") {
						return InboundMessage{}, ErrInvalidInbound
					}
					appendText(fence + language + "\n" + node.Text + "\n" + fence)
				case "hr":
					appendText("\n---\n")
				default:
					return InboundMessage{}, ErrUnsupportedInbound
				}
			}
		}
		if len(c.Files) != 0 {
			// Observed Feishu posts keep file cards in a sibling array. The
			// content/content_v2 rows contain only the accompanying text/images.
			var files []struct {
				Key      string `json:"file_key"`
				Name     string `json:"file_name"`
				IsFolder *bool  `json:"is_folder"`
			}
			if json.Unmarshal(c.Files, &files) != nil || files == nil || len(files) > maxInboundNodes-nodes {
				return InboundMessage{}, ErrInvalidInbound
			}
			flushText()
			for _, file := range files {
				if file.IsFolder == nil {
					return InboundMessage{}, ErrInvalidInbound
				}
				if *file.IsFolder {
					return InboundMessage{}, ErrUnsupportedInbound
				}
				if !validResourceIdentifier(file.Key) || strings.TrimSpace(file.Name) == "" {
					return InboundMessage{}, ErrInvalidInbound
				}
				out.Parts = append(out.Parts, InboundPart{Type: "file", ResourceKey: file.Key, Name: file.Name})
			}
		}
	default:
		return InboundMessage{}, ErrUnsupportedInbound
	}
	if normalizationErr != nil {
		return InboundMessage{}, normalizationErr
	}
	flushText()
	out.Text = allText.String()
	return out, nil
}

func replaceInboundMentions(text string, mentions map[string]inboundMention) (string, error) {
	var out strings.Builder
	for {
		match := inboundMentionPlaceholder.FindStringIndex(text)
		if match == nil {
			if len(text) > maxInboundContent-out.Len() {
				return "", ErrInvalidInbound
			}
			out.WriteString(text)
			return out.String(), nil
		}
		replacement := text[match[0]:match[1]]
		if m, known := mentions[replacement]; known {
			replacement = ""
			if !m.bot {
				replacement = "@" + m.name
			}
		}
		if match[0]+len(replacement) > maxInboundContent-out.Len() {
			return "", ErrInvalidInbound
		}
		out.WriteString(text[:match[0]])
		out.WriteString(replacement)
		text = text[match[1]:]
	}
}

func validResourceIdentifier(s string) bool {
	if s == "" || len(s) > 512 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
