package creation

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
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/attachments"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
)

type creationMultipartNative struct {
	*managerNative
	inputs []json.RawMessage
}

func (n *creationMultipartNative) AddInput(ctx context.Context, thread string, input json.RawMessage, client string) (codexclient.Queued, error) {
	n.inputs = append(n.inputs, append(json.RawMessage{}, input...))
	q, err := n.managerNative.Add(ctx, thread, "", client)
	if err == nil {
		q.Input = input
		n.queue[len(n.queue)-1] = q
	}
	return q, err
}

func (*creationMultipartNative) SteerInput(context.Context, string, string, json.RawMessage, string) error {
	return errors.New("creation must not steer")
}

type creationResourceGateway struct {
	bodies map[string][]byte
	calls  []string
}

func (g *creationResourceGateway) OpenMessageResource(_ context.Context, message, key, kind string) (io.ReadCloser, feishu.ResourceMetadata, error) {
	g.calls = append(g.calls, message+":"+key+":"+kind)
	body := g.bodies[key]
	return io.NopCloser(bytes.NewReader(body)), feishu.ResourceMetadata{ContentLength: int64(len(body))}, nil
}

func creationAttachmentFixture(t *testing.T, h *managerHarness) (*creationMultipartNative, *attachments.Worker, *creationResourceGateway) {
	t.Helper()
	h.m.Config.Attachments.Enabled = true
	n := &creationMultipartNative{managerNative: h.n}
	h.m.Native = func() Native { return n }
	limits := h.m.Config.AttachmentLimits()
	cache, err := attachments.NewCache(filepath.Join(t.TempDir(), "cache"), limits)
	if err != nil {
		t.Fatal(err)
	}
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	g := &creationResourceGateway{bodies: map[string][]byte{"img_fixture": picture.Bytes(), "file_fixture": []byte("FIXTURE,42\n")}}
	worker := &attachments.Worker{Store: h.s, Cache: cache, Gateway: g, HostID: "host", AppID: "app"}
	return n, worker, g
}

func receiveCreationAttachments(t *testing.T, h *managerHarness) store.CreationDraft {
	t.Helper()
	handled, err := h.m.Receive(context.Background(), Inbound{
		MessageID: "source-resources", ChatID: "oc_private", ChatType: "p2p", UserID: "ou_creator", Text: "/new compare materials",
		Parts: []feishu.InboundPart{{Type: "text", Text: "/new compare materials"}, {Type: "image", ResourceKey: "img_fixture"}, {Type: "file", ResourceKey: "file_fixture", Name: "测试 \"报告\".csv"}},
	})
	if err != nil || !handled {
		t.Fatal("attachment creation not admitted", handled, err)
	}
	h.tick()
	return h.draft("source-resources")
}

