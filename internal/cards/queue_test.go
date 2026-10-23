package cards

import (
	"strings"
	"testing"
)

func TestQueueInputUsesOnlyVerifiedActiveTurnForSteer(t *testing.T) {
	for _, variant := range []string{"active", "stopped", "idle", "received", "unknown"} {
		t.Run(variant, func(t *testing.T) {
			v := QueueView{State: "queued", Token: "token", QueueID: "native-q", Revision: 2, QueueOrdinal: 3}
			switch variant {
			case "active":
				v.Active = true
			case "stopped":
				v.Stopped = true
			case "received":
				v.State = "received"
				v.QueueID = ""
				v.LocalPending = true
			case "unknown":
				v.State = "unknown"
			}
			card := QueueInputView(v)
			if strings.Contains(card, "追加引导") != (variant == "active") {
				t.Fatal("unverified steer")
			}
			if strings.Contains(card, "当前轮结束后执行") {
				t.Fatal("false execution promise")
			}
			if variant == "stopped" && !strings.Contains(card, "等待显式继续") {
				t.Fatal("paused queue lost guidance")
			}
			if strings.Contains(card, "cancel_pending") != (variant == "received") {
				t.Fatal("unsafe local cancel button")
			}
		})
	}
}

func TestQueueInputWorktreeRestoreUsesFixedStatusTextAndLocalCancelOnly(t *testing.T) {
	statuses := map[string]string{
		"restoring":           "工作树已回收，正在从快照恢复。原消息已保存，无需重发。",
		"snapshot_missing":    "未找到工作树快照，暂时无法自动恢复。原消息已保存；可在 Codex App 恢复工作树后继续。",
		"identity_unverified": "暂时无法确认工作树的恢复位置。原消息已保存；请先在 Codex App 恢复工作树。",
		"conflict":            "工作树位置已被占用，自动恢复已停止。原消息已保存。",
		"unknown":             "正在核验工作树恢复结果。原消息已保存，请勿重复发送。",
		"unsupported":         "当前工作树暂不支持自动恢复。原消息已保存；可在 Codex App 恢复后继续。",
		"restored":            "工作树已恢复，正在提交原消息。",
		"failed":              "工作树恢复暂未完成。原消息已保存，可在 Codex App 恢复后继续。",
	}
	for status, label := range statuses {
		t.Run(status, func(t *testing.T) {
			card := QueueInputView(QueueView{
				State: "received", Token: "opaque-token", Revision: 4,
				WorktreeRestore: status, LocalPending: true,
			})
			if !strings.Contains(card, label) {
				t.Fatalf("restore status missing: %s", card)
			}
			if strings.Contains(card, "追加引导") || strings.Contains(card, "继续") && !strings.Contains(label, "继续") {
				t.Fatalf("restore card exposed native control: %s", card)
			}
			if !strings.Contains(card, "cancel_pending") {
				t.Fatalf("restore card lost local cancellation: %s", card)
			}
		})
	}
}

func TestQueueInputIgnoresUnallowlistedOrNonReceivedRestoreStatus(t *testing.T) {
	for _, v := range []QueueView{
		{State: "received", Token: "token", Revision: 1, WorktreeRestore: "secret error at /tmp/worktree", LocalPending: true},
		{State: "queued", Token: "token", QueueID: "queue", Revision: 1, WorktreeRestore: "restoring", Active: true},
		{State: "unknown", Token: "token", Revision: 1, WorktreeRestore: "restoring"},
	} {
		card := QueueInputView(v)
		if strings.Contains(card, "secret error") || strings.Contains(card, "/tmp/worktree") || strings.Contains(card, "工作树") {
			t.Fatalf("restore detail leaked or overrode state: %s", card)
		}
	}
}
