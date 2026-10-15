package app

import (
	"context"
	"errors"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

type ReactionGateway interface {
	AddReaction(context.Context, string, string) (string, error)
	DeleteReaction(context.Context, string, string) error
}

type Feedback struct {
	Store         *store.Store
	Gateway       ReactionGateway
	HostID, AppID string
	Waiting       func(string, string) bool
	Now           func() time.Time
	BeginWrite    func(context.Context, string) (context.Context, func(), error)
}

func (f *Feedback) scope(thread string) store.SubmissionScope {
	return store.SubmissionScope{HostID: f.HostID, AppID: f.AppID, ThreadID: thread}
}

// Stage only updates queue UI and does not send reactions before the result
// outbox had a chance to deliver. All inputs keep their original human message.
func (f *Feedback) Stage(ctx context.Context, thread string) error {
	if _, err := f.Store.SubmissionBinding(ctx, f.scope(thread)); err != nil {
		return err
	}
	ns, err := f.Store.Submissions(ctx, f.scope(thread))
	if err != nil {
		return err
	}
	for _, n := range ns {
		if err := f.Store.StageQueueCard(ctx, f.scope(thread), n.ID); err != nil {
			return err
		}
	}
	return nil
}

// StageReceived performs local work only, after a native generation has joined
// and before the next recovery. The service separately dispatches this existing
// mirror outbox serially; this method never calls a gateway or native daemon.
func (f *Feedback) StageReceived(ctx context.Context, thread string) error {
	if err := f.Store.StageSubmissions(ctx, f.scope(thread)); err != nil {
		return err
	}
	ns, err := f.Store.Submissions(ctx, f.scope(thread))
	if err != nil {
		return err
	}
	for _, n := range ns {
		if err := f.Store.StageQueueCardWithOptions(ctx, f.scope(thread), n.ID, store.QueueCardOptions{LocalPending: true, Offline: true}); err != nil {
			return err
		}
	}
	return nil
}

func (f *Feedback) Sync(ctx context.Context, thread string, turns []codexclient.Turn) error {
	ns, err := f.Store.Submissions(ctx, f.scope(thread))
	if err != nil {
		return err
	}
	history := make(map[string]codexclient.Turn, len(turns))
	for _, turn := range turns {
		history[turn.ID] = turn
	}
	engine := messages.Engine{Store: f.Store, HostID: f.HostID}
	for _, n := range ns {
		effective := n
		if n.State == "draft" {
			target, found, err := f.Store.AttachmentSourceSubmission(ctx, f.scope(thread), n.ID)
			if err != nil {
				return err
			}
			if found {
				effective = target
			}
		}
		turn := history[effective.TurnID]
		desired := reactionFor(effective.Receipt, turn)
		if turn.Status == "inProgress" && f.Waiting != nil && f.Waiting(thread, turn.ID) {
			desired = "THINKING"
		}
		if turn.Status == "completed" {
			done, err := engine.FinalDelivered(ctx, thread, turn.ID)
			if err != nil {
				return err
			}
			if !done {
				desired = "OnIt"
			}
		}
		if err := f.set(ctx, thread, n.Receipt, desired); err != nil {
			return err
		}
	}
	return nil
}

// Received works while the native generation is disconnected. CAS against
// current input state prevents its stale snapshot replacing actor feedback.
func (f *Feedback) Received(ctx context.Context, thread string) error {
	receipts, err := f.Store.ReceiptsForDeployment(ctx, f.HostID, thread)
	if err != nil {
		return err
	}
	for _, r := range receipts {
		if r.State == "received" || r.State == "draft" || r.State == "collecting" {
			effective := r
			if r.State == "draft" {
				target, found, err := f.Store.AttachmentSourceSubmission(ctx, f.scope(thread), r.ID)
				if err != nil {
					return err
				}
				if found {
					if target.State != "collecting" && target.State != "received" {
						continue
					}
					effective = target.Receipt
				}
			}
			if err := f.set(ctx, thread, r, reactionFor(effective, codexclient.Turn{})); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *Feedback) set(ctx context.Context, thread string, r store.Receipt, desired string) error {
	opctx, release, err := beginFeedbackWrite(ctx, thread, f.BeginWrite)
	if err != nil {
		return err
	}
	defer release()
	ctx = opctx
	clock := f.Now
	if clock == nil {
		clock = time.Now
	}
	claimAt := clock().UnixMilli()
	t, claimed, err := f.Store.ClaimReactionTransitionAt(ctx, f.scope(thread), r.ID, r.State, desired, claimAt)
	if err != nil || !claimed {
		return err
	}
	releaseUnsent := func(cause error) error {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := f.Store.ReleaseUnsentReactionTransition(cleanup, t); err != nil {
			return err
		}
		return cause
	}
	remoteID := ""
	finishError := func(operation string, remoteErr error) error {
		var limited *feishu.RateLimited
		if errors.As(remoteErr, &limited) {
			at := clock().UnixMilli()
			if at < claimAt {
				at = claimAt
			}
			delay := limited.RetryAfter.Milliseconds()
			if delay < 1000 {
				delay = 1000
			}
			return f.Store.DeferReactionTransition(ctx, t, operation, at, delay)
		}
		key := store.ReactionOperationKey(t, operation)
		state := resultState(remoteErr)
		if err := f.Store.Finish(ctx, key, state, ""); err != nil {
			return err
		}
		return f.Store.FinishReactionTransition(ctx, t, state, "")
	}
	if t.PreviousID != "" {
		if err := ctx.Err(); err != nil {
			return releaseUnsent(err)
		}
		fresh, err := f.Store.Begin(ctx, store.ReactionOperationKey(t, "delete"), "feishu.reaction.delete")
		if err != nil {
			return releaseUnsent(err)
		}
		if !fresh {
			return store.ErrConflict
		}
		if err := ctx.Err(); err != nil {
			return err // Journaled but not dispatched: conservative recovery.
		}
		if err := f.Gateway.DeleteReaction(ctx, t.MessageID, t.PreviousID); err != nil {
			return finishError("delete", err)
		}
		if err := f.Store.RecordReactionDeletion(ctx, t); err != nil {
			return err
		}
	}
	if desired != "" {
		if t.PreviousID != "" {
			// A completed delete does not grant the following add. Recheck
			// scope before its journal; keep the known empty reaction on refusal.
			addctx, releaseAdd, err := beginFeedbackWrite(ctx, thread, f.BeginWrite)
			if err != nil {
				return releaseUnsent(err)
			}
			defer releaseAdd()
			ctx = addctx
		}
		if err := ctx.Err(); err != nil {
			return releaseUnsent(err)
		}
		fresh, err := f.Store.Begin(ctx, store.ReactionOperationKey(t, "add"), "feishu.reaction.add")
		if err != nil {
			return releaseUnsent(err)
		}
		if !fresh {
			return store.ErrConflict
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		remoteID, err = f.Gateway.AddReaction(ctx, t.MessageID, desired)
		if err != nil {
			return finishError("add", err)
		}
		if remoteID == "" {
			if err := f.Store.Finish(ctx, store.ReactionOperationKey(t, "add"), "unknown", ""); err != nil {
				return err
			}
			return f.Store.FinishReactionTransition(ctx, t, "unknown", "")
		}
		if err := f.Store.Finish(ctx, store.ReactionOperationKey(t, "add"), "confirmed", remoteID); err != nil {
			return err
		}
	}
	return f.Store.FinishReactionTransition(ctx, t, "confirmed", remoteID)
}

func beginFeedbackWrite(ctx context.Context, thread string, begin func(context.Context, string) (context.Context, func(), error)) (context.Context, func(), error) {
	if begin == nil {
		return ctx, func() {}, ctx.Err()
	}
	opctx, release, err := begin(ctx, thread)
	if err == nil && opctx != nil && release != nil && opctx.Err() == nil {
		return opctx, release, nil
	}
	if release != nil {
		release()
	}
	return nil, nil, errors.New("feedback delivery authority unavailable")
}
