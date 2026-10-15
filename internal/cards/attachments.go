package cards

import (
	"fmt"
	"strings"
)

type AttachmentDraftView struct {
	State, Token string
	Revision     int64
	Items        []AttachmentItem
	Sources      []string
}
type AttachmentItem struct{ ID, Name, State, Error string }

func AttachmentDraft(v AttachmentDraftView) string {
	label := map[string]string{"open": "附件待发送", "collecting": "正在收取附件 · 尚未进入 Codex 队列", "received": "附件已收齐 · 等待提交 Codex", "queued": "附件已进入原生队列", "consumed": "附件已进入会话", "steered": "附件已追加引导", "failed": "附件收取失败 · 本批尚未提交", "native_failed": "Codex 提交失败", "unknown": "提交结果正在核验 · 请勿重复发送", "cleared": "附件草稿已清空", "removed": "本批输入已移除"}[v.State]
	if label == "" {
		label = "附件状态正在核验"
	}
	els := []element{{"tag": "markdown", "content": fmt.Sprintf("**%s** · %d 个附件", label, len(v.Items)), "text_size": "normal"}}
	button := func(label, action, part string) element {
		b := Button(label, action, v.Token)
		b["behaviors"] = []element{{"type": "callback", "value": element{"action": action, "token": v.Token, "revision": v.Revision, "part_id": part}}}
		return b
	}
	failed := false
	for i, item := range v.Items {
		name := item.Name
		if name == "" {
			name = fmt.Sprintf("图片 %d", i+1)
		}
		name = string([]rune(name)[:min(len([]rune(name)), 160)])
		status := map[string]string{"pending": "等待收取", "downloading": "收取中", "ready": "已收取", "failed": "收取失败"}[item.State]
		if item.Error != "" {
			status += "（" + attachmentError(item.Error) + "）"
		}
		els = append(els, element{"tag": "markdown", "content": fmt.Sprintf("%d. %s · %s", i+1, SafeMarkdown(name), status), "text_size": "notation"})
		if item.State == "failed" && (v.State == "open" || v.State == "failed") {
			failed = true
			els = append(els, button("移除此附件", "attachment_remove", item.ID))
		}
	}
	if v.Token != "" && v.Revision > 0 {
		if v.State == "open" {
			els = append(els, element{"tag": "markdown", "content": "继续发送附件会加入本批；下一条普通 @ 对话会带上本批附件。管理命令不会发送附件。", "text_size": "notation"})
			send := button("发送给 Codex", "attachment_send", "")
			send["name"], send["form_action_type"] = "attachment_send", "submit"
			els = append(els, element{"tag": "form", "name": "attachment_form", "elements": []element{
				{"tag": "input", "name": "attachment_notes", "input_type": "multiline_text", "placeholder": element{"tag": "plain_text", "content": "补充说明（可留空）"}, "max_length": 1000}, send,
			}})
		}
		if failed {
			els = append(els, button("重试收取", "attachment_retry", ""))
		}
		if v.State == "open" || v.State == "failed" {
			els = append(els, button("清空", "attachment_clear", ""))
		}
	}
	if len(v.Sources) > 0 {
		els = append(els, footer("本批来源消息："+strings.Join(v.Sources, "、")))
	}
	return render(els)
}

func attachmentError(code string) string {
	switch code {
	case "size-limit", "batch-size-limit":
		return "超过大小限制"
	case "input-size-limit":
		return "本批说明或文件名过长，请清空后缩短重发"
	case "pixel-limit":
		return "图片像素超过限制"
	case "image-type":
		return "仅支持静态 PNG/JPEG/WebP"
	case "disk-quota":
		return "附件存储额度不足"
	case "download-rejected":
		return "消息资源不可读取，请检查权限或原消息"
	case "disk-error":
		return "附件无法保存"
	case "integrity":
		return "文件校验失败"
	default:
		return "下载未完成，请重试"
	}
}
