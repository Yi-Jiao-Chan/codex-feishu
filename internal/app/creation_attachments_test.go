package app

import (
	"context"
	"strings"
	"testing"

	"codex-feishu/internal/creation"
)

func TestCreationReceiverRoutesNewPostWithImagesAndFiles(t *testing.T) {
	for _, tc := range []struct {
		name, chat, kind, task, root                              string
		boundTopic, mentioned, enabled, operator, draft, followup bool
	}{
		{name: "private new", chat: "private", kind: "p2p", task: "/new inspect", enabled: true, operator: true, draft: true},
		{name: "entry new", chat: "entry", kind: "group", task: "/new inspect", mentioned: true, enabled: true, operator: true, draft: true},
		{name: "entry ordinary", chat: "entry", kind: "group", task: "inspect", mentioned: true, enabled: true, operator: true, draft: true},
		{name: "bound new", chat: "chat", kind: "group", task: "/new inspect", mentioned: true, enabled: true, operator: true, draft: true},
		{name: "topic new", chat: "chat", kind: "group", task: "/new inspect", root: "root", boundTopic: true, mentioned: true, enabled: true, operator: true, draft: true},
		{name: "bound followup", chat: "chat", kind: "group", task: "inspect", mentioned: true, enabled: true, operator: true, followup: true},
		{name: "no group mention", chat: "entry", kind: "group", task: "/new inspect", enabled: true, operator: true},
		{name: "unauthorized sender", chat: "private", kind: "p2p", task: "/new inspect", enabled: true},
		{name: "disabled resources", chat: "private", kind: "p2p", task: "/new inspect", operator: true},
		{name: "only attachments", chat: "private", kind: "p2p", task: "/new", enabled: true, operator: true, draft: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, db := receiverFixture(t)
			r.Config.Creation.Enabled = true
			r.Config.Creation.AllowedRoots = []string{t.TempDir()}
			r.Config.Creation.EntryChatIDs = []string{"entry", "chat"}
			r.Config.Attachments.Enabled = tc.enabled
			m := &creation.Manager{Store: db, Config: r.Config}
			r.Create = m.Receive
			if tc.boundTopic {
				appFollowTopic(t, db, tc.root)
			}
			user := "ou_operator"
			if !tc.operator {
				user = "ou_other"
			}
			message := attachmentEvent(t, "creation-source", user, "post", map[string]any{
				"content": [][]map[string]any{{{"tag": "text", "text": tc.task}, {"tag": "img", "image_key": "img_fixture"}}},
				"files":   []map[string]any{{"file_key": "file_fixture", "file_name": "fixture.csv", "is_folder": false}},
			}, tc.mentioned)
			message.Event.Message.ChatId, message.Event.Message.ChatType = &tc.chat, &tc.kind
			if tc.boundTopic {
				platform := "platform-root"
				message.Event.Message.RootId, message.Event.Message.ThreadId = &tc.root, &platform
			}
			for replay := 0; replay < 2; replay++ {
				if err := r.Message(context.Background(), message); err != nil {
					t.Fatal(err)
				}
			}
			var drafts, receipts int
			if err := db.DB.QueryRow(`SELECT COUNT(*) FROM creation_drafts`).Scan(&drafts); err != nil {
				t.Fatal(err)
			}
			if err := db.DB.QueryRow(`SELECT COUNT(*) FROM m0_receipts`).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if drafts != boolCount(tc.draft) || receipts != boolCount(tc.followup) {
				t.Fatal("attachment creation chose the wrong route", drafts, receipts)
			}
			if tc.draft {
				var id string
				if err := db.DB.QueryRow(`SELECT draft_id FROM creation_drafts`).Scan(&id); err != nil {
					t.Fatal(err)
				}
				draft, err := db.CreationDraft(context.Background(), id)
				wantBody := "inspect"
				if tc.task == "/new" {
					wantBody = "请查看附件。"
				}
				if err != nil || len(draft.Attachments) != 2 || draft.Attachments[0].Kind != "image" || draft.Attachments[1].ResourceKey != "file_fixture" || draft.Body != wantBody || draft.SourceRootID != tc.root {
					t.Fatal("creation draft lost text, resources or source topic", err)
				}
			}
		})
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestCreationReceiverRejectsFilePostWhenAttachmentsDisabled(t *testing.T) {
	r, db := receiverFixture(t)
	r.Config.Creation.Enabled = true
	m := &creation.Manager{Store: db, Config: r.Config}
	r.Create = m.Receive
	var unsupported bool
	r.Command = func(_ context.Context, command InboundCommand) error { unsupported = command.Unsupported; return nil }
	e := attachmentEvent(t, "disabled-file", "ou_operator", "post", map[string]any{
		"content": [][]map[string]string{{{"tag": "text", "text": "/new inspect"}}},
		"files":   []map[string]any{{"file_key": "file_fixture", "file_name": "fixture.csv", "is_folder": false}},
	}, false)
	chat, kind := "private", "p2p"
	e.Event.Message.ChatId, e.Event.Message.ChatType = &chat, &kind
	if err := r.Message(context.Background(), e); err != nil || !unsupported {
		t.Fatal("disabled file post was accepted as text-only creation", err)
	}
}

func TestCreationReceiverAttachmentLimitReturnsFeedbackWithoutPartialDraft(t *testing.T) {
	r, db := receiverFixture(t)
	r.Config.Creation.Enabled, r.Config.Attachments.Enabled = true, true
	r.Config.Attachments.MaxResources = 1
	m := &creation.Manager{Store: db, Config: r.Config}
	r.Create = m.Receive
	var feedback string
	r.Command = func(_ context.Context, command InboundCommand) error { feedback = command.RouteFeedback; return nil }
	e := attachmentEvent(t, "limit", "ou_operator", "post", map[string]any{
		"content": [][]map[string]string{{{"tag": "text", "text": "/new inspect"}, {"tag": "img", "image_key": "img_fixture"}}},
		"files":   []map[string]any{{"file_key": "file_fixture", "file_name": "fixture.csv", "is_folder": false}},
	}, true)
	if err := r.Message(context.Background(), e); err != nil || !strings.Contains(feedback, "最多 1 个附件") {
		t.Fatal("resource limit was not explained", err)
	}
	var drafts, receipts int
	if err := db.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM creation_drafts),(SELECT COUNT(*) FROM m0_receipts)`).Scan(&drafts, &receipts); err != nil || drafts != 0 || receipts != 0 {
		t.Fatal("over-budget message created a partial draft or existing-thread input", err)
	}
}
