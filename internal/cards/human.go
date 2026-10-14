package cards

import (
	"fmt"
	"strings"
)

type AttachmentView struct {
	Kind, Name, Path, ImageKey, Status string
}

// Human renders attachment metadata and already uploaded image keys, with no
// file access, upload, or URL resolution. Callers own card splitting and limits.
func Human(source, body string, attachments []AttachmentView) string {
	if len(attachments) == 0 {
		return Message("人类发送", source, body)
	}
	var elements []element
	if strings.TrimSpace(body) != "" {
		elements = append(elements, publicBodyElements(body, "normal")...)
	} else {
		elements = append(elements, element{"tag": "markdown", "content": fmt.Sprintf("发送了 %d 个附件", len(attachments)), "text_size": "notation"})
	}
	images, files := 0, 0
	var previews []element
	var details []element
	for _, attachment := range attachments {
		label := "文件"
		if attachment.Kind == "image" {
			images++
			label = "图片"
			if attachment.ImageKey != "" && !humanDataURL(attachment.ImageKey) {
				previews = append(previews, element{
					"tag": "img", "img_key": attachment.ImageKey,
					"alt":     element{"tag": "plain_text", "content": fmt.Sprintf("附件图片 %d", images)},
					"preview": true, "scale_type": "crop_top", "size": "large",
				})
			}
		} else {
			files++
		}
		text := label
		if attachment.Name != "" && !humanDataURL(attachment.Name) {
			text += " · " + humanMetadata(attachment.Name)
		}
		if attachment.Path != "" && !humanDataURL(attachment.Path) {
			text += "\n" + humanMetadata(attachment.Path)
		}
		if attachment.Kind == "image" && (attachment.ImageKey == "" || humanDataURL(attachment.ImageKey)) {
			text += "\n图片暂不可预览，请回Codex App查看"
		}
		details = append(details, element{"tag": "markdown", "content": text, "text_size": "notation", "margin": "0px"})
	}
	for n := 0; n < len(previews); n += 2 {
		if n+1 == len(previews) {
			elements = append(elements, previews[n])
			continue
		}
		elements = append(elements, element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "columns": []element{
			{"tag": "column", "width": "weighted", "weight": 1, "elements": []element{previews[n]}},
			{"tag": "column", "width": "weighted", "weight": 1, "elements": []element{previews[n+1]}},
		}})
	}
	var counts []string
	if images > 0 {
		counts = append(counts, fmt.Sprintf("%d张图片", images))
	}
	if files > 0 {
		counts = append(counts, fmt.Sprintf("%d个文件", files))
	}
	panel := taskPanel("附件 · "+strings.Join(counts, " · "), details)
	panel["element_id"] = "human_attachments"
	elements = append(elements, panel, footer("人类发送 · "+source))
	return renderWidth(elements, "")
}

func humanDataURL(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "data:")
}

// Metadata must stay text even when a filename contains Markdown or XML.
func humanMetadata(value string) string {
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "#", "\\#", "!", "\\!").Replace(Safe(value))
}
