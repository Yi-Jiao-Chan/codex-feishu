package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/attachments"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func attachmentEvent(t *testing.T, id, user, kind string, content any, mentioned bool) *im.P2MessageReceiveV1 {
	t.Helper()
	e := receiverMessage(t, "chat", user, id, "group", mentioned)
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	e.Event.Message.MessageType, e.Event.Message.Content = &kind, &body
	return e
}

type appResourceGateway struct {
	images map[string][]byte
	calls  int
}

func (g *appResourceGateway) OpenMessageResource(_ context.Context, message, key, kind string) (io.ReadCloser, feishu.ResourceMetadata, error) {
	g.calls++
	body, ok := g.images[message+":"+key]
	if !ok {
		return nil, feishu.ResourceMetadata{}, &feishu.Rejected{Code: 234003, HTTPStatus: 400}
	}
	contentType := "application/octet-stream"
	if kind == "image" {
		contentType = "image/png"
	}
	return io.NopCloser(bytes.NewReader(body)), feishu.ResourceMetadata{ContentType: contentType, ContentLength: int64(len(body))}, nil
}

type appMultipartNative struct {
	*serviceNative
	inputs []json.RawMessage
}

func (n *appMultipartNative) AddInput(ctx context.Context, thread string, input json.RawMessage, client string) (codexclient.Queued, error) {
	n.inputs = append(n.inputs, append(json.RawMessage{}, input...))
	return n.enqueue(thread, input, client)
}
func (n *appMultipartNative) Add(_ context.Context, thread, body, client string) (codexclient.Queued, error) {
	input, _ := json.Marshal(codexclient.TextInput(body))
	return n.enqueue(thread, input, client)
}
func (n *appMultipartNative) enqueue(thread string, input json.RawMessage, client string) (codexclient.Queued, error) {
	if thread != "thread" {
		return codexclient.Queued{}, errors.New("wrong thread")
	}
	n.adds.Add(1)
	q := codexclient.Queued{ID: "native-" + client, Input: input, ClientUserMessageID: &client}
	n.qmu.Lock()
	n.queue = append(n.queue, q)
	n.qmu.Unlock()
	return q, nil
}
func (*appMultipartNative) SteerInput(context.Context, string, string, json.RawMessage, string) error {
	return errors.New("unexpected fixture steer")
}

func attachmentWorker(t *testing.T, r *Receiver, db *store.Store, g *appResourceGateway) *attachments.Worker {
	t.Helper()
	limits := r.Config.AttachmentLimits()
	cache, err := attachments.NewCache(filepath.Join(t.TempDir(), "cache"), limits)
	if err != nil {
		t.Fatal(err)
	}
	return &attachments.Worker{Store: db, Cache: cache, Gateway: g, HostID: "host", AppID: "app"}
}

