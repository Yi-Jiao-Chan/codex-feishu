// Package sync reconciles public Codex content and its Feishu deliveries.
package sync

import (
	"context"
	"errors"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type MessageGateway interface {
	SendCard(context.Context, string, string, string, string) (string, error)
	ReplyCard(context.Context, string, string, string) (string, error)
	UpdateCard(context.Context, string, string) error
}

// Outbound dispatches already-rendered public cards. The composition root owns
// notification decisions and must not place an expired @ list in a mirror.
// Notification rendering and verified unknown-attempt reconciliation are not
// implemented in this foundation; no ambiguous request is automatically retried.
type Outbound struct {
	Store   *store.Store
	Gateway MessageGateway
}

type DeliveryResult struct {
	AttemptID, Key, Kind, TurnID, State, MessageID string
}

// DispatchOne performs at most one remote write. An empty AttemptID means no
// currently dispatchable work. The journal is committed before invoking the
// gateway; a failed local confirmation leaves it frozen for startup recovery.
func (o Outbound) DispatchOne(ctx context.Context, host, thread string, nowMS int64) (DeliveryResult, error) {
	if o.Store == nil || o.Gateway == nil {
		return DeliveryResult{}, errors.New("outbound requires store and gateway")
	}
	ready, err := o.Store.ReadyMirrors(ctx, host, thread)
	if err != nil {
		return DeliveryResult{}, err
	}
	for _, m := range ready {
		a, claimed, err := o.Store.ClaimMirror(ctx, host, thread, m.Key, m.Revision, nowMS)
		if err != nil {
			return DeliveryResult{}, err
		}
		if !claimed {
			continue
		}
		result := DeliveryResult{AttemptID: a.ID, Key: m.Key, Kind: m.Kind, TurnID: m.TurnID}
		var remoteID string
		var sendErr error
		switch a.Operation {
		case "send":
			remoteID, sendErr = o.Gateway.SendCard(ctx, "chat_id", a.ChatID, a.Card, a.ID)
		case "reply":
			remoteID, sendErr = o.Gateway.ReplyCard(ctx, a.ParentMessageID, a.Card, a.ID)
		case "update":
			sendErr = o.Gateway.UpdateCard(ctx, a.TargetMessageID, a.Card)
			remoteID = a.TargetMessageID
		default:
			// Schema constraints and ClaimMirror make this unreachable.
			sendErr = errors.New("unsupported persisted outbound operation")
		}
		result.State = "confirmed"
		if sendErr != nil || remoteID == "" {
			result.State, remoteID = "unknown", ""
			var rejected *feishu.Rejected
			if errors.As(sendErr, &rejected) {
				result.State = "rejected"
			}
		}
		if err := o.Store.FinishMirrorAttempt(ctx, a.ID, result.State, remoteID); err != nil {
			// The gateway may already have accepted the operation. Never
			// turn a failed local commit into another remote request.
			result.State = "unknown"
			return result, err
		}
		result.MessageID = remoteID
		return result, nil
	}
	return DeliveryResult{}, nil
}
