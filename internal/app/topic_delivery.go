package app

import (
	"context"
	"errors"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

// replyFollowAttempt sends only the target frozen by the original claim. It
// cannot change the parent, choose a latest topic, or fall back to SendCard.
func replyFollowAttempt(ctx context.Context, s *store.Store, g messages.MessageGateway, id, host, thread, chat, parent, card string) (string, error) {
	target, err := s.FollowAttemptTarget(ctx, id)
	if err != nil {
		return "", err
	}
	if target.HostID != host || target.ThreadID != thread || target.ChatID != chat || target.ParentMessageID != parent {
		return "", store.ErrConflict
	}
	if target.FollowMode != "topic" {
		return g.ReplyCard(ctx, parent, card, id)
	}
	if thread != "" {
		b, err := s.Binding(ctx, host, thread)
		if err != nil {
			return "", err
		}
		if b.RepairRequired {
			return "", store.ErrTargetRepair
		}
		if b.FollowMode != "topic" || b.ChatID != chat || b.RootMessageID != target.RootMessageID || target.PlatformThreadID != "" && b.PlatformThreadID != target.PlatformThreadID {
			return "", store.ErrConflict
		}
	}
	gateway, ok := g.(messages.TopicMessageGateway)
	if !ok {
		return "", errors.New("topic reply gateway unavailable")
	}
	reader, _ := g.(messages.TopicRootReader)
	rootBinding := store.Binding{HostID: host, ThreadID: thread, ChatID: chat, FollowMode: "topic", RootMessageID: target.RootMessageID, PlatformThreadID: target.PlatformThreadID}
	if err := messages.VerifyTopicRoot(ctx, reader, rootBinding); err != nil {
		if thread != "" && errors.Is(err, messages.ErrTopicRootGone) {
			if persistErr := s.MarkBindingRepair(ctx, host, thread, "topic_root_unavailable"); persistErr != nil {
				return "", persistErr
			}
		}
		return "", err
	}
	var remote, platform string
	if detailed, ok := g.(messages.TopicResultGateway); ok {
		result, e := detailed.ReplyTopicCardResult(ctx, parent, card, id)
		remote, platform, err = result.MessageID, result.ThreadID, e
	} else {
		remote, err = gateway.ReplyTopicCard(ctx, parent, card, id)
	}
	if thread == "" && err == nil && platform != "" && target.PlatformThreadID != "" && platform != target.PlatformThreadID {
		return "", store.ErrConflict
	}
	if thread != "" && feishu.TopicTargetRejected(err) {
		if persistErr := s.MarkBindingRepair(ctx, host, thread, "topic_target_rejected"); persistErr != nil {
			return "", persistErr
		}
	}
	if thread != "" && err == nil && remote != "" && platform != "" {
		if persistErr := s.RecordBindingPlatformThread(ctx, host, thread, platform); persistErr != nil {
			if errors.Is(persistErr, store.ErrConflict) {
				if markErr := s.MarkBindingRepair(ctx, host, thread, "topic_identity_conflict"); markErr != nil {
					return "", markErr
				}
			}
			return "", persistErr
		}
	}
	return remote, err
}