func TestCreationAttachmentsWaitForCompleteInputAcrossRestart(t *testing.T) {
	for _, topic := range []bool{false, true} {
		t.Run(map[bool]string{false: "new group", true: "workgroup topic"}[topic], func(t *testing.T) {
			h := newManagerHarness(t)
			n, worker, gateway := creationAttachmentFixture(t, h)
			ctx := context.Background()
			if topic {
				h.m.Config.Creation.TopicsEnabled = true
				h.m.Config.Creation.WorkgroupChatIDs = []string{"oc_work"}
				h.g.workgroups = map[string]string{"oc_work": "workgroup"}
				h.g.readback = true
			}
			d := receiveCreationAttachments(t, h)
			card := h.g.cards[d.CardID].Content
			if !strings.Contains(card, "测试") || !strings.Contains(card, "报告") || !strings.Contains(card, "creation_remove_attachment") || len(gateway.calls) != 0 {
				t.Fatal("draft lost its attachment rows or downloaded before confirmation")
			}
			form := localCreationForm(map[string]any{"creation_body": "edited task"})
			if topic {
				form["creation_mode"], form["creation_target"] = "topic", "oc_work"
			}
			if err := h.action(d, "creation_submit", form); err != nil {
				t.Fatal(err)
			}
			h.tick()
			i := h.intent(d)
			h.m.Config.Attachments.Enabled = false // Confirmed creation must still finish.
			if pending, err := h.s.HasCollectingAttachments(ctx, "host", "app"); err != nil || !pending {
				t.Fatal("disabled attachment feature lost the pending creation", err)
			}
			scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: i.NativeThreadID}
			receipt, err := h.s.Submission(ctx, scope, i.ReceiptID)
			if err != nil || receipt.State != "collecting" || receipt.InputJSON != "" || len(n.inputs) != 0 || len(h.activated) != 0 {
				t.Fatalf("creation not collecting: receipt=%s intent=%s error=%s inputs=%d active=%d err=%v", receipt.State, i.State, h.draft(d.SourceMessageID).ErrorCode, len(n.inputs), len(h.activated), err)
			}
			controller := &submissions.Controller{Store: h.s, Native: n, HostID: "host", AppID: "app", ThreadID: i.NativeThreadID}
			reads := h.n.reads
			if err := controller.SubmitInitial(ctx, receipt.ID); err != nil || h.n.reads != reads || len(n.inputs) != 0 {
				t.Fatal("collecting initial input queried an unpersisted rollout", err)
			}
			if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
				t.Fatal("image download failed", worked, err)
			}
			h.tick()
			if len(n.inputs) != 0 {
				t.Fatal("initial input omitted pending file")
			}
			// Reopen the real database while one resource is already cached.
			h.restart()
			worker.Store = h.s
			if err := h.s.RecoverAttachmentDownloads(ctx, "host", "app"); err != nil {
				t.Fatal(err)
			}
			if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
				t.Fatal("file download did not resume", worked, err)
			}
			h.tick()
			receiptAfter, err := h.s.Submission(ctx, scope, receipt.ID)
			if err != nil || receiptAfter.ClientID != receipt.ClientID || len(n.inputs) != 1 || len(h.n.starts) != 1 || h.g.roots != 1 || len(h.activated) != 1 || h.draft(d.SourceMessageID).State != "created" {
				t.Fatalf("collection restart changed input: state=%s intent=%s error=%s inputs=%d starts=%d roots=%d active=%d err=%v", receiptAfter.State, h.intent(d).State, h.draft(d.SourceMessageID).ErrorCode, len(n.inputs), len(h.n.starts), h.g.roots, len(h.activated), err)
			}
			if len(gateway.calls) != 2 || gateway.calls[0] != "source-resources:img_fixture:image" || gateway.calls[1] != "source-resources:file_fixture:file" {
				t.Fatal("resource GET lost the original source/order", gateway.calls)
			}
			var parts []struct {
				Type, Text, Path string
			}
			if err := json.Unmarshal(n.inputs[0], &parts); err != nil || len(parts) != 3 || parts[0].Text != "edited task" || parts[1].Type != "localImage" || parts[2].Type != "text" {
				t.Fatal("native input lost edited text or resource order", err)
			}
			if _, err := os.Stat(parts[1].Path); err != nil {
				t.Fatal("native image path is unavailable", err)
			}
			refs, err := h.s.AttachmentCacheReferences(ctx, "host", "app")
			if err != nil || len(refs) != 3 {
				t.Fatal("resource references not retained", err)
			}
			for _, ref := range refs {
				if ref.Kind == "file" {
					path := filepath.Join(worker.Cache.Root, ref.FileID)
					name, _ := json.Marshal("测试 \"报告\".csv")
					quotedPath, _ := json.Marshal(path)
					if parts[2].Text != string(name)+": "+string(quotedPath) {
						t.Fatal("native file reference is incomplete or unescaped")
					}
					if body, err := os.ReadFile(path); err != nil || string(body) != "FIXTURE,42\n" {
						t.Fatal("original file bytes were not preserved", err)
					}
				}
			}
			active := &submissions.Controller{Store: h.s, Native: n, HostID: "host", AppID: "app", ThreadID: i.NativeThreadID}
			if err := active.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if err := h.s.StageQueueCard(ctx, scope, receipt.ID); err != nil {
				t.Fatal(err)
			}
			mirror, err := h.s.SubmissionCard(ctx, scope, receipt.ID)
			if err != nil || mirror.ParentMessageID != i.RootMessageID || mirror.ParentMessageID == d.SourceMessageID {
				t.Fatal("queue card replied across chats instead of to the new root", err)
			}
			h.m.Config.Attachments.Enabled = true
			receiveCreationAttachments(t, h)
			h.tick()
			if len(n.inputs) != 1 || len(h.n.starts) != 1 || h.g.roots != 1 || !reflect.DeepEqual(h.draft(d.SourceMessageID).Attachments, d.Attachments) {
				t.Fatal("replayed creation duplicated or changed the original batch")
			}
		})
	}
}

