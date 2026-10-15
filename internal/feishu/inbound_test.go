package feishu

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func inboundFixture(kind string, content any) *im.EventMessage {
	b, _ := json.Marshal(content)
	s := string(b)
	return &im.EventMessage{MessageType: &kind, Content: &s}
}

func inboundMentionFixture(key, id, name string) *im.MentionEvent {
	return &im.MentionEvent{Key: &key, Id: &im.UserId{OpenId: &id}, Name: &name}
}

func TestParseInboundPostOrderAndAlternativeContent(t *testing.T) {
	msg := inboundFixture("post", map[string]any{
		"title": "说明", "content": []any{
			[]any{map[string]any{"tag": "at", "user_id": "@_user_1"}, map[string]any{"tag": "text", "text": "先看"}, map[string]any{"tag": "img", "image_key": "img_v3_first"}, map[string]any{"tag": "text", "text": "再看"}, map[string]any{"tag": "img", "image_key": "img_v3_second"}},
			[]any{map[string]any{"tag": "a", "text": "资料", "href": "https://example.invalid/file.png"}, map[string]any{"tag": "at", "user_id": "@_user_2"}},
			[]any{map[string]any{"tag": "code_block", "language": "go", "text": "x := `ok`\nprintln(x)"}},
		}, "content_v2": []any{[]any{map[string]any{"tag": "img", "image_key": "img_v3_duplicate"}}},
	})
	msg.Mentions = []*im.MentionEvent{inboundMentionFixture("@_user_1", "ou_bot", "机器人"), inboundMentionFixture("@_user_2", "ou_person", "小明")}
	got, err := ParseInbound(msg, "ou_bot")
	if err != nil {
		t.Fatal(err)
	}
	want := []InboundPart{
		{Type: "text", Text: "说明\n先看"}, {Type: "image", ResourceKey: "img_v3_first"},
		{Type: "text", Text: "再看"}, {Type: "image", ResourceKey: "img_v3_second"},
		{Type: "text", Text: "\n资料 (https://example.invalid/file.png)@小明\n```go\nx := `ok`\nprintln(x)\n```"},
	}
	if !reflect.DeepEqual(got.Parts, want) {
		t.Fatalf("parts = %#v", got.Parts)
	}
	if got.Text != want[0].Text+want[2].Text+want[4].Text {
		t.Fatalf("text = %q", got.Text)
	}
}

func TestParseInboundV2FallbackAndPresentEmptyContent(t *testing.T) {
	for _, primary := range []any{nil, []any{}} {
		msg := inboundFixture("post", map[string]any{"content": primary, "content_v2": [][]map[string]string{{{"tag": "md", "text": "**说明**\nhttps://example.invalid/img.png"}, {"tag": "img", "image_key": "img_v2_key"}}}})
		got, err := ParseInbound(msg, "")
		if err != nil {
			t.Fatal(err)
		}
		if primary == nil && len(got.Parts) != 2 {
			t.Fatalf("fallback parts = %#v", got.Parts)
		}
		if primary != nil && len(got.Parts) != 0 {
			t.Fatalf("present content must win: %#v", got.Parts)
		}
	}
}

func TestParseInboundMentionsAreOnlyMappedContent(t *testing.T) {
	msg := inboundFixture("text", map[string]string{"text": "@_user_1 @_user_10 @_user_100\n@机器人 <at user_id=\"ou_bot\">bot</at>\n  code  "})
	msg.Mentions = []*im.MentionEvent{inboundMentionFixture("@_user_1", "ou_bot", "机器人"), inboundMentionFixture("@_user_10", "ou_person", "同事")}
	got, err := ParseInbound(msg, "ou_bot")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != " @同事 @_user_100\n@机器人 <at user_id=\"ou_bot\">bot</at>\n  code  " {
		t.Fatalf("text = %q", got.Text)
	}
	msg = inboundFixture("post", map[string]any{"content": [][]map[string]string{{{"tag": "at", "user_id": "ou_bot", "user_name": "机器人"}}}})
	got, err = ParseInbound(msg, "ou_bot")
	if err != nil || got.Text != "@机器人" {
		t.Fatalf("unmapped at = %#v, %v", got, err)
	}
}

func TestParseInboundSingleResourcesAndNoURLExtraction(t *testing.T) {
	for _, kind := range []string{"image", "file"} {
		keyField := "image_key"
		if kind == "file" {
			keyField = "file_key"
		}
		got, err := ParseInbound(inboundFixture(kind, map[string]string{keyField: "resource_v3_123", "file_name": "中文 测试.pdf"}), "")
		if err != nil || len(got.Parts) != 1 || got.Parts[0].ResourceKey != "resource_v3_123" || got.Parts[0].Name != "中文 测试.pdf" {
			t.Fatalf("%s: %#v, %v", kind, got, err)
		}
	}
	got, err := ParseInbound(inboundFixture("text", map[string]string{"text": "下载 https://example.invalid/image.png img_v3_fake"}), "")
	if err != nil || len(got.Parts) != 1 || got.Parts[0].Type != "text" {
		t.Fatalf("URL extraction: %#v %v", got, err)
	}
}

