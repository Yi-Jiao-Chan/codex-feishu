package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codex-feishu/internal/creation"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

func TestCreationReceiverUsesStandaloneSelectOption(t *testing.T) {
	for _, tag := range []string{"select_static", "button"} {
		t.Run(tag, func(t *testing.T) {
			r, _ := receiverFixture(t)
			r.Config.Creation.Enabled = true
			called := false
			r.CreationAction = func(_ context.Context, user, chat, message, token string, revision int64, kind string, value, form map[string]any) error {
				called = true
				if user != "ou_operator" || chat != "chat" || message != "card" || token != "token" || revision != 3 || kind != "creation_browse" {
					t.Fatal("directory callback identity changed")
				}
				if tag == "select_static" && value["option"] != "/allowed/selected" || tag == "button" && value["option"] != nil {
					t.Fatal("callback choice did not come from the standalone select", value)
				}
				return nil
			}
			raw, err := json.Marshal(map[string]any{"event": map[string]any{
				"operator": map[string]any{"open_id": "ou_operator"},
				"context":  map[string]any{"open_chat_id": "chat", "open_message_id": "card"},
				"action":   map[string]any{"tag": tag, "option": "/allowed/selected", "value": map[string]any{"action": "creation_browse", "token": "token", "revision": 3, "option": "/forged"}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			var event callback.CardActionTriggerEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			response, err := r.Action(context.Background(), &event)
			if err != nil || !called || response.Toast == nil || response.Toast.Content != "正在打开目录…" {
				t.Fatal("directory selection was not claimed with specific feedback", err)
			}
			if event.Event.Action.Value["option"] != "/forged" || response.Card != nil {
				t.Fatal("receiver mutated ingress or raced the ordered card writer")
			}
		})
	}
}

func TestCreationReceiverRoutesEntrancesAndTopicFollowups(t *testing.T) {
	for _, tc := range []struct {
		name, chat, kind, text, root, platform string
		topicBinding, draft, receipt, guidance bool
	}{
		{name: "private ordinary", chat: "private", kind: "p2p", text: "task", draft: true},
		{name: "private slash new", chat: "private", kind: "p2p", text: "/new task", draft: true},
		{name: "entry group ordinary", chat: "oc_entry", kind: "group", text: "task", draft: true},
		{name: "bound group followup", chat: "chat", kind: "group", text: "task", receipt: true},
		{name: "bound group new", chat: "chat", kind: "group", text: "/new task", draft: true},
		{name: "bound topic followup", chat: "chat", kind: "group", text: "task", root: "root", platform: "platform-root", topicBinding: true, receipt: true},
		{name: "bound topic new", chat: "chat", kind: "group", text: "/new task", root: "root", platform: "platform-root", topicBinding: true, draft: true},
		{name: "workgroup main needs guidance", chat: "chat", kind: "group", text: "task", topicBinding: true, guidance: true},
		{name: "other topic needs guidance", chat: "chat", kind: "group", text: "task", root: "other-root", platform: "other-platform", topicBinding: true, guidance: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, db := receiverFixture(t)
			r.Config.Creation.Enabled = true
			r.Config.Creation.AllowedRoots = []string{t.TempDir()}
			r.Config.Creation.EntryChatIDs = []string{"oc_entry", "chat"}
			r.Config.Creation.WorkgroupChatIDs = []string{"chat"}
			if tc.topicBinding {
				appFollowTopic(t, db, "root")
			}
			m := &creation.Manager{Store: db, Config: r.Config}
			r.Create = m.Receive
			var commands []InboundCommand
			r.Command = func(_ context.Context, in InboundCommand) error {
				commands = append(commands, in)
				return nil
			}
			e := receiverMessage(t, tc.chat, "ou_operator", "source", tc.kind, tc.kind == "group")
			body := tc.text
			if tc.kind == "group" {
				body = "@_user_1 " + body
			}
			raw, err := json.Marshal(map[string]string{"text": body})
			if err != nil {
				t.Fatal(err)
			}
			content := string(raw)
			e.Event.Message.Content = &content
			if tc.root != "" {
				e.Event.Message.RootId = &tc.root
				e.Event.Message.ThreadId = &tc.platform
			}
			for replay := 0; replay < 2; replay++ {
				if err := r.Message(context.Background(), e); err != nil {
					t.Fatal(err)
				}
			}
			var drafts int
			if err := db.DB.QueryRow(`SELECT COUNT(*) FROM creation_drafts`).Scan(&drafts); err != nil {
				t.Fatal(err)
			}
			receipts, err := db.Receipts(context.Background(), "thread")
			if err != nil {
				t.Fatal(err)
			}
			wantDrafts, wantReceipts := 0, 0
			if tc.draft {
				wantDrafts = 1
			}
			if tc.receipt {
				wantReceipts = 1
			}
			if drafts != wantDrafts || len(receipts) != wantReceipts {
				t.Fatalf("route duplicated or chose a different thread: drafts=%d receipts=%d", drafts, len(receipts))
			}
			if tc.guidance {
				if len(commands) != 2 || commands[0].RouteFeedback == "" || commands[0].ThreadID != "" || commands[0].SourceRootID != tc.root || commands[0].SourceThreadID != tc.platform {
					t.Fatal("unbound workgroup message guessed a topic or lost its reply identity")
				}
			} else if len(commands) != 0 {
				t.Fatal("creation/followup was also routed as a command")
			}
			if tc.draft {
				var body, root, platform string
				if err := db.DB.QueryRow(`SELECT body,source_root_id,source_thread_id FROM creation_drafts`).Scan(&body, &root, &platform); err != nil {
					t.Fatal(err)
				}
				if body != "task" || root != tc.root || platform != tc.platform {
					t.Fatal("creation draft lost its prefill or source topic")
				}
			}
		})
	}
}

func TestCreationReceiverOversizedTaskHasFeedbackInItsOriginalTopic(t *testing.T) {
	for _, topic := range []bool{false, true} {
		t.Run(map[bool]string{false: "private", true: "bound topic"}[topic], func(t *testing.T) {
			r, db := receiverFixture(t)
			r.Config.Creation.Enabled = true
			r.Config.Creation.AllowedRoots = []string{t.TempDir()}
			m := &creation.Manager{Store: db, Config: r.Config}
			r.Create = m.Receive
			chat, kind, root, platform := "private", "p2p", "root", "platform-root"
			if topic {
				appFollowTopic(t, db, root)
				chat, kind = "chat", "group"
			}
			in := receiverMessage(t, chat, "ou_operator", "oversized", kind, topic)
			body := "/new " + strings.Repeat("长", creation.MaxTaskRunes+1)
			if topic {
				body = "@_user_1 " + body
				in.Event.Message.RootId = &root
				in.Event.Message.ThreadId = &platform
			}
			raw, _ := json.Marshal(map[string]string{"text": body})
			content := string(raw)
			in.Event.Message.Content = &content
			var replies []InboundCommand
			r.Command = func(_ context.Context, feedback InboundCommand) error {
				replies = append(replies, feedback)
				return nil
			}
			if err := r.Message(context.Background(), in); err != nil {
				t.Fatal("oversized task caused ingress failure without feedback", err)
			}
			if len(replies) != 1 || !strings.Contains(replies[0].RouteFeedback, "4000") || replies[0].MessageID != "oversized" || replies[0].ChatID != chat || strings.Contains(replies[0].Text, "长") {
				t.Fatal("task size rejection was silent or retained the oversized body")
			}
			if topic && (replies[0].ThreadID != "thread" || replies[0].SourceRootID != root || replies[0].SourceThreadID != platform) {
				t.Fatal("task size feedback lost its original topic")
			}
			var drafts, receipts int
			if err := db.DB.QueryRow(`SELECT COUNT(*) FROM creation_drafts`).Scan(&drafts); err != nil {
				t.Fatal(err)
			}
			if err := db.DB.QueryRow(`SELECT COUNT(*) FROM m0_receipts`).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if drafts != 0 || receipts != 0 {
				t.Fatal("oversized creation reached a draft or an existing native thread")
			}
		})
	}
}
