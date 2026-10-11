package app

import (
	"context"
	"fmt"

	"codex-feishu/internal/codexclient"
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
		turn := history[n.TurnID]
		desired := reactionFor(n.Receipt, turn)
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
		if r.State == "received" {
			if err := f.set(ctx, thread, r, "Get"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *Feedback) set(ctx context.Context, thread string, r store.Receipt, desired string) error {
	t, claimed, err := f.Store.ClaimReactionTransition(ctx, f.scope(thread), r.ID, r.State, desired)
	if err != nil || !claimed {
		return err
	}
	key := fmt.Sprintf("p1.reaction.%s.%d", r.ID, t.Revision)
	remoteID := ""
	finishError := func(key string, remoteErr error) error {
		state := resultState(remoteErr)
		if err := f.Store.Finish(ctx, key, state, ""); err != nil {
			return err
		}
		return f.Store.FinishReactionTransition(ctx, t, state, "")
	}
	if t.PreviousID != "" {
		fresh, err := f.Store.Begin(ctx, key+".delete", "feishu.reaction.delete")
		if err != nil {
			return err
		}
		if !fresh {
			return store.ErrConflict
		}
		if err := f.Gateway.DeleteReaction(ctx, t.MessageID, t.PreviousID); err != nil {
			return finishError(key+".delete", err)
		}
		if err := f.Store.Finish(ctx, key+".delete", "confirmed", t.PreviousID); err != nil {
			return err
		}
	}
	if desired != "" {
		fresh, err := f.Store.Begin(ctx, key+".add", "feishu.reaction.add")
		if err != nil {
			return err
		}
		if !fresh {
			return store.ErrConflict
		}
		remoteID, err = f.Gateway.AddReaction(ctx, t.MessageID, desired)
		if err != nil {
			return finishError(key+".add", err)
		}
		if remoteID == "" {
			if err := f.Store.Finish(ctx, key+".add", "unknown", ""); err != nil {
				return err
			}
			return f.Store.FinishReactionTransition(ctx, t, "unknown", "")
		}
		if err := f.Store.Finish(ctx, key+".add", "confirmed", remoteID); err != nil {
			return err
		}
	}
	return f.Store.FinishReactionTransition(ctx, t, "confirmed", remoteID)
}
