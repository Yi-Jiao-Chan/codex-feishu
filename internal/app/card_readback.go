package app

import (
	"context"

	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

// Confirmation reads only the original frozen card. It never reapplies a local
// command, answers a native question or regenerates mentions from current rules.
func reconcileFeedbackCard(ctx context.Context, s *store.Store, g messages.MessageGateway, kind, host, app, thread string, nowMS int64) (bool, error) {
	reader, ok := g.(messages.MessageReadbackGateway)
	if !ok {
		return false, nil
	}
	r, claimed, err := s.ClaimCardReadback(ctx, kind, host, app, thread, nowMS, 30000)
	if err != nil || !claimed {
		return false, err
	}
	remote, err := messages.FindMarkedCard(ctx, reader, messages.CardReadbackProof{FollowMode: r.Target.FollowMode, RootMessageID: r.Target.RootMessageID, PlatformThreadID: r.Target.PlatformThreadID, AppID: r.AppID, ChatID: r.ChatID, ParentMessageID: r.ParentMessageID, Card: r.Card, Marker: r.Marker})
	if err != nil || remote == "" {
		return false, err
	}
	if kind == "command" {
		err = s.FinishCommandReply(ctx, r.ID, "confirmed", remote)
	} else {
		err = s.FinishRequestReminder(ctx, r.ID, "confirmed", remote)
	}
	return true, err
}
