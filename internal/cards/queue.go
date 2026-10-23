package cards

import "strconv"

// QueueView contains verified operation facts, never the human message body.
// Active means a currently verified native turn; unknown/idle never offers steer.
type QueueView struct {
	State, Token, QueueID         string
	WorktreeRestore               string
	Revision                      int64
	QueueOrdinal                  int
	Active, Stopped, LocalPending bool
}

// QueueInput retains the old entry point with conservative unknown turn state.
func QueueInput(state, token, queueID string, revision int64) string {
	return QueueInputView(QueueView{State: state, Token: token, QueueID: queueID, Revision: revision})
}
func QueueInputView(v QueueView) string {
	labels := map[string]string{"received": "已收到 · 待提交", "submitting": "正在提交", "accepted": "已进入会话", "queued": "已排队 · 等待执行状态核验", "consumed": "已进入会话", "steering": "正在追加引导", "removing": "正在移除", "steered": "已追加引导", "removed": "已移除", "unknown": "正在核验 · 请勿重复提交", "failed": "提交失败"}
	worktreeRestoreLabels := map[string]string{
		"restoring":           "工作树已回收，正在从快照恢复。原消息已保存，无需重发。",
		"snapshot_missing":    "未找到工作树快照，暂时无法自动恢复。原消息已保存；可在 Codex App 恢复工作树后继续。",
		"identity_unverified": "暂时无法确认工作树的恢复位置。原消息已保存；请先在 Codex App 恢复工作树。",
		"conflict":            "工作树位置已被占用，自动恢复已停止。原消息已保存。",
		"unknown":             "正在核验工作树恢复结果。原消息已保存，请勿重复发送。",
		"unsupported":         "当前工作树暂不支持自动恢复。原消息已保存；可在 Codex App 恢复后继续。",
		"restored":            "工作树已恢复，正在提交原消息。",
		"failed":              "工作树恢复暂未完成。原消息已保存，可在 Codex App 恢复后继续。",
	}
	label := labels[v.State]
	if label == "" {
		label = labels["unknown"]
	}
	if v.State == "queued" {
		if v.Stopped {
			label = "已排队 · 队列已保留，等待显式继续"
		} else if v.Active {
			// An active turn does not prove the separate native queue is
			// unpaused, so it grants steer without promising automatic start.
			label = "已排队"
		}
		if v.QueueOrdinal > 0 {
			label += " · 第 " + strconv.Itoa(v.QueueOrdinal) + " 条"
		}
	}
	if v.State == "received" && v.LocalPending {
		label = "已收到 · 尚未提交，可取消本地待提交消息"
		if restoreLabel := worktreeRestoreLabels[v.WorktreeRestore]; restoreLabel != "" {
			label = restoreLabel
		}
	}
	els := []element{{"tag": "markdown", "content": label, "text_size": "notation"}}
	els = append(els, queueControls(v)...)
	return render(els)
}

// queueControls renders the existing native queue actions for a verified
// queue projection. Callers supply the authoritative state and credentials;
// this helper only exposes actions that the projection proves are safe.
func queueControls(v QueueView) []element {
	if v.Token == "" || v.Revision <= 0 {
		return nil
	}
	button := func(label, action string) element {
		b := Button(label, action, v.Token)
		b["behaviors"] = []element{{"type": "callback", "value": element{"action": action, "token": v.Token, "queue_id": v.QueueID, "revision": v.Revision}}}
		return b
	}
	var buttons []element
	if v.State == "queued" && v.QueueID != "" {
		if v.Active && !v.Stopped {
			buttons = append(buttons, button("追加引导", "steer"))
		}
		buttons = append(buttons, button("移除", "remove"))
	}
	if v.State == "received" && v.LocalPending && v.QueueID == "" {
		buttons = append(buttons, button("取消待提交", "cancel_pending"))
	}
	if len(buttons) == 0 {
		return nil
	}
	columns := make([]element, 0, len(buttons))
	for _, b := range buttons {
		columns = append(columns, element{"tag": "column", "width": "auto", "elements": []element{b}})
	}
	return []element{{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "columns": columns}}
}
