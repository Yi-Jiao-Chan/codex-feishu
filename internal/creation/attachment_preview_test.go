package creation

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type creationPreviewGateway struct {
	*managerGateway
	body               []byte
	contentType        string
	length             *int64
	getErr             error
	uploadErr          error
	gets, uploads      int
	message, key, kind string
	beforeUpload       func()
	duringUpload       func(context.Context)
}

func (g *creationPreviewGateway) OpenMessageResource(ctx context.Context, message, key, kind string) (io.ReadCloser, feishu.ResourceMetadata, error) {
	g.gets++
	g.message, g.key, g.kind = message, key, kind
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 2*time.Second {
		g.h.t.Fatal("preview resource lacks bounded deadline")
	}
	length := int64(len(g.body))
	if g.length != nil {
		length = *g.length
	}
	return io.NopCloser(bytes.NewReader(g.body)), feishu.ResourceMetadata{ContentLength: length, ContentType: g.contentType}, g.getErr
}

func (g *creationPreviewGateway) UploadImage(ctx context.Context, name string, data []byte) (string, error) {
	g.uploads++
	if g.beforeUpload != nil {
		g.beforeUpload()
	}
	if g.duringUpload != nil {
		g.duringUpload(ctx)
	}
	if name != "attachment.png" || !bytes.Equal(data, g.body) {
		g.h.t.Fatal("upload did not use validated bytes and a fixed filename")
	}
	if g.uploadErr != nil {
		return "", g.uploadErr
	}
	return "img_preview_confirmed", nil
}

func previewFixture(t *testing.T, part store.CreationAttachment) (*managerHarness, *creationPreviewGateway, store.CreationDraft) {
	t.Helper()
	h := newManagerHarness(t)
	h.m.Config.Attachments.Enabled = true
	g := &creationPreviewGateway{managerGateway: h.g}
	h.m.Gateway = g
	d := store.CreationDraft{ID: "draft", HostID: "host", AppID: "app", CreatorID: "ou_creator", SourceMessageID: "original_message", State: "draft", ExpiresAtMS: h.clock.Add(time.Hour).UnixMilli(), Attachments: []store.CreationAttachment{part}}
	return h, g, d
}

func previewPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestCreationPreviewImageJournalsBeforeUploadAndReusesAfterReopen(t *testing.T) {
	h, g, d := previewFixture(t, store.CreationAttachment{Kind: "image", ResourceKey: "inbound_user_key"})
	g.body, g.contentType = previewPNG(t), "image/png"
	g.beforeUpload = func() {
		var state, kind string
		if err := h.s.DB.QueryRow(`SELECT state,kind FROM m0_operations WHERE kind='creation_image_upload'`).Scan(&state, &kind); err != nil || state != "sending" {
			t.Fatal("upload lacked durable journal", state, kind, err)
		}
	}
	view, err := h.m.prepareAttachmentPreview(context.Background(), d, 0)
	if err != nil || view.State != "ready" || view.ImageKey != "img_preview_confirmed" || view.ImageKey == d.Attachments[0].ResourceKey || g.message != d.SourceMessageID || g.kind != "image" || g.key != d.Attachments[0].ResourceKey {
		t.Fatal("image preview did not use original GET and confirmed upload", view, err)
	}
	if err := h.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(h.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	h.m.Store = reopened
	again, err := h.m.prepareAttachmentPreview(context.Background(), d, 0)
	if err != nil || again != view || g.gets != 1 || g.uploads != 1 {
		t.Fatal("reopened preview repeated GET/upload", again, err, g.gets, g.uploads)
	}
}

func TestCreationPreviewUnknownRejectedAndUnfinishedUploadsNeverRepeat(t *testing.T) {
	for _, uploadErr := range []error{errors.New("private transport failure"), &feishu.Rejected{Code: 234001}, nil} {
		t.Run(map[bool]string{true: "known", false: "unknown"}[uploadErr == nil], func(t *testing.T) {
			h, g, d := previewFixture(t, store.CreationAttachment{Kind: "image", ResourceKey: "inbound_user_key"})
			g.body, g.uploadErr = previewPNG(t), uploadErr
			if uploadErr == nil {
				g.beforeUpload = func() {
					h.s.DB.Exec(`CREATE TRIGGER preview_finish_failure BEFORE UPDATE ON m0_operations BEGIN SELECT RAISE(FAIL,'fixture'); END`)
				}
			}
			first, err := h.m.prepareAttachmentPreview(context.Background(), d, 0)
			if uploadErr == nil && err == nil {
				t.Fatal("finish failure was hidden")
			}
			second, err := h.m.prepareAttachmentPreview(context.Background(), d, 0)
			if err != nil || first.ImageKey != "" || second.ImageKey != "" || g.gets != 1 || g.uploads != 1 {
				t.Fatal("uncertain/rejected upload was repeated", first, second, err, g.gets, g.uploads)
			}
		})
	}
}

func TestCreationPreviewTextIsBoundedUTF8CachedAndNeverReadsItsFilename(t *testing.T) {
	h, g, d := previewFixture(t, store.CreationAttachment{Kind: "file", ResourceKey: "file_fixture", Name: "/private/no-such-file.txt"})
	g.body = []byte(strings.Repeat("合成文本", 6000))
	view, err := h.m.prepareAttachmentPreview(context.Background(), d, 0)
	if err != nil || view.State != "ready" || !view.Truncated || !utf8.ValidString(view.Text) || utf8.RuneCountInString(view.Text) != 1500 || len(view.Text) > 6<<10 || g.uploads != 0 {
		t.Fatal("text limits or trust boundary failed", view.State, len(view.Text), err)
	}
	g.body = []byte("changed")
	again, err := h.m.prepareAttachmentPreview(context.Background(), d, 0)
	if err != nil || again != view || g.gets != 1 {
		t.Fatal("text preview did not reuse private cache", again, err, g.gets)
	}
}

func TestCreationPreviewCanceledContextKeepsAttachmentLabel(t *testing.T) {
	for _, kind := range []string{"file", "image"} {
		t.Run(kind, func(t *testing.T) {
			h, g, d := previewFixture(t, store.CreationAttachment{Kind: kind, ResourceKey: "resource_fixture", Name: "原附件.txt"})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			// The cancelled budget must be handled before consulting the store.
			h.s.Close()
			view, err := h.m.prepareAttachmentPreview(ctx, d, 0)
			if err != nil || view.State != "unavailable" || view.Name != d.Attachments[0].Name || view.Kind != kind || g.gets != 0 || g.uploads != 0 {
				t.Fatal("cancelled preview lost its label or performed work", view, err, g.gets, g.uploads)
			}
		})
	}
}

func TestCreationPreviewUploadContextExpiryKeepsSendingJournal(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "deadline"}[deadline], func(t *testing.T) {
			h, g, d := previewFixture(t, store.CreationAttachment{Kind: "image", ResourceKey: "resource_fixture", Name: "原图片.png"})
			g.body = previewPNG(t)
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			}
			defer cancel()
			g.duringUpload = func(uploadCtx context.Context) {
				if !deadline {
					cancel()
				}
				<-uploadCtx.Done()
			}
			view, err := h.m.prepareAttachmentPreview(ctx, d, 0)
			if err != nil || view.State != "unknown" || view.ImageKey != "" || view.Name != d.Attachments[0].Name || view.Kind != "image" {
				t.Fatal("expired upload interrupted the card or exposed an unconfirmed key", view, err)
			}
			var state string
			if err := h.s.DB.QueryRow(`SELECT state FROM m0_operations WHERE kind='creation_image_upload'`).Scan(&state); err != nil || state != "sending" {
				t.Fatal("expired confirmation lost its sending journal", state, err)
			}
			again, err := h.m.prepareAttachmentPreview(context.Background(), d, 0)
			if err != nil || again.State != "unknown" || g.gets != 1 || g.uploads != 1 {
				t.Fatal("expired upload was repeated", again, err, g.gets, g.uploads)
			}
		})
	}
}

func TestCreationPreviewUnsupportedBinaryAndInvalidImageDoNotUpload(t *testing.T) {
	for _, tc := range []struct {
		name, kind, file, body, mime string
		wantGets                     int
	}{
		{"binary extension", "file", "fixture.pdf", "binary", "", 0},
		{"binary bytes", "file", "fixture.txt", "text\x00binary", "", 1},
		{"invalid utf8", "file", "fixture.txt", "text\xffbad", "", 1},
		{"invalid utf8 at prefix boundary", "file", "fixture.txt", strings.Repeat("a", creationTextPreviewBytes-1) + "\xffmore", "", 1},
		{"wrong image", "image", "fixture.png", "not an image", "image/png", 1},
		{"wrong mime", "image", "fixture.png", "", "text/plain", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, g, d := previewFixture(t, store.CreationAttachment{Kind: tc.kind, ResourceKey: "resource_fixture", Name: tc.file})
			g.body, g.contentType = []byte(tc.body), tc.mime
			if tc.name == "wrong mime" {
				g.body = previewPNG(t)
			}
			view, err := h.m.prepareAttachmentPreview(context.Background(), d, 0)
			if err != nil || view.State == "ready" || view.ImageKey != "" || view.Text != "" || g.gets != tc.wantGets || g.uploads != 0 {
				t.Fatal("unsupported preview crossed resource boundary", view, err, g.gets, g.uploads)
			}
		})
	}
}