func TestCreationAttachmentsUnconfirmedDraftsRespectFeatureAndCancellation(t *testing.T) {
	for _, action := range []string{"disabled", "canceled", "expired"} {
		t.Run(action, func(t *testing.T) {
			h := newManagerHarness(t)
			_, worker, _ := creationAttachmentFixture(t, h)
			d := receiveCreationAttachments(t, h)
			switch action {
			case "disabled":
				h.m.Config.Attachments.Enabled = false
				h.submitLocal(d)
				if got := h.draft(d.SourceMessageID); got.State != "draft" || got.ErrorCode != "attachments-disabled" || !reflect.DeepEqual(got.Attachments, d.Attachments) {
					t.Fatal("disabled feature discarded attachments or created a task")
				}
			case "canceled":
				if err := h.action(d, "creation_cancel", nil); err != nil {
					t.Fatal(err)
				}
			case "expired":
				h.clock = h.clock.Add(h.m.Config.CreationTTL())
			}
			h.tick()
			if worked, err := worker.ProcessOne(context.Background()); err != nil || worked || len(h.n.starts) != 0 || h.g.groups != 0 || h.g.roots != 0 {
				t.Fatal("unconfirmed creation downloaded or created remote resources", err)
			}
		})
	}
}

func TestCreationAttachmentFailureRetriesSameResourcesAndNativeUnknownDoesNotResend(t *testing.T) {
	h := newManagerHarness(t)
	n, worker, gateway := creationAttachmentFixture(t, h)
	ctx := context.Background()
	d := receiveCreationAttachments(t, h)
	h.submitLocal(d)
	i := h.intent(d)
	good := gateway.bodies["img_fixture"]
	gateway.bodies["img_fixture"] = []byte("not an image")
	if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
		t.Fatal("invalid image result was not saved", err)
	}
	h.tick()
	if h.intent(d).State != "rejected" || h.draft(d.SourceMessageID).ErrorCode != "input-attachments" || len(n.inputs) != 0 {
		t.Fatalf("invalid resource state: intent=%s error=%s inputs=%d", h.intent(d).State, h.draft(d.SourceMessageID).ErrorCode, len(n.inputs))
	}
	if pending, err := h.s.HasCollectingAttachments(ctx, "host", "app"); err != nil || !pending {
		t.Fatal("failed creation would lose its downloader after restart with intake disabled", err)
	}
	h.m.Config.Attachments.Enabled = false
	h.restart()
	worker.Store = h.s
	gateway.bodies["img_fixture"] = good
	d = h.draft(d.SourceMessageID)
	if err := h.action(d, "creation_retry", nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	for j := 0; j < 2; j++ {
		if worked, err := worker.ProcessOne(ctx); err != nil || !worked {
			t.Fatal("explicit retry did not finish the original resources", j, err)
		}
	}
	h.n.addErrors = []error{&codexclient.UnknownError{Method: "thread/queue/add", Cause: errors.New("response lost")}}
	h.tick()
	if h.intent(d).State != "unknown" || len(n.inputs) != 1 {
		t.Fatal("unknown first input was not retained")
	}
	h.restart()
	h.tick()
	if err := h.s.RetryCreation(ctx, h.draft(d.SourceMessageID)); !errors.Is(err, store.ErrSubmissionStale) {
		t.Fatal("unknown first input allowed retry", err)
	}
	if len(n.inputs) != 1 || len(h.n.starts) != 1 || h.g.groups != 1 || h.g.roots != 1 || h.intent(d).ReceiptID != i.ReceiptID {
		t.Fatal("recovery duplicated initial input or replaced its resources")
	}
	client := h.n.adds[0].client
	h.n.queue = []codexclient.Queued{{ID: "confirmed-original", ClientUserMessageID: &client, Input: n.inputs[0]}}
	h.tick()
	if h.draft(d.SourceMessageID).State != "created" || len(n.inputs) != 1 {
		t.Fatal("queue evidence did not reconcile the original multipart input")
	}
}
