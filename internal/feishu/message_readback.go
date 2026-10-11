package feishu

import (
	"context"
	"errors"
	"net/url"

	core "github.com/larksuite/oapi-sdk-go/v3/core"
)

// Readback data is private worker evidence, never a diagnostic payload. Pointer
// fields distinguish missing platform evidence from documented zero values.
type CardMessage struct {
	ID, ChatID, Type, SenderID, SenderIDType, SenderType string  `json:"-"`
	ParentID, RootID, ThreadID                           *string `json:"-"`
	Deleted                                              *bool   `json:"-"`
	Content                                              string  `json:"-"`
}

type CardMessagePage struct {
	Items   []CardMessage `json:"-"`
	HasMore bool          `json:"-"`
	Token   string        `json:"-"`
}

type messageReadbackWire struct {
	ID       string  `json:"message_id"`
	ChatID   string  `json:"chat_id"`
	Type     string  `json:"msg_type"`
	ParentID *string `json:"parent_id"`
	RootID   *string `json:"root_id"`
	ThreadID *string `json:"thread_id"`
	Deleted  *bool   `json:"deleted"`
	Sender   struct {
		ID     string `json:"id"`
		IDType string `json:"id_type"`
		Type   string `json:"sender_type"`
	} `json:"sender"`
	Body struct {
		Content string `json:"content"`
	} `json:"body"`
}

func (w messageReadbackWire) record() CardMessage {
	return CardMessage{ID: w.ID, ChatID: w.ChatID, Type: w.Type, ParentID: w.ParentID, RootID: w.RootID, ThreadID: w.ThreadID, Deleted: w.Deleted, SenderID: w.Sender.ID, SenderIDType: w.Sender.IDType, SenderType: w.Sender.Type, Content: w.Body.Content}
}

func readbackID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (g *Gateway) ReadCardMessage(ctx context.Context, id string) (CardMessage, error) {
	if !readbackID(id) {
		return CardMessage{}, errors.New("invalid readback message ID")
	}
	var r struct {
		Data struct {
			Items []messageReadbackWire `json:"items"`
		} `json:"data"`
	}
	if err := g.request(ctx, "GET", "/open-apis/im/v1/messages/"+url.PathEscape(id), core.QueryParams{"card_msg_content_type": {"user_card_content"}}, nil, &r); err != nil {
		return CardMessage{}, err
	}
	var selected CardMessage
	count := 0
	for _, item := range r.Data.Items {
		if item.ID == id {
			selected = item.record()
			count++
		}
	}
	// Forwarded messages can include child records. Select the requested ID,
	// and reject missing/duplicate identities rather than taking the first row.
	if count != 1 {
		return CardMessage{}, &Unknown{Operation: "readback message identity unavailable"}
	}
	return selected, nil
}

func (g *Gateway) ListCardMessages(ctx context.Context, containerType, containerID, cursor string) (CardMessagePage, error) {
	if containerType != "chat" && containerType != "thread" || !readbackID(containerID) || len(cursor) > 4096 {
		return CardMessagePage{}, errors.New("invalid readback container or cursor")
	}
	var r struct {
		Data struct {
			Items []messageReadbackWire `json:"items"`
			More  *bool                 `json:"has_more"`
			Token string                `json:"page_token"`
		} `json:"data"`
	}
	q := core.QueryParams{"container_id_type": {containerType}, "container_id": {containerID}, "page_size": {"50"}, "sort_type": {"ByCreateTimeDesc"}, "card_msg_content_type": {"user_card_content"}}
	if cursor != "" {
		q["page_token"] = []string{cursor}
	}
	if err := g.request(ctx, "GET", "/open-apis/im/v1/messages", q, nil, &r); err != nil {
		return CardMessagePage{}, err
	}
	if r.Data.More == nil || len(r.Data.Items) > 50 || *r.Data.More && (r.Data.Token == "" || r.Data.Token == cursor || len(r.Data.Token) > 4096) {
		return CardMessagePage{}, &Unknown{Operation: "readback message page incomplete"}
	}
	p := CardMessagePage{HasMore: *r.Data.More, Token: r.Data.Token}
	for _, item := range r.Data.Items {
		p.Items = append(p.Items, item.record())
	}
	return p, nil
}
