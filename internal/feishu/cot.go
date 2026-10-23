package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"unicode/utf8"

	core "github.com/larksuite/oapi-sdk-go/v3/core"
)

const (
	MaxCOTEvents = 50
	// Count the serialized event, including content escaping, conservatively
	// against the platform's single-event length limit.
	MaxCOTEventBytes = 4096
)

// COTEvent carries a display-safe AG-UI event. Content is a JSON-encoded object,
// not raw model reasoning or an unfiltered tool payload.
type COTEvent struct {
	EventType string `json:"event_type"`
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp"`
}

type COTMessage struct {
	COTID     string `json:"cot_id"`
	MessageID string `json:"message_id"`
}

// CreateCOT creates a native progress message. Its endpoint has no verified
// idempotency key: callers must journal the operation and reconcile unknowns
// without retrying or creating a replacement message.
func (g *Gateway) CreateCOT(ctx context.Context, chatID, originMessageID string, replyInThread bool) (COTMessage, error) {
	if !readbackID(chatID) || originMessageID != "" && !readbackID(originMessageID) || replyInThread && originMessageID == "" {
		return COTMessage{}, errors.New("invalid COT target")
	}
	body := struct {
		ReceiveID       string `json:"receive_id"`
		OriginMessageID string `json:"origin_message_id,omitempty"`
		ReplyInThread   bool   `json:"reply_in_thread,omitempty"`
	}{chatID, originMessageID, replyInThread}
	var r struct {
		Data COTMessage `json:"data"`
	}
	if err := g.request(ctx, "POST", "/open-apis/im/v1/message_cot", core.QueryParams{"receive_id_type": {"chat_id"}}, body, &r); err != nil {
		return COTMessage{}, err
	}
	if !readbackID(r.Data.COTID) || !readbackID(r.Data.MessageID) {
		return COTMessage{}, &Unknown{Operation: "create COT response missing or invalid identity"}
	}
	return r.Data, nil
}

// AppendCOT sends one ordered batch; it never retries an ambiguous mutation.
func (g *Gateway) AppendCOT(ctx context.Context, messageID, cotID string, events []COTEvent) error {
	if !readbackID(messageID) || !readbackID(cotID) {
		return errors.New("invalid COT identity")
	}
	if len(events) == 0 || len(events) > MaxCOTEvents {
		return errors.New("COT batch requires 1 to 50 events")
	}
	for _, event := range events {
		if len(event.Content) > MaxCOTEventBytes {
			return errors.New("COT event exceeds serialized length limit")
		}
		if !readbackID(event.EventType) || event.Timestamp <= 0 || !utf8.ValidString(event.Content) {
			return errors.New("invalid COT event")
		}
		var content map[string]json.RawMessage
		if json.Unmarshal([]byte(event.Content), &content) != nil || content == nil {
			return errors.New("COT event content must be a JSON object")
		}
		encoded, err := json.Marshal(event)
		if err != nil || len(encoded) > MaxCOTEventBytes {
			return errors.New("COT event exceeds serialized length limit")
		}
	}
	return g.request(ctx, "PUT", "/open-apis/im/v1/message_cot", nil, struct {
		MessageID string     `json:"message_id"`
		COTID     string     `json:"cot_id"`
		Events    []COTEvent `json:"events"`
	}{messageID, cotID, events}, nil)
}

// CompleteCOT closes the platform lifecycle. The timeout reason is reserved for
// the platform's own recovery workers, so external callers use done or error.
func (g *Gateway) CompleteCOT(ctx context.Context, messageID, cotID, reason string) error {
	if !readbackID(messageID) || !readbackID(cotID) || reason != "done" && reason != "error" {
		return errors.New("invalid COT completion")
	}
	return g.request(ctx, "POST", "/open-apis/im/v1/message_cot/complete/"+url.PathEscape(cotID), core.QueryParams{"message_id": {messageID}, "reason": {reason}}, nil, nil)
}
