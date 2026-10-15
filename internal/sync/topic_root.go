package sync

import (
	"context"
	"errors"
	"fmt"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type TopicRootReader interface {
	ReadCardMessage(context.Context, string) (feishu.CardMessage, error)
}

var ErrTopicRootGone = errors.New("topic root is deleted or inaccessible")
var ErrTopicRootIncomplete = errors.New("topic root evidence unavailable")

// VerifyTopicRoot is GET-only. Reply success cannot prove the root still exists:
// Feishu can accept replies to a recalled root. Missing evidence blocks writes
// without declaring the target permanently broken. Group targets need no GET.
func VerifyTopicRoot(ctx context.Context, reader TopicRootReader, b store.Binding) error {
	if b.FollowMode != "topic" {
		return nil
	}
	if reader == nil || b.RootMessageID == "" || b.ChatID == "" {
		return ErrTopicRootIncomplete
	}
	root, err := reader.ReadCardMessage(ctx, b.RootMessageID)
	if err != nil {
		var rejected *feishu.Rejected
		if errors.As(err, &rejected) {
			// Official GET /im/v1/messages/:message_id errors: bot outside group,
			// unavailable user, missing permissions, invisible or deleted message.
			// https://open.feishu.cn/document/server-docs/im-v1/message/get.md
			switch rejected.Code {
			case 230002, 230013, 230027, 230050, 230110:
				return fmt.Errorf("%w: %w", ErrTopicRootGone, err)
			}
		}
		return fmt.Errorf("%w: %w", ErrTopicRootIncomplete, err)
	}
	if root.ID != b.RootMessageID || root.ChatID != b.ChatID || root.ParentID != nil && *root.ParentID != "" || root.RootID != nil && *root.RootID != "" && *root.RootID != root.ID || root.Deleted == nil {
		return ErrTopicRootIncomplete
	}
	if *root.Deleted {
		return ErrTopicRootGone
	}
	if b.PlatformThreadID != "" && (root.ThreadID == nil || *root.ThreadID != b.PlatformThreadID) {
		return ErrTopicRootIncomplete
	}
	return nil
}
