// Package sync reconciles public Codex content and its Feishu deliveries.
package sync

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type MessageGateway interface {
	SendCard(context.Context, string, string, string, string) (string, error)
	ReplyCard(context.Context, string, string, string) (string, error)
	UpdateCard(context.Context, string, string) error
}

type MembershipGateway interface {
	MemberIDs(context.Context, string) ([]string, error)
}

// Outbound dispatches public cards. Optional notifications must have been
// scheduled at first observation. Membership is read outside SQL transactions;
// missing proof suppresses mentions while body delivery continues.
type Outbound struct {
	Store                 *store.Store
	Gateway               MessageGateway
	BeginWrite            func(context.Context, store.Binding) (context.Context, func(), error)
	Mentions              *store.MentionOptions
	Membership            MembershipGateway
	Readback              MessageReadbackGateway
	MinProgressIntervalMS int64
	Now                   func() time.Time // Clock at response receipt, not request start.
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
	var readbackErr error
	if result, err := o.ReconcileOne(ctx, host, thread, nowMS); result.AttemptID != "" {
		return result, err
	} else {
		// A failed GET has a persisted cooldown. It must not starve unrelated
		// ready documents; the original unknown target stays unclaimable.
		readbackErr = err
	}
	ready, err := o.Store.ReadyMirrorsAt(ctx, host, thread, nowMS)
	if err != nil {
		return DeliveryResult{}, err
	}
	for _, m := range ready {
		options := store.MentionOptions{}
		if o.Mentions != nil {
			options = *o.Mentions
			options.ChatID, options.MemberIDs = "", nil
			if options.Enabled && m.EventType != "" && m.RemoteID == "" && o.Membership != nil {
				binding, err := o.Store.Binding(ctx, host, thread)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return DeliveryResult{}, err
				}
				if err == nil {
					members, err := o.Membership.MemberIDs(ctx, binding.ChatID)
					if err == nil {
						options.ChatID, options.MemberIDs = binding.ChatID, members
					}
				}
			}
		}
		claimAt := nowMS
		// Membership may paginate or wait on the network across a quiet-hours
		// boundary. Formal runtime supplies its clock; recheck at claim time.
		if o.Now != nil {
			if current := o.Now().UnixMilli(); current > claimAt {
				claimAt = current
			}
		}
		result, claimed, err := o.claimAndDispatch(ctx, host, thread, m, claimAt, options)
		if err != nil {
			return result, err
		}
		if claimed {
			return result, nil
		}
	}
	return DeliveryResult{}, readbackErr
}

// claimAndDispatch owns one candidate's write lease from its durable claim
// through the single gateway operation and its local result save. A lease is
// intentionally acquired per candidate so a later candidate cannot reuse a
// grant that covered an earlier write.
func (o Outbound) claimAndDispatch(ctx context.Context, host, thread string, m store.Mirror, claimAt int64, options store.MentionOptions) (DeliveryResult, bool, error) {
	leaseCtx := ctx
	release := func() {}
	if o.BeginWrite != nil {
		binding, err := o.Store.Binding(ctx, host, thread)
		if err != nil {
			return DeliveryResult{}, false, err
		}
		leaseCtx, release, err = o.BeginWrite(ctx, binding)
		if err != nil {
			if release != nil {
				release()
			}
			return DeliveryResult{}, false, err
		}
		if leaseCtx == nil {
			if release != nil {
				release()
			}
			return DeliveryResult{}, false, errors.New("outbound write lease unavailable")
		}
		if release == nil {
			return DeliveryResult{}, false, errors.New("outbound write lease unavailable")
		}
	}
	defer release()
	if err := leaseCtx.Err(); err != nil {
		return DeliveryResult{}, false, err
	}
	// Verification authorization can perform network checks across a quiet
	// boundary too. Recompute notification time after it, before freezing wire.
	if o.Now != nil {
		claimAt = max(claimAt, o.Now().UnixMilli())
	}
	a, claimed, err := o.Store.ClaimMirrorWithMentions(leaseCtx, host, thread, m.Key, m.Revision, claimAt, options, o.MinProgressIntervalMS)
	if err != nil || !claimed {
		return DeliveryResult{}, claimed, err
	}
	result := DeliveryResult{AttemptID: a.ID, Key: m.Key, Kind: m.Kind, TurnID: m.TurnID}
	// A lease may be revoked after the durable claim. Keep the attempt
	// dispatching for conservative recovery, but never issue the gateway call.
	if err := leaseCtx.Err(); err != nil {
		result.State = "unknown"
		return result, true, err
	}
	var remoteID string
	var sendErr error
	switch a.Operation {
	case "send":
		remoteID, sendErr = o.Gateway.SendCard(leaseCtx, "chat_id", a.ChatID, a.Card, a.ID)
	case "reply":
		remoteID, sendErr = o.Gateway.ReplyCard(leaseCtx, a.ParentMessageID, a.Card, a.ID)
	case "update":
		sendErr = o.Gateway.UpdateCard(leaseCtx, a.TargetMessageID, a.Card)
		remoteID = a.TargetMessageID
	default:
		// Schema constraints and ClaimMirror make this unreachable.
		sendErr = errors.New("unsupported persisted outbound operation")
	}
	var limited *feishu.RateLimited
	if errors.As(sendErr, &limited) {
		clock := o.Now
		if clock == nil {
			clock = time.Now
		}
		receivedAt := clock().UnixMilli()
		if receivedAt < claimAt {
			receivedAt = claimAt
		}
		delay := limited.RetryAfter.Milliseconds()
		if delay < 1000 {
			delay = 1000
		}
		if err := o.Store.DeferMirrorAttempt(leaseCtx, a.ID, receivedAt, delay); err != nil {
			result.State = "unknown"
			return result, true, err
		}
		result.State = "deferred"
		return result, true, nil
	}
	result.State = "confirmed"
	if sendErr != nil || remoteID == "" {
		result.State, remoteID = "unknown", ""
		var rejected *feishu.Rejected
		if errors.As(sendErr, &rejected) {
			result.State = "rejected"
		}
	}
	if err := o.Store.FinishMirrorAttempt(leaseCtx, a.ID, result.State, remoteID); err != nil {
		// The gateway may already have accepted the operation. Never
		// turn a failed local commit into another remote request.
		result.State = "unknown"
		return result, true, err
	}
	result.MessageID = remoteID
	return result, true, nil
}
