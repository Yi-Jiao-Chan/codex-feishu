package app

import (
	"context"
	"testing"

	"codex-feishu/internal/store"
)

func TestReceiverOrdinaryInputAfterStopCapturesContinueWithEitherAttachmentSetting(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "text-only", true: "attachments-enabled"}[enabled], func(t *testing.T) {
			r, s := receiverFixture(t)
			r.Config.Attachments.Enabled = enabled
			ctx := context.Background()
			scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
			before := receiverMessage(t, "chat", "ou_operator", "before-stop", "group", true)
			if err := r.Message(ctx, before); err != nil {
				t.Fatal(err)
			}
			stop, err := s.ClaimThreadSubmissionActionExpected(ctx, scope, "stop", "synthetic-stop", "turn")
			if err != nil {
				t.Fatal(err)
			}
			// Old message replay wakes recovery, but never gains new authority.
			if err := r.Message(ctx, before); err != nil {
				t.Fatal(err)
			}
			after := receiverMessage(t, "chat", "ou_operator", "after-stop", "group", true)
			for i := 0; i < 10; i++ {
				if err := r.Message(ctx, after); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.StageSubmissions(ctx, scope); err != nil {
				t.Fatal(err)
			}
			inputs, err := s.Submissions(ctx, scope)
			if err != nil || len(inputs) != 2 {
				t.Fatal("ordinary inputs were duplicated or lost", err)
			}
			actions, err := s.SubmissionActions(ctx, scope)
			if err != nil || len(actions) != 2 {
				t.Fatal("fresh input did not atomically record exactly one intention", err)
			}
			if source, ok := store.InputContinueSource(actions[1]); !ok || source != stop.ID || actions[1].ReceiptID != inputs[1].ID || inputs[1].State != "received" {
				t.Fatal("wrong ordinary message acquired continuation authority")
			}
			attempts, err := s.SubmissionAttempts(ctx, scope)
			if err != nil || len(attempts) != 0 {
				t.Fatal("ingress performed a native write", err)
			}
		})
	}
}
