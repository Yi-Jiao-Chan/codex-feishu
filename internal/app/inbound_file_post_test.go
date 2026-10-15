package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
)

func TestReceiverPostTopLevelFilesReachOneCompleteNativeInput(t *testing.T) {
	r, db := receiverFixture(t)
	r.Config.Attachments.Enabled = true
	ctx := context.Background()
	var unsupported bool
	r.Command = func(_ context.Context, command InboundCommand) error { unsupported = command.Unsupported; return nil }
	// The observed EML post shape has no file node in either text representation.
	// Only the post's sibling files array identifies the original message resources.
	event := attachmentEvent(t, "mail-post", "ou_operator", "post", map[string]any{
		"title":      "邮件材料",
		"content":    [][]map[string]any{{{"tag": "at", "style": []string{}, "user_id": "@_user_1", "user_name": "机器人"}, {"tag": "text", "style": []string{}, "text": " 请检查邮件和表格。"}}},
		"content_v2": [][]map[string]any{{{"tag": "at", "style": []string{}, "user_id": "@_user_1", "user_name": "机器人"}, {"tag": "text", "style": []string{}, "text": " 请检查邮件和表格。"}}},
		"files":      []map[string]any{{"file_key": "file_v3_mail_fixture", "file_name": "测试邮件.eml", "is_folder": false}, {"file_key": "file_v3_csv_fixture", "file_name": "相关数据.csv", "is_folder": false}},
	}, true)
	if err := r.Message(ctx, event); err != nil {
		t.Fatal(err)
	}
	if unsupported {
		t.Fatal("valid file post was routed as unsupported")
	}
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	inputs, err := db.Submissions(ctx, scope)
	if err != nil || len(inputs) != 1 || inputs[0].State != "collecting" || !strings.Contains(inputs[0].Body, "请检查邮件和表格。") {
		t.Fatalf("post submitted as partial text: count=%d err=%v", len(inputs), err)
	}
	native := &appMultipartNative{serviceNative: &serviceNative{runtimeNative: &runtimeNative{turns: map[string][]codexclient.Turn{}}}}
	controller := &submissions.Controller{Store: db, Native: native, HostID: "host", AppID: "app", ThreadID: "thread"}
	if err := controller.Tick(ctx); err != nil || native.adds.Load() != 0 {
		t.Fatalf("post submitted before files were ready: adds=%d err=%v", native.adds.Load(), err)
	}
	eml := []byte("Subject: fixture mail\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nMAIL_FIXTURE_MARKER\r\n")
	csv := []byte("name,value\nfixture,42\n")
	gateway := &appResourceGateway{images: map[string][]byte{"mail-post:file_v3_mail_fixture": eml, "mail-post:file_v3_csv_fixture": csv}}
	worker := attachmentWorker(t, r, db, gateway)
	for i := 0; i < 2; i++ {
		if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
			t.Fatalf("file %d not prepared: worked=%v err=%v", i, worked, err)
		}
		if i == 0 {
			if err := controller.Tick(ctx); err != nil || native.adds.Load() != 0 {
				t.Fatalf("one file silently omitted: adds=%d err=%v", native.adds.Load(), err)
			}
		}
	}
	if err := controller.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if native.adds.Load() != 1 || len(native.inputs) != 1 || gateway.calls != 2 {
		t.Fatalf("file post submissions=%d multipart=%d downloads=%d", native.adds.Load(), len(native.inputs), gateway.calls)
	}
	var parts []map[string]any
	if err := json.Unmarshal(native.inputs[0], &parts); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, part := range parts {
		if part["type"] != "text" {
			t.Fatalf("ordinary file became unsupported native part: %#v", part["type"])
		}
		if value, ok := part["text"].(string); ok {
			text.WriteString(value)
		}
	}
	for _, want := range []string{"邮件材料", "请检查邮件和表格。", "测试邮件.eml", "相关数据.csv"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("native input lost %q", want)
		}
	}
	if strings.Count(text.String(), "请检查邮件和表格。") != 1 {
		t.Fatal("content/content_v2 duplicated the instruction")
	}
	refs, err := db.AttachmentCacheReferences(ctx, scope.HostID, scope.AppID)
	if err != nil {
		t.Fatal(err)
	}
	resources := 0
	for _, ref := range refs {
		if ref.Kind != "file" {
			continue
		}
		resources++
		path := filepath.Join(worker.Cache.Root, ref.FileID)
		if !strings.Contains(text.String(), path) {
			t.Fatal("native input omitted local file reference")
		}
		body, err := os.ReadFile(path)
		if err != nil || string(body) != string(eml) && string(body) != string(csv) {
			t.Fatalf("original file bytes unavailable: %v", err)
		}
	}
	if resources != 2 {
		t.Fatalf("persisted resource count=%d", resources)
	}
	if err := r.Message(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err := controller.Tick(ctx); err != nil || native.adds.Load() != 1 || gateway.calls != 2 {
		t.Fatalf("replayed post duplicated input: adds=%d downloads=%d err=%v", native.adds.Load(), gateway.calls, err)
	}
}
