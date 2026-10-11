package cards

// QueueInput renders only operation state, never the human input body. Revision
// and the exact native queue ID travel in the callback; stale cards cannot act
// on a replacement queue entry after a restore or concurrent App operation.
func QueueInput(state, token, queueID string, revision int64) string {
	labels := map[string]string{
		"received": "已收到", "submitting": "正在提交", "accepted": "已进入会话",
		"queued": "已排队 · 当前轮结束后执行", "consumed": "已进入会话",
		"steering": "正在追加引导", "removing": "正在移除", "steered": "已追加引导",
		"removed": "已移除", "unknown": "正在核验 · 请勿重复提交", "failed": "提交失败",
	}
	label := labels[state]
	if label == "" {
		label = labels["unknown"]
	}
	els := []element{{"tag": "markdown", "content": label, "text_size": "notation"}}
	if state == "queued" && token != "" && queueID != "" && revision > 0 {
		button := func(label, action string) element {
			b := Button(label, action, token)
			b["behaviors"] = []element{{"type": "callback", "value": element{
				"action": action, "token": token, "queue_id": queueID, "revision": revision,
			}}}
			return b
		}
		els = append(els, element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "columns": []element{
			{"tag": "column", "width": "auto", "elements": []element{button("追加引导", "steer")}},
			{"tag": "column", "width": "auto", "elements": []element{button("移除", "remove")}},
		}})
	}
	return render(els)
}