func TestParseInboundRejectsWholeUnsupportedOrMalformedMessage(t *testing.T) {
	for _, node := range []map[string]string{
		{"tag": "media", "file_key": "file_v3_video"}, {"tag": "file", "file_key": "file_v3_doc"}, {"tag": "unknown"},
		{"tag": "text", "text": "not a resource", "image_key": "img_hidden"}, {"tag": "img", "image_key": "https://example.invalid/image"},
	} {
		msg := inboundFixture("post", map[string]any{"content": [][]map[string]string{{{"tag": "text", "text": "private-body"}, node}}})
		got, err := ParseInbound(msg, "")
		if err == nil || len(got.Parts) != 0 || got.Text != "" || strings.Contains(err.Error(), "private-body") {
			t.Fatalf("partial result: %#v %v", got, err)
		}
	}
	for _, msg := range []*im.EventMessage{nil, {}, inboundFixture("image", map[string]string{"image_key": "../secret"}), inboundFixture("post", map[string]any{"content": nil})} {
		if _, err := ParseInbound(msg, ""); !errors.Is(err, ErrInvalidInbound) {
			t.Fatalf("error = %v", err)
		}
	}
	if _, err := ParseInbound(inboundFixture("text", map[string]string{"text": strings.Repeat("x", maxInboundContent)}), ""); !errors.Is(err, ErrInvalidInbound) {
		t.Fatalf("oversized: %v", err)
	}
}

func TestParseInboundBoundsMentionExpansionAndNodeCount(t *testing.T) {
	msg := inboundFixture("text", map[string]string{"text": strings.Repeat("@_user_1", 1000)})
	msg.Mentions = []*im.MentionEvent{inboundMentionFixture("@_user_1", "ou_person", strings.Repeat("x", 4096))}
	if got, err := ParseInbound(msg, "ou_bot"); !errors.Is(err, ErrInvalidInbound) || len(got.Parts) != 0 {
		t.Fatalf("unbounded mention expansion: %d parts, %v", len(got.Parts), err)
	}
	row := make([]map[string]string, maxInboundNodes+1)
	for i := range row {
		row[i] = map[string]string{"tag": "img", "image_key": "img_key"}
	}
	if got, err := ParseInbound(inboundFixture("post", map[string]any{"content": []any{row}}), ""); !errors.Is(err, ErrInvalidInbound) || len(got.Parts) != 0 {
		t.Fatalf("unbounded nodes: %d parts, %v", len(got.Parts), err)
	}
}

func TestParseInboundCodeFencePreservesEmbeddedFences(t *testing.T) {
	source := "before\n```\nafter"
	got, err := ParseInbound(inboundFixture("post", map[string]any{"content": [][]map[string]string{{{"tag": "code_block", "language": "text", "text": source}}}}), "")
	if err != nil || got.Text != "````text\n"+source+"\n````" {
		t.Fatalf("code = %q, %v", got.Text, err)
	}
	// A long single run must be scanned once, rather than trying every fence length.
	source = strings.Repeat("`", 100000)
	got, err = ParseInbound(inboundFixture("post", map[string]any{"content": [][]map[string]string{{{"tag": "code_block", "text": source}}}}), "")
	if err != nil || len(got.Text) != 300004 {
		t.Fatalf("long fence: length=%d err=%v", len(got.Text), err)
	}
}

// This shape follows the observed Feishu post readback: the files live beside
// content/content_v2, rather than inside either representation's row nodes.
func TestParseInboundPostTopLevelFiles(t *testing.T) {
	for _, count := range []int{1, 2} {
		files := []map[string]any{{"file_key": "file_v3_mail_fixture", "file_name": "会议记录.eml", "is_folder": false}}
		if count == 2 {
			files = append(files, map[string]any{"file_key": "file_v3_csv_fixture", "file_name": "附件 数据.csv", "is_folder": false})
		}
		msg := inboundFixture("post", map[string]any{
			"title":      "邮件材料",
			"content":    [][]map[string]string{{{"tag": "at", "user_id": "@_user_1", "user_name": "机器人"}, {"tag": "text", "text": "请查看"}, {"tag": "img", "image_key": "img_v3_fixture"}, {"tag": "text", "text": "再看邮件。"}}},
			"content_v2": [][]map[string]string{{{"tag": "text", "text": "duplicate-alternative"}, {"tag": "img", "image_key": "img_v3_duplicate"}}},
			"files":      files,
		})
		msg.Mentions = []*im.MentionEvent{inboundMentionFixture("@_user_1", "ou_bot", "机器人")}
		got, err := ParseInbound(msg, "ou_bot")
		if err != nil {
			t.Fatal(err)
		}
		want := []InboundPart{{Type: "text", Text: "邮件材料\n请查看"}, {Type: "image", ResourceKey: "img_v3_fixture"}, {Type: "text", Text: "再看邮件。"}, {Type: "file", ResourceKey: "file_v3_mail_fixture", Name: "会议记录.eml"}}
		if count == 2 {
			want = append(want, InboundPart{Type: "file", ResourceKey: "file_v3_csv_fixture", Name: "附件 数据.csv"})
		}
		if !reflect.DeepEqual(got.Parts, want) || got.Text != "邮件材料\n请查看再看邮件。" {
			t.Fatalf("top-level files lost or reordered: %#v", got)
		}
	}
}

