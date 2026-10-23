package app

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
	messages "codex-feishu/internal/sync"
)

func TestCaptionedImageConsumedImmediatelyOnlyStagesCOTAndFinal(t *testing.T) {
	r, db := receiverFixture(t)
	r.Config.Attachments.Enabled = true
	ctx := context.Background()
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	feedback := Feedback{Store: db, Gateway: &fakeMessages{}, HostID: "host", AppID: "app"}
	assertPublicMirrors := func(want int) {
		t.Helper()
		mirrors, err := db.Mirrors(ctx, "host", "thread")
		if err != nil || len(mirrors) != want {
			t.Fatalf("expected %d public mirrors without an input receipt card, got %d: %v", want, len(mirrors), err)
		}
		for _, mirror := range mirrors {
			if mirror.TurnID != "image-turn" || mirror.ParentMessageID != "image-post" {
				t.Fatal("image input created an extra reply or lost its reply anchor")
			}
			if mirror.Kind == "progress" {
				if _, recognized, err := cot.Parse(mirror.Card); err != nil || !recognized {
					t.Fatal("image progress is not native CoT", err)
				}
			} else if mirror.Kind != "final" {
				t.Fatal("unexpected image reply kind", mirror.Kind)
			}
		}
	}
	if err := r.Message(ctx, attachmentEvent(t, "image-post", "ou_operator", "post", map[string]any{
		"content": [][]map[string]string{{{"tag": "at", "user_id": "@_user_1"}, {"tag": "text", "text": "inspect this image"}, {"tag": "img", "image_key": "img_fixture"}}},
	}, true)); err != nil {
		t.Fatal(err)
	}
	if err := feedback.Received(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	assertPublicMirrors(0)
	inputs, err := db.Submissions(ctx, scope)
	if err != nil || len(inputs) != 1 || inputs[0].State != "collecting" {
		t.Fatal("captioned image did not create one collecting input", err)
	}
	if emoji, state, err := db.ReactionFeedbackState(ctx, inputs[0].ID); err != nil || state != "confirmed" || emoji != "THINKING" {
		t.Fatal("collecting image lost reaction feedback", err)
	}
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	worker := attachmentWorker(t, r, db, &appResourceGateway{images: map[string][]byte{"image-post:img_fixture": data.Bytes()}})
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatal("image download failed", err)
	}
	if err := feedback.Stage(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	assertPublicMirrors(0)
	native := &appMultipartNative{serviceNative: &serviceNative{runtimeNative: &runtimeNative{turns: map[string][]codexclient.Turn{}}}}
	controller := submissions.Controller{Store: db, Native: native, HostID: "host", AppID: "app", ThreadID: "thread"}
	if err := controller.Tick(ctx); err != nil || len(native.inputs) != 1 || len(native.queue) != 1 {
		t.Fatal("image did not enter the native queue once", err)
	}
	var content []json.RawMessage
	if err := json.Unmarshal(native.inputs[0], &content); err != nil {
		t.Fatal(err)
	}
	client := *native.queue[0].ClientUserMessageID
	turns := []codexclient.Turn{{ID: "image-turn", Status: "inProgress", Items: []codexclient.Item{
		{ID: "image-user", Type: "userMessage", ClientID: &client, Content: content},
		{ID: "commentary", Type: "agentMessage", Text: "working", Phase: str("commentary")},
	}}}
	// An idle native queue consumes the input before queue feedback is staged.
	native.queue, native.turns["thread"] = nil, turns
	if err := controller.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	engine := messages.Engine{Store: db, HostID: "host", NativeCOT: true}
	for i, status := range []string{"inProgress", "completed"} {
		turns[0].Status = status
		if status == "completed" {
			turns[0].Items = append(turns[0].Items, codexclient.Item{ID: "answer", Type: "agentMessage", Text: "fixture complete", Phase: str("final_answer")})
		}
		if err := feedback.Stage(ctx, "thread"); err != nil {
			t.Fatal(err)
		}
		if err := engine.Apply(ctx, "thread", turns, false, int64(200+i)); err != nil {
			t.Fatal(err)
		}
		if err := feedback.Sync(ctx, "thread", turns); err != nil {
			t.Fatal(err)
		}
		assertPublicMirrors(i + 1)
	}
	if err := controller.Tick(ctx); err != nil || len(native.inputs) != 1 {
		t.Fatal("image input was submitted more than once", err)
	}
}

// Exercise the same frozen input through history, delivery and reactions. In
// particular, each attachment source follows the one native receipt even when
// the instruction message (or the first attachment's card) sealed the batch.
func TestAttachmentHistoryFeedbackKeepsSourcesAndReplyAnchor(t *testing.T) {
	for _, send := range []string{"mention", "card"} {
		t.Run(send, func(t *testing.T) {
			r, db := receiverFixture(t)
			r.Config.Attachments.Enabled, r.Config.Attachments.ReceiveStandalone = true, true
			ctx := context.Background()
			scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
			gateway := &fakeMessages{}
			feedback := Feedback{Store: db, Gateway: gateway, HostID: "host", AppID: "app"}
			outbound := messages.Outbound{Store: db, Gateway: gateway}
			deliver := func(at int64) {
				t.Helper()
				for i := 0; i < 20; i++ {
					result, err := outbound.DispatchOne(ctx, "host", "thread", at)
					if err != nil || result.AttemptID != "" && result.State != "confirmed" {
						t.Fatal("attachment fixture delivery failed", err)
					}
					if result.AttemptID == "" {
						return
					}
				}
				t.Fatal("attachment fixture outbox did not settle")
			}
			assertReactions := func(want string) {
				t.Helper()
				receipts, err := db.ReceiptsForDeployment(ctx, "host", "thread")
				if err != nil || len(receipts) < 2 {
					t.Fatal("source receipts missing", err)
				}
				for _, receipt := range receipts {
					emoji, state, err := db.ReactionFeedbackState(ctx, receipt.ID)
					if err != nil || state != "confirmed" || emoji != want {
						t.Fatal("source reaction did not follow sealed input", state, emoji, want, err)
					}
				}
			}
			if err := r.Message(ctx, attachmentEvent(t, "source-image", "ou_operator", "image", map[string]string{"image_key": "img_fixture"}, false)); err != nil {
				t.Fatal(err)
			}
			if err := r.Message(ctx, attachmentEvent(t, "source-file", "ou_operator", "file", map[string]string{"file_key": "file_fixture", "file_name": "materials.csv"}, false)); err != nil {
				t.Fatal(err)
			}
			if err := feedback.Received(ctx, "thread"); err != nil {
				t.Fatal(err)
			}
			assertReactions("Get")
			draft, err := db.OpenAttachmentBatch(ctx, scope, "ou_operator")
			if err != nil {
				t.Fatal(err)
			}
			anchor := "instruction"
			if send == "mention" {
				if err := r.Message(ctx, attachmentEvent(t, anchor, "ou_operator", "text", map[string]string{"text": "@_user_1 inspect the materials"}, true)); err != nil {
					t.Fatal(err)
				}
			} else {
				anchor = "source-image"
				deliver(100)
				card, err := db.Mirror(ctx, "host", "thread", store.AttachmentCardKey(draft.ID))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.AttachmentAction(ctx, scope, "ou_operator", "chat", card.RemoteID, draft.Token, draft.Revision, "attachment_send", "", "inspect the materials"); err != nil {
					t.Fatal(err)
				}
			}
			if err := feedback.Received(ctx, "thread"); err != nil {
				t.Fatal(err)
			}
			assertReactions("THINKING")
			var data bytes.Buffer
			if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
				t.Fatal(err)
			}
			worker := attachmentWorker(t, r, db, &appResourceGateway{images: map[string][]byte{"source-image:img_fixture": data.Bytes(), "source-file:file_fixture": []byte("fixture,42\n")}})
			for i := 0; i < 2; i++ {
				if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
					t.Fatal("attachment fixture download failed", err)
				}
			}
			native := &appMultipartNative{serviceNative: &serviceNative{runtimeNative: &runtimeNative{turns: map[string][]codexclient.Turn{}}}}
			controller := submissions.Controller{Store: db, Native: native, HostID: "host", AppID: "app", ThreadID: "thread"}
			if err := controller.Tick(ctx); err != nil || len(native.inputs) != 1 || len(native.queue) != 1 {
				t.Fatal("sealed input did not enter queue once", err)
			}
			if err := feedback.Stage(ctx, "thread"); err != nil {
				t.Fatal(err)
			}
			if err := feedback.Sync(ctx, "thread", nil); err != nil {
				t.Fatal(err)
			}
			assertReactions("OneSecond")
			var content []json.RawMessage
			if err := json.Unmarshal(native.inputs[0], &content); err != nil {
				t.Fatal(err)
			}
			client := *native.queue[0].ClientUserMessageID
			turns := []codexclient.Turn{{ID: "attachment-turn", Status: "inProgress", Items: []codexclient.Item{
				{ID: "attachment-user", Type: "userMessage", ClientID: &client, Content: content},
				{ID: "commentary", Type: "agentMessage", Text: "working", Phase: str("commentary")},
			}}}
			native.queue, native.turns["thread"] = nil, turns
			if err := controller.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if err := feedback.Stage(ctx, "thread"); err != nil {
				t.Fatal(err)
			}
			engine := messages.Engine{Store: db, HostID: "host", PrepareHuman: func(context.Context, string, []codexclient.AttachmentReference) ([]cards.AttachmentView, error) {
				t.Fatal("Feishu input was prepared for redundant image upload")
				return nil, nil
			}}
			if err := engine.Apply(ctx, "thread", turns, false, 200); err != nil {
				t.Fatal(err)
			}
			if err := feedback.Sync(ctx, "thread", turns); err != nil {
				t.Fatal(err)
			}
			assertReactions("OnIt")
			turns[0].Status = "completed"
			turns[0].Items = append(turns[0].Items, codexclient.Item{ID: "answer", Type: "agentMessage", Text: "fixture complete", Phase: str("final_answer")})
			if err := engine.Apply(ctx, "thread", turns, false, 201); err != nil {
				t.Fatal(err)
			}
			if err := feedback.Sync(ctx, "thread", turns); err != nil {
				t.Fatal(err)
			}
			assertReactions("OnIt") // Completion feedback waits for final delivery.
			deliver(1000)
			if err := feedback.Sync(ctx, "thread", turns); err != nil {
				t.Fatal(err)
			}
			assertReactions("CheckMark")
			mirrors, err := db.Mirrors(ctx, "host", "thread")
			if err != nil {
				t.Fatal(err)
			}
			public := 0
			for _, mirror := range mirrors {
				if mirror.Kind == "human" {
					t.Fatal("Feishu attachment input was echoed")
				}
				if mirror.TurnID == "attachment-turn" {
					public++
					if mirror.ParentMessageID != anchor || mirror.ParentKey != "" || mirror.State != "confirmed" {
						t.Fatal("process/result lost submission source")
					}
				}
			}
			links, err := db.InputLinks(ctx, "host", "thread")
			if err != nil || len(links) != 1 || links[0].TurnID != "attachment-turn" || public != 2 {
				t.Fatal("native attachment association or process/result missing", err)
			}
			writes := len(gateway.sends) + len(gateway.updates) + len(gateway.added) + len(gateway.deleted)
			if err := engine.Apply(ctx, "thread", turns, true, 202); err != nil {
				t.Fatal(err)
			}
			deliver(2000)
			if err := feedback.Sync(ctx, "thread", turns); err != nil || writes != len(gateway.sends)+len(gateway.updates)+len(gateway.added)+len(gateway.deleted) {
				t.Fatal("history replay duplicated attachment feedback", err)
			}
		})
	}
}
