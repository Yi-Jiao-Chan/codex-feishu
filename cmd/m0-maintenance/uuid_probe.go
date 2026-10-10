package main

import (
	"codex-feishu/internal/cards"
	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Each intentional duplicate has its own journal entry. Unknown responses are
// frozen; rerunning this command never repeats a completed probe after expiry.
func confirmedProbe(ctx context.Context, db *store.Store, key, kind string, call func() (string, error)) (string, bool, error) {
	fresh, err := db.Begin(ctx, key, kind)
	if err != nil {
		return "", false, err
	}
	if !fresh {
		state, id, err := db.Operation(ctx, key)
		if err != nil {
			return "", false, err
		}
		if state != "confirmed" || id == "" {
			return "", false, errors.New("prior probe unresolved; not retrying")
		}
		return id, false, nil
	}
	id, err := call()
	if err != nil {
		state := "unknown"
		var rejected *feishu.Rejected
		if errors.As(err, &rejected) {
			state = "rejected"
		}
		if saveErr := db.Finish(ctx, key, state, ""); saveErr != nil {
			return "", true, saveErr
		}
		return "", true, err
	}
	return id, true, db.Finish(ctx, key, "confirmed", id)
}

func probeUUID(ctx context.Context, g *feishu.Gateway, db *store.Store, cfg config.Config, chat string) {
	fail := func(err error) { reportError(err); os.Exit(4) }
	members, err := g.MemberIDs(ctx, chat)
	if err != nil {
		fail(err)
	}
	ownerPresent := false
	for _, id := range members {
		ownerPresent = ownerPresent || id == cfg.Access.OwnerOpenID
	}
	if !ownerPresent {
		fail(errors.New("mention target is not in the test group"))
	}
	groupKey := "m0-uuid-group-v1"
	group, groupFresh, err := confirmedProbe(ctx, db, groupKey+".first", "feishu.uuid_group.first", func() (string, error) {
		return g.CreateGroup(ctx, "Codex Feishu · M0 UUID 验证", cfg.Access.OwnerOpenID, []string{cfg.Access.OwnerOpenID}, groupKey)
	})
	if err != nil {
		fail(err)
	}
	// A gap between processes cannot safely assume the original dedupe window.
	if !groupFresh {
		state, _, e := db.Operation(ctx, groupKey+".repeat")
		if e != nil || state != "confirmed" {
			fail(errors.New("group duplicate probe was not completed in the original process; not retrying"))
		}
	}
	groupAgain, _, err := confirmedProbe(ctx, db, groupKey+".repeat", "feishu.uuid_group.repeat", func() (string, error) {
		return g.CreateGroup(ctx, "Codex Feishu · M0 UUID 验证", cfg.Access.OwnerOpenID, []string{cfg.Access.OwnerOpenID}, groupKey)
	})
	if err != nil {
		fail(err)
	}
	if groupAgain != group {
		fail(errors.New("group duplicate returned a different chat_id"))
	}
	card := cards.Message("验证消息", "仅本人测试", "M0：@ 提醒、去重与手机长代码布局验证。\n\n```go\n"+strings.Repeat("// M0 layout fixture: long code line 012345678901234567890123456789012345678901234567890123456789\n", 24)+"```\n\n目录示例：`/allowed/project/services/example/long-directory-name/another-long-directory-name`")
	var data map[string]any
	if err = json.Unmarshal([]byte(card), &data); err != nil {
		fail(err)
	}
	body := data["body"].(map[string]any)
	elements := body["elements"].([]any)
	first := elements[0].(map[string]any)
	first["content"] = cards.Mention(cfg.Access.OwnerOpenID) + "\n" + first["content"].(string)
	rendered, err := json.Marshal(data)
	if err != nil {
		fail(err)
	}
	messageKey := "m0-uuid-message-v1"
	messageCard := cards.Progress("M0 消息 UUID 验证", "同一 UUID 的两次立即调用应返回同一条消息。")
	message, fresh, err := confirmedProbe(ctx, db, messageKey+".first", "feishu.uuid_message.first", func() (string, error) {
		return g.SendCard(ctx, "chat_id", group, messageCard, messageKey)
	})
	if err != nil {
		fail(err)
	}
	if !fresh {
		state, _, e := db.Operation(ctx, messageKey+".repeat")
		if e != nil || state != "confirmed" {
			fail(errors.New("message duplicate probe was not completed in the original process; not retrying"))
		}
	}
	messageAgain, _, err := confirmedProbe(ctx, db, messageKey+".repeat", "feishu.uuid_message.repeat", func() (string, error) {
		return g.SendCard(ctx, "chat_id", group, messageCard, messageKey)
	})
	if err != nil {
		fail(err)
	}
	if messageAgain != message {
		fail(errors.New("message duplicate returned a different message_id"))
	}
	if err = g.Chat(ctx, group); err != nil {
		fail(err)
	}
	if err = g.ReadMessage(ctx, message); err != nil {
		fail(err)
	}
	// Main-group probes also quote the actual user input, as product replies do.
	var parent string
	if err = db.DB.QueryRowContext(ctx, "SELECT message_id FROM m0_receipts WHERE chat_id=? AND state='steered' ORDER BY rowid DESC LIMIT 1", chat).Scan(&parent); err != nil {
		fail(err)
	}
	replyKey := "m0-uuid-reply-v1"
	reply, replyFresh, err := confirmedProbe(ctx, db, replyKey+".first", "feishu.uuid_reply.first", func() (string, error) {
		return g.ReplyCard(ctx, parent, string(rendered), replyKey)
	})
	if err != nil {
		fail(err)
	}
	if !replyFresh {
		state, _, e := db.Operation(ctx, replyKey+".repeat")
		if e != nil || state != "confirmed" {
			fail(errors.New("reply duplicate probe was not completed in the original process; not retrying"))
		}
	}
	replyAgain, _, err := confirmedProbe(ctx, db, replyKey+".repeat", "feishu.uuid_reply.repeat", func() (string, error) {
		return g.ReplyCard(ctx, parent, string(rendered), replyKey)
	})
	if err != nil {
		fail(err)
	}
	if replyAgain != reply {
		fail(errors.New("reply duplicate returned a different message_id"))
	}
	actualParent, err := g.MessageParent(ctx, reply)
	if err != nil {
		fail(err)
	}
	if actualParent != parent {
		fail(errors.New("mention probe has incorrect reply parent"))
	}
	if err = json.NewEncoder(os.Stdout).Encode(map[string]any{"group_uuid_query_deduplication": true, "message_uuid_body_deduplication": true, "reply_uuid_body_deduplication": true, "duplicate_test_window": "same process, immediate", "uuid_test_chat_id": group, "uuid_test_message_id": message, "mention_layout_message_id": reply, "reply_parent_verified": true, "mention_target_in_group": ownerPresent, "notification_ui_verified": false, "mobile_layout_verified": false}); err != nil {
		fmt.Fprintln(os.Stderr, "cannot encode probe evidence")
		os.Exit(4)
	}
}