func TestReceiverAttachmentDraftToCompleteNativeInput(t *testing.T) {
	r, db := receiverFixture(t)
	r.Config.Attachments.Enabled, r.Config.Attachments.ReceiveStandalone = true, true
	ctx := context.Background()
	var pngBody bytes.Buffer
	if err := png.Encode(&pngBody, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	g := &appResourceGateway{images: map[string][]byte{"source-image:img_fixture": pngBody.Bytes(), "source-file:file_fixture": []byte("CSV_MARKER,42\n")}}
	worker := attachmentWorker(t, r, db, g)
	for _, e := range []*im.P2MessageReceiveV1{
		attachmentEvent(t, "foreign", "ou_other", "image", map[string]string{"image_key": "img_foreign"}, false),
		attachmentEvent(t, "not-mentioned", "ou_operator", "text", map[string]string{"text": "should not submit"}, false),
		attachmentEvent(t, "source-image", "ou_operator", "image", map[string]string{"image_key": "img_fixture"}, false),
		attachmentEvent(t, "source-file", "ou_operator", "file", map[string]string{"file_key": "file_fixture", "file_name": "中文 报告.csv"}, false),
	} {
		if err := r.Message(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	draft, err := db.OpenAttachmentBatch(ctx, scope, "ou_operator")
	if err != nil {
		t.Fatal(err)
	}
	// Management commands don't seal a waiting attachment draft.
	r.Command = func(context.Context, InboundCommand) error { return nil }
	if err := r.Message(ctx, attachmentEvent(t, "stop", "ou_operator", "text", map[string]string{"text": "@_user_1 /stop"}, true)); err != nil {
		t.Fatal(err)
	}
	if same, err := db.OpenAttachmentBatch(ctx, scope, "ou_operator"); err != nil || same.ID != draft.ID {
		t.Fatal("command consumed attachment draft", err)
	}
	if err := r.Message(ctx, attachmentEvent(t, "instruction", "ou_operator", "text", map[string]string{"text": "@_user_1 compare these"}, true)); err != nil {
		t.Fatal(err)
	}
	if err := r.Message(ctx, attachmentEvent(t, "later", "ou_operator", "text", map[string]string{"text": "@_user_1 another task"}, true)); err != nil {
		t.Fatal(err)
	}
	native := &appMultipartNative{serviceNative: &serviceNative{runtimeNative: &runtimeNative{turns: map[string][]codexclient.Turn{}}}}
	controller := &submissions.Controller{Store: db, Native: native, HostID: "host", AppID: "app", ThreadID: "thread"}
	if err := controller.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if native.adds.Load() != 0 {
		t.Fatal("later text overtook collecting batch")
	}
	for i := 0; i < 2; i++ {
		if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
			t.Fatal("resource not completed", err)
		}
	}
	if err := controller.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if native.adds.Load() != 2 || len(native.inputs) != 1 || g.calls != 2 {
		t.Fatal("batch/text submission count mismatch", native.adds.Load(), len(native.inputs), g.calls)
	}
	var input []map[string]any
	if err := json.Unmarshal(native.inputs[0], &input); err != nil {
		t.Fatal(err)
	}
	images, files := 0, 0
	for _, part := range input {
		if part["type"] == "localImage" {
			images++
			if _, err := os.Stat(part["path"].(string)); err != nil {
				t.Fatal(err)
			}
		}
		if text, _ := part["text"].(string); strings.Contains(text, "中文 报告.csv") {
			files++
		}
	}
	if images != 1 || files != 1 || !strings.Contains(string(native.inputs[0]), "compare these") {
		t.Fatal("multipart input lost a resource or instruction")
	}
	if err := r.Message(ctx, attachmentEvent(t, "source-image", "ou_operator", "image", map[string]string{"image_key": "img_fixture"}, false)); err != nil {
		t.Fatal(err)
	}
	if err := controller.Tick(ctx); err != nil || native.adds.Load() != 2 {
		t.Fatal("replayed source sent another input", err)
	}
}

func TestReceiverAttachmentLimitHasDurableFeedbackAndReplayDoesNotAdmit(t *testing.T) {
	r, db := receiverFixture(t)
	r.Config.Attachments.Enabled, r.Config.Attachments.ReceiveStandalone = true, true
	r.Config.Attachments.MaxResources, r.Config.Attachments.MaxImages = 1, 1
	ctx := context.Background()
	for _, id := range []string{"first", "over-limit"} {
		if err := r.Message(ctx, attachmentEvent(t, id, "ou_operator", "image", map[string]string{"image_key": "img_" + id}, false)); err != nil {
			t.Fatal(err)
		}
	}
	feedback, err := db.CommandReply(ctx, "host", "app", "over-limit")
	if err != nil || !strings.Contains(feedback.Card, "未加入草稿") {
		t.Fatal("over-limit message has no failure feedback", err)
	}
	if _, err := db.DB.Exec(`UPDATE attachment_batches SET state='cleared'`); err != nil {
		t.Fatal(err)
	}
	if err := r.Message(ctx, attachmentEvent(t, "over-limit", "ou_operator", "image", map[string]string{"image_key": "img_over-limit"}, false)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM m0_receipts WHERE message_id='over-limit'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected replay later became accepted", err)
	}
}

func TestAttachmentWorkerRecoversResultSaveFailureWithoutAnotherGET(t *testing.T) {
	r, db := receiverFixture(t)
	r.Config.Attachments.Enabled, r.Config.Attachments.ReceiveStandalone = true, true
	ctx := context.Background()
	if err := r.Message(ctx, attachmentEvent(t, "original-file", "ou_operator", "file", map[string]string{"file_key": "file_fixture", "file_name": "test.txt"}, false)); err != nil {
		t.Fatal(err)
	}
	if err := r.Message(ctx, attachmentEvent(t, "send", "ou_operator", "text", map[string]string{"text": "@_user_1 read it"}, true)); err != nil {
		t.Fatal(err)
	}
	g := &appResourceGateway{images: map[string][]byte{"original-file:file_fixture": []byte("fixture-marker\n")}}
	worker := attachmentWorker(t, r, db, g)
	if _, err := db.DB.Exec(`CREATE TRIGGER fail_ready BEFORE UPDATE ON attachment_parts WHEN NEW.state='ready' AND NEW.kind='file' BEGIN SELECT RAISE(ABORT,'fixture disk transient'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(ctx); err == nil {
		t.Fatal("ready-save failure was not observed")
	}
	if _, err := db.DB.Exec(`DROP TRIGGER fail_ready; UPDATE attachment_parts SET next_at_ms=0`); err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatal("download claim stranded after save failure", err)
	}
	if g.calls != 1 {
		t.Fatal("atomically saved bytes were downloaded again", g.calls)
	}
	if err := db.RecoverExpiredAttachmentDownloads(ctx, "host", "app", time.Now().Add(2*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	inputs, err := db.Submissions(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if err != nil || inputs[len(inputs)-1].State != "received" || !strings.Contains(inputs[len(inputs)-1].InputJSON, "test.txt") {
		t.Fatal("recovered resource not dispatchable", err)
	}
}
