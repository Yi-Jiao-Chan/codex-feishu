package feishu

import (
	"context"
	"errors"
	"net/url"
)

// TopicCardResult retains platform topic identity without changing the existing
// group gateway's string-returning methods.
type TopicCardResult struct{ MessageID, ThreadID string }

func (g *Gateway) ReplyTopicCard(ctx context.Context, parent, card, uuid string) (string, error) {
	r, err := g.ReplyTopicCardResult(ctx, parent, card, uuid)
	return r.MessageID, err
}

// ReplyTopicCardResult uses the official reply API with reply_in_thread=true.
// https://open.feishu.cn/document/server-docs/im-v1/message/reply.md
func (g *Gateway) ReplyTopicCardResult(ctx context.Context, parent, card, uuid string) (TopicCardResult, error) {
	if parent == "" {
		return TopicCardResult{}, errors.New("topic root required")
	}
	var r struct {
		Data struct {
			MessageID string `json:"message_id"`
			ThreadID  string `json:"thread_id"`
		} `json:"data"`
	}
	err := g.request(ctx, "POST", "/open-apis/im/v1/messages/"+url.PathEscape(parent)+"/reply", nil, map[string]any{"msg_type": "interactive", "content": card, "uuid": uuid, "reply_in_thread": true}, &r)
	if err == nil && r.Data.MessageID == "" {
		err = &Unknown{Operation: "topic reply response missing message_id"}
	}
	return TopicCardResult{MessageID: r.Data.MessageID, ThreadID: r.Data.ThreadID}, err
}

// TopicTargetRejected excludes malformed content, quota and transient limits.
// These official reply errors prove that the selected target cannot be used.
func TopicTargetRejected(err error) bool {
	var r *Rejected
	if !errors.As(err, &r) {
		return false
	}
	switch r.Code {
	case 230002, 230011, 230013, 230017, 230018, 230019, 230027, 230035, 230050:
		return true
	}
	return false
}