func TestParseInboundPostTopLevelFilesWithV2Fallback(t *testing.T) {
	msg := inboundFixture("post", map[string]any{
		"title":      "材料",
		"content_v2": [][]map[string]string{{{"tag": "at", "user_id": "@_user_1"}, {"tag": "text", "text": "请检查邮件。"}}},
		"files":      []map[string]any{{"file_key": "file_v3_fallback", "file_name": "测试邮件.eml", "is_folder": false}},
	})
	msg.Mentions = []*im.MentionEvent{inboundMentionFixture("@_user_1", "ou_bot", "机器人")}
	got, err := ParseInbound(msg, "ou_bot")
	want := []InboundPart{{Type: "text", Text: "材料\n请检查邮件。"}, {Type: "file", ResourceKey: "file_v3_fallback", Name: "测试邮件.eml"}}
	if err != nil || !reflect.DeepEqual(got.Parts, want) {
		t.Fatalf("v2 with files=%#v err=%v", got, err)
	}
}

func TestParseInboundPostRejectsInvalidTopLevelFilesWithoutPartialSubmission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files any
		want  error
	}{
		{"object", map[string]any{"file_key": "file_v3_fixture"}, ErrInvalidInbound},
		{"null", nil, ErrInvalidInbound},
		{"null-member", []any{nil}, ErrInvalidInbound},
		{"empty-key", []any{map[string]any{"file_key": "", "file_name": "test.eml", "is_folder": false}}, ErrInvalidInbound},
		{"missing-key", []any{map[string]any{"file_name": "test.eml", "is_folder": false}}, ErrInvalidInbound},
		{"invalid-key", []any{map[string]any{"file_key": "https://example.invalid/file", "file_name": "test.eml", "is_folder": false}}, ErrInvalidInbound},
		{"wrong-key-type", []any{map[string]any{"file_key": 42, "file_name": "test.eml", "is_folder": false}}, ErrInvalidInbound},
		{"empty-name", []any{map[string]any{"file_key": "file_v3_fixture", "file_name": "", "is_folder": false}}, ErrInvalidInbound},
		{"missing-name", []any{map[string]any{"file_key": "file_v3_fixture", "is_folder": false}}, ErrInvalidInbound},
		{"wrong-name-type", []any{map[string]any{"file_key": "file_v3_fixture", "file_name": 42, "is_folder": false}}, ErrInvalidInbound},
		{"folder", []any{map[string]any{"file_key": "file_v3_fixture", "file_name": "folder", "is_folder": true}}, ErrUnsupportedInbound},
		{"wrong-folder-type", []any{map[string]any{"file_key": "file_v3_fixture", "file_name": "test.eml", "is_folder": "false"}}, ErrInvalidInbound},
		{"missing-folder-type", []any{map[string]any{"file_key": "file_v3_fixture", "file_name": "test.eml"}}, ErrInvalidInbound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := inboundFixture("post", map[string]any{"content": [][]map[string]string{{{"tag": "text", "text": "private-fixture-text"}, {"tag": "img", "image_key": "img_v3_fixture"}}}, "files": tc.files})
			got, err := ParseInbound(msg, "")
			if !errors.Is(err, tc.want) || got.Text != "" || len(got.Parts) != 0 {
				t.Fatalf("invalid file produced partial submission: %#v %v", got, err)
			}
			if strings.Contains(err.Error(), "private-fixture-text") || strings.Contains(err.Error(), "file_v3_fixture") {
				t.Fatal("parser error exposed content")
			}
		})
	}
}

func TestParseInboundPostCountsTopLevelFilesAgainstNodeLimit(t *testing.T) {
	for _, textNodes := range []int{maxInboundNodes - 1, maxInboundNodes} {
		row := make([]map[string]string, textNodes)
		for i := range row {
			row[i] = map[string]string{"tag": "text", "text": ""}
		}
		msg := inboundFixture("post", map[string]any{"content": []any{row}, "files": []map[string]any{{"file_key": "file_v3_fixture", "file_name": "test.eml", "is_folder": false}}})
		got, err := ParseInbound(msg, "")
		if textNodes == maxInboundNodes-1 {
			if err != nil || len(got.Parts) != 1 || got.Parts[0].Type != "file" {
				t.Fatalf("boundary files=%#v err=%v", got, err)
			}
		} else if !errors.Is(err, ErrInvalidInbound) || len(got.Parts) != 0 {
			t.Fatalf("files bypassed node limit: %#v %v", got, err)
		}
	}
}