func TestCreationPreviewLimitsInactiveDraftsAndFailures(t *testing.T) {
	t.Run("unknown and inconsistent content length", func(t *testing.T) {
		for _, length := range []int64{-1, 1, 1000} {
			h, g, d := previewFixture(t, store.CreationAttachment{Kind: "image", ResourceKey: "resource_fixture"})
			g.body, g.length = previewPNG(t), &length
			view, err := h.m.prepareAttachmentPreview(context.Background(), d, 0)
			if err != nil || (view.ImageKey != "") != (length == -1) || (g.uploads == 1) != (length == -1) {
				t.Fatal("resource length was not checked", length, view, err, g.uploads)
			}
		}
	})
	t.Run("expired disabled and foreign drafts", func(t *testing.T) {
		for _, change := range []func(*managerHarness, *store.CreationDraft){
			func(h *managerHarness, d *store.CreationDraft) { d.ExpiresAtMS = h.clock.UnixMilli() },
			func(h *managerHarness, _ *store.CreationDraft) { h.m.Config.Attachments.Enabled = false },
			func(_ *managerHarness, d *store.CreationDraft) { d.AppID = "foreign_app" },
			func(_ *managerHarness, d *store.CreationDraft) { d.HostID = "foreign_host" },
		} {
			h, g, d := previewFixture(t, store.CreationAttachment{Kind: "file", ResourceKey: "resource_fixture", Name: "test.txt"})
			change(h, &d)
			h.s.Close()
			if view, err := h.m.prepareAttachmentPreview(context.Background(), d, 0); err != nil || view.State == "ready" || view.Name != "test.txt" || view.Kind != "file" || g.gets != 0 || g.uploads != 0 {
				t.Fatal(view, err, g.gets)
			}
		}
	})
	t.Run("size and pixel limits", func(t *testing.T) {
		h, g, d := previewFixture(t, store.CreationAttachment{Kind: "image", ResourceKey: "resource_fixture"})
		g.body = previewPNG(t)
		h.m.Config.Attachments.MaxImageBytes = 1
		if view, err := h.m.prepareAttachmentPreview(context.Background(), d, 0); err != nil || view.ImageKey != "" || g.uploads != 0 {
			t.Fatal(view, err)
		}
		h.m.Config.Attachments.MaxImageBytes = 10 << 20
		h.m.Config.Attachments.MaxImagePixels = 1
		if view, err := h.m.prepareAttachmentPreview(context.Background(), d, 0); err != nil || view.ImageKey != "" || g.uploads != 0 {
			t.Fatal(view, err)
		}
	})
	for _, state := range []string{"canceled", "expired", "creating", "created"} {
		t.Run(state, func(t *testing.T) {
			h, g, d := previewFixture(t, store.CreationAttachment{Kind: "file", ResourceKey: "resource_fixture", Name: "test.txt"})
			d.State = state
			if view, err := h.m.prepareAttachmentPreview(context.Background(), d, 0); err != nil || view.State == "ready" || g.gets != 0 {
				t.Fatal(view, err, g.gets)
			}
		})
	}
	t.Run("get failure is hidden", func(t *testing.T) {
		h, g, d := previewFixture(t, store.CreationAttachment{Kind: "file", ResourceKey: "resource_fixture", Name: "test.txt"})
		g.getErr = errors.New("private error detail")
		if view, err := h.m.prepareAttachmentPreview(context.Background(), d, 0); err != nil || view.State != "unavailable" || view.Text != "" {
			t.Fatal(view, err)
		}
	})
	t.Run("database failure is reported safely", func(t *testing.T) {
		h, g, d := previewFixture(t, store.CreationAttachment{Kind: "file", ResourceKey: "resource_fixture", Name: "test.txt"})
		h.s.Close()
		if _, err := h.m.prepareAttachmentPreview(context.Background(), d, 0); err == nil || strings.Contains(err.Error(), "sql") || g.gets != 0 {
			t.Fatal(err, g.gets)
		}
	})
}
