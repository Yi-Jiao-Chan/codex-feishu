// Package cards renders native Feishu Card JSON 2.0, with no I/O.
package cards

import (
	"encoding/json"
	"html"
	"strings"
)

type element = map[string]any

func render(body []element) string {
	b, _ := json.Marshal(map[string]any{"schema": "2.0", "config": map[string]any{"update_multi": true, "width_mode": "compact"}, "body": element{"elements": body, "padding": "8px 12px", "vertical_spacing": "4px"}})
	return string(b)
}

// Escape mention XML from content. Only explicit policies may create mentions.
func Safe(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "<", "&lt;"), ">", "&gt;")
}
func footer(s string) element {
	return element{"tag": "markdown", "content": "> " + Safe(s), "text_size": "notation"}
}
func Message(role, source, body string) string {
	return render([]element{{"tag": "markdown", "content": Safe(body), "text_size": "normal"}, footer(role + " · " + source)})
}
func Button(label, action, token string) element {
	return element{"tag": "button", "type": "text", "size": "small", "text": element{"tag": "plain_text", "content": label}, "behaviors": []element{{"type": "callback", "value": element{"action": action, "token": token, "revision": 1}}}}
}
func Queue(state, token string) string {
	labels := map[string]string{"received": "已收到", "queued": "已排队", "consumed": "已进入会话", "steered": "已追加引导", "removed": "已移除", "unknown": "正在核验", "failed": "提交失败"}
	title := labels[state]
	if title == "" {
		title = "正在核验"
	}
	elements := []element{{"tag": "markdown", "content": title, "text_size": "notation"}}
	if state == "queued" {
		elements[0]["content"] = "已排队 · 当前轮结束后执行"
		elements = append(elements, element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "columns": []element{
			{"tag": "column", "width": "auto", "elements": []element{Button("追加引导", "steer", token)}},
			{"tag": "column", "width": "auto", "elements": []element{Button("移除", "remove", token)}},
		}})
	}
	return render(elements)
}
func Probe(token string) string {
	return render([]element{{"tag": "markdown", "content": "**Codex × 飞书 · M0 联调**\n验证原生卡片渲染与交互。点击下方按钮后应收到回调确认。"}, Button("确认卡片可用", "probe", token), footer("验证消息 · 仅本人")})
}
func Mention(openID string) string { return "<at id=\"" + html.EscapeString(openID) + "\"></at>" }

// Progress is a compact, mutable per-turn card. Final replies use Message.
func Progress(status, body string) string {
	elements := []element{{"tag": "markdown", "content": Safe(status), "text_size": "notation"}}
	if body != "" {
		elements = append(elements, element{"tag": "markdown", "content": Safe(body), "text_size": "notation"})
	}
	return render(elements)
}
