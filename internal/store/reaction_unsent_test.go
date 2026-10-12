package store

import (
	"context"
	"testing"
)

func TestUnsentReactionTransitionRequiresExactNoDispatchProof(t *testing.T) {
	for _, stage := range []string{"none", "add-journal", "delete-journal", "delete-confirmed", "wrong-delete-kind"} {
		t.Run(stage, func(t *testing.T) {
			s, _, scope, r := submissionStore(t)
			ctx := context.Background()
			first := reactionClaim(t, s, scope, r, "Get", 1000)
			reactionBegin(t, s, first, "add")
			if err := s.Finish(ctx, ReactionOperationKey(first, "add"), "confirmed", "reaction-old"); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishReactionTransition(ctx, first, "confirmed", "reaction-old"); err != nil {
				t.Fatal(err)
			}
			tr := reactionClaim(t, s, scope, r, "THINKING", 1001)
			switch stage {
			case "add-journal":
				reactionBegin(t, s, tr, "add")
			case "delete-journal", "delete-confirmed":
				reactionBegin(t, s, tr, "delete")
				if stage == "delete-confirmed" {
					if err := s.RecordReactionDeletion(ctx, tr); err != nil {
						t.Fatal(err)
					}
				}
			case "wrong-delete-kind":
				if _, err := s.Begin(ctx, ReactionOperationKey(tr, "delete"), "another.operation"); err != nil {
					t.Fatal(err)
				}
			}
			err := s.ReleaseUnsentReactionTransition(ctx, tr)
			allowed := stage == "none" || stage == "delete-confirmed"
			if (err == nil) != allowed {
				t.Fatal("incorrect no-dispatch proof", stage, err)
			}
			emoji, state, err := s.ReactionFeedbackState(ctx, r.ID)
			if err != nil || allowed && state != "confirmed" || !allowed && state != "changing" {
				t.Fatal(emoji, state, err)
			}
			if stage == "delete-confirmed" && emoji != "" || stage == "none" && emoji != "Get" {
				t.Fatal("release lost known remote state", emoji)
			}
		})
	}
}
