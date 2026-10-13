package cards

import (
	"encoding/json"
	"strings"
	"testing"
)

func humanElements(t *testing.T, card string) []any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(card), &document); err != nil {
		t.Fatal(err)
	}
	if _, exists := document["header"]; exists {
		t.Fatal("human card gained a colored/title header")
	}
	if _, exists := document["config"].(map[string]any)["width_mode"]; exists {
		t.Fatal("human message was forced to compact width")
	}
	return document["body"].(map[string]any)["elements"].([]any)
}

func TestHumanWithoutAttachmentsRetainsOriginalMessage(t *testing.T) {
	body := "# 用户正文\n\n```go\nfmt.Println(\"hi\")\n```"
	if Human("Codex 端", body, nil) != Message("人类发送", "Codex 端", body) {
		t.Fatal("attachment-free message changed")
	}
}

func TestHumanMixedAttachmentsKeepBodyFirstAndMetadataCollapsed(t *testing.T) {
	body := "# 检查图片\n\n```go\nfmt.Println(\"hello\")\n```\n\n<at id=\"untrusted\"></at>"
	card := Human("Codex App · 16:20", body, []AttachmentView{
		{Kind: "image", Name: "random-uuid.png", Path: "/not-read/random-uuid.png", ImageKey: "img_v3_confirmed", Status: "uploaded"},
		{Kind: "file", Name: "report.pdf", Path: "/not-read/report.pdf"},
		{Kind: "file", Name: "backup.zip", Path: "/not-read/backup.zip"},
		{Kind: "image", Name: "missing.png", Path: "/not-read/missing.png", Status: "unsupported"},
	})
	elements := humanElements(t, card)
	if len(elements) != 4 {
		t.Fatalf("unexpected number of elements: %d", len(elements))
	}
	first := elements[0].(map[string]any)
	if first["text_size"] != "normal" || first["content"] != Safe(body) {
		t.Fatal("real body Markdown/code changed")
	}
	image := elements[1].(map[string]any)
	if image["tag"] != "img" || image["img_key"] != "img_v3_confirmed" || image["preview"] != true || image["scale_type"] != "crop_top" || image["size"] != "large" {
		t.Fatalf("missing native image fields: %#v", image)
	}
	alt := image["alt"].(map[string]any)
	if alt["tag"] != "plain_text" || alt["content"] != "附件图片 1" {
		t.Fatal("image alt exposes random filenames or paths")
	}
	panel := elements[2].(map[string]any)
	title := panel["header"].(map[string]any)["title"].(map[string]any)
	if panel["tag"] != "collapsible_panel" || panel["expanded"] != false || title["content"] != "附件 · 2张图片 · 2个文件" || title["text_size"] != "notation" || title["text_color"] != "grey" {
		t.Fatalf("attachment disclosure changed: %#v", panel)
	}
	for _, value := range panel["elements"].([]any) {
		if value.(map[string]any)["text_size"] != "notation" {
			t.Fatal("attachment details gained large text")
		}
	}
	for _, n := range []int{0, 1, 3} {
		raw, _ := json.Marshal(elements[n])
		if strings.Contains(string(raw), "/not-read/") || strings.Contains(string(raw), "random-uuid") {
			t.Fatal("attachment metadata is visible outside disclosure")
		}
	}
	last := elements[len(elements)-1].(map[string]any)
	if last["content"] != "> 人类发送 · Codex App · 16:20" || last["text_size"] != "notation" {
		t.Fatal("source footer is not at the bottom")
	}
	if !strings.Contains(card, "图片暂不可预览，请回Codex App查看") || strings.Contains(card, `"tag":"button"`) || strings.Contains(card, "<at") || strings.Contains(card, `"tag":"file"`) || strings.Contains(card, "file://") {
		t.Fatal("failed preview, mention safety, or file-list contract changed")
	}
}

func TestHumanImageOnlyAndDataURLNeverLeaks(t *testing.T) {
	dataURL := "data:image/png;base64,VERY_PRIVATE_PAYLOAD"
	card := Human("Codex 端", "", []AttachmentView{
		{Kind: "image", ImageKey: "img_v3_one"},
		{Kind: "image", Name: dataURL, Path: dataURL, ImageKey: dataURL, Status: "failed"},
	})
	elements := humanElements(t, card)
	first := elements[0].(map[string]any)
	if first["content"] != "发送了 2 个附件" || first["text_size"] != "notation" || len(elements) != 4 {
		t.Fatal("image-only card lost its small human summary or successful image")
	}
	if strings.Contains(card, "data:") || strings.Contains(card, "VERY_PRIVATE_PAYLOAD") {
		t.Fatal("raw data URL leaked into rendered card")
	}
}

func TestHumanAttachmentNamesStayDataAndAllAttachmentsRemain(t *testing.T) {
	attachments := []AttachmentView{{Kind: "file", Name: "[click](https://example.invalid)", Path: "/tmp/<at id=\"x\">\n# title"}}
	for n := 0; n < 40; n++ {
		attachments = append(attachments, AttachmentView{Kind: "image", ImageKey: "img_v3_test"})
	}
	elements := humanElements(t, Human("Codex 端", "正文", attachments))
	count := 0
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if v["tag"] == "img" {
				count++
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(elements)
	if count != 40 {
		t.Fatal("pure renderer dropped images before caller budget decisions")
	}
	panel := elements[len(elements)-2].(map[string]any)
	details := panel["elements"].([]any)
	if len(details) != len(attachments) {
		t.Fatal("pure renderer dropped attachment details")
	}
	content := details[0].(map[string]any)["content"].(string)
	if strings.Contains(content, "[click](") || strings.Contains(content, "<at") || strings.Contains(content, "\n# title") {
		t.Fatal("attachment filename or path became active Markdown")
	}
}
