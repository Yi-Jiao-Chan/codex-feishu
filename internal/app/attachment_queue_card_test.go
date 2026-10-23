package app

import (
	"context"
	"strings"
	"testing"

	"codex-feishu/internal/store"
)

func TestAttachmentQueueReusesDeliveredCardAndChecksCallbackIdentity(t *testing.T) {
	for _, action := range []string{"steer", "remove"} {
		t.Run(action, func(t *testing.T) {
			r, db := receiverFixture(t)
			r.Config.Attachments.Enabled, r.Config.Attachments.ReceiveStandalone = true, true
			ctx := context.Background()
			scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
			if err := r.Message(ctx, attachmentEvent(t, "image-post", "ou_operator", "image", map[string]string{"image_key": "img_fixture"}, false)); err != nil {
				t.Fatal(err)
			}
			if err := r.Message(ctx, attachmentEvent(t, "instruction", "ou_operator", "text", map[string]string{"text": "@_user_1 inspect this image"}, true)); err != nil {
				t.Fatal(err)
			}
			inputs, err := db.Submissions(ctx, scope)
			if err != nil || len(inputs) != 2 || inputs[1].State != "collecting" {
				t.Fatal("attachment input missing", err)
			}
			n := inputs[1]
			card, err := db.SubmissionCard(ctx, scope, n.ID)
			if err != nil || !strings.HasPrefix(card.Key, "ui:attachments:") {
				t.Fatal("initial attachment card missing", err)
			}
			deliver := func(card store.Mirror, at int64, operation string) {
				t.Helper()
				attempt, claimed, err := db.ClaimMirror(ctx, "host", "thread", card.Key, card.Revision, at)
				if err != nil || !claimed || attempt.Operation != operation || operation == "update" && attempt.TargetMessageID != "shared-attachment-card" {
					t.Fatal("card changed identity instead of updating", err)
				}
				if err := db.FinishMirrorAttempt(ctx, attempt.ID, "confirmed", "shared-attachment-card"); err != nil {
					t.Fatal(err)
				}
			}
			deliver(card, 1000, "reply")
			part, claimed, err := db.ClaimAttachmentDownload(ctx, "host", "app", 100)
			if err != nil || !claimed {
				t.Fatal(err)
			}
			part.Size, part.Digest, part.FileID = 10, "fixture-digest", "fixture-file"
			if err := db.FinishAttachmentDownload(ctx, part, "", false, 100); err != nil {
				t.Fatal(err)
			}
			if _, err := db.RefreshAttachmentBatches(ctx, "host", "app", 50<<20, 20<<20, func(store.AttachmentPart) (string, error) { return "/private/fixture.png", nil }); err != nil {
				t.Fatal(err)
			}
			if err := db.ObserveSubmission(ctx, scope, n.ID, "queued", "native-image-q", 3, "", ""); err != nil {
				t.Fatal(err)
			}
			if err := db.StageTaskControls(ctx, scope, []store.TaskControl{{TurnID: "active-turn", NativeStatus: "inProgress", Kind: "stop", State: "available", Fingerprint: "active"}}); err != nil {
				t.Fatal(err)
			}
			feedback := Feedback{Store: db, HostID: "host", AppID: "app"}
			if err := feedback.Stage(ctx, "thread"); err != nil {
				t.Fatal(err)
			}
			queued, err := db.SubmissionCard(ctx, scope, n.ID)
			if err != nil || queued.Key != card.Key || queued.RemoteID != "shared-attachment-card" || !strings.Contains(queued.Card, "第 3 条") || !strings.Contains(queued.Card, "追加引导") || !strings.Contains(queued.Card, "移除") || strings.Contains(queued.Card, "个附件") || strings.Contains(queued.Card, "来源消息") {
				t.Fatal("queued image retained duplicate attachment presentation", err)
			}
			deliver(queued, 5000, "update")
			mirrors, err := db.Mirrors(ctx, "host", "thread")
			if err != nil || len(mirrors) != 1 {
				t.Fatal("queued attachment created a second card", err)
			}
			n, err = db.Submission(ctx, scope, n.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range []string{"foreign-card", "shared-attachment-card", "shared-attachment-card"} {
				event := receiverAction(t, "ou_operator", message, n.ActionToken, n.QueueID, n.Revision)
				event.Event.Action.Value["action"] = action
				if _, err := r.Action(ctx, event); err != nil {
					t.Fatal(err)
				}
				actions, err := db.SubmissionActions(ctx, scope)
				want := 1
				if message == "foreign-card" {
					want = 0
				}
				if err != nil || len(actions) != want || want == 1 && (actions[0].Kind != action || actions[0].QueueID != "native-image-q") {
					t.Fatal("shared card callback lost identity or duplicated action", err)
				}
			}
		})
	}
}
