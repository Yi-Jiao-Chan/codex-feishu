package creation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	_ "golang.org/x/image/webp"
)

const creationTextPreviewBytes = 16 << 10

// AttachmentPreview contains display-only data. It never replaces the original
// resource reference used when the user confirms the creation draft.
type AttachmentPreview struct {
	Index                 int
	Kind, Name            string
	ImageKey, Text, State string
	Truncated             bool
}

type creationResourceReader interface {
	OpenMessageResource(context.Context, string, string, string) (io.ReadCloser, feishu.ResourceMetadata, error)
}

type creationImageUploader interface {
	UploadImage(context.Context, string, []byte) (string, error)
}

// prepareAttachmentPreview reads only an original message resource, never a
// local filename or URL. Confirmed display data is privately cached before return.
// Unknown uploads remain in the operation journal and are never resent.
func (m *Manager) prepareAttachmentPreview(ctx context.Context, d store.CreationDraft, index int) (AttachmentPreview, error) {
	preview := AttachmentPreview{Index: index, State: "unavailable"}
	if index < 0 || index >= len(d.Attachments) {
		return preview, nil
	}
	part := d.Attachments[index]
	preview.Kind, preview.Name = part.Kind, part.Name
	if d.State != "draft" || d.ExpiresAtMS <= m.now().UnixMilli() || !m.Config.Attachments.Enabled || d.HostID != m.Config.Host.ID || d.AppID != m.Config.Feishu.AppID || ctx.Err() != nil {
		return preview, nil
	}
	if part.Kind != "image" && (part.Kind != "file" || !creationTextAttachment(part.Name)) {
		preview.State = "unsupported"
		return preview, nil
	}
	if m.Store == nil {
		return preview, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	hash := sha256.Sum256([]byte(d.HostID + "\x00" + d.AppID + "\x00" + d.SourceMessageID + "\x00" + part.Kind + "\x00" + part.ResourceKey))
	key := fmt.Sprintf("creation-preview-v1:%s:%x", part.Kind, hash)
	if ctx.Err() != nil {
		return preview, nil
	}
	state, cached, err := m.Store.Operation(ctx, key)
	if err == nil {
		if state == "done" {
			if part.Kind == "image" && cached != "" {
				preview.State, preview.ImageKey = "ready", cached
			} else if part.Kind == "file" {
				var text struct {
					Text      string
					Truncated bool
				}
				if json.Unmarshal([]byte(cached), &text) != nil || len(text.Text) > 6<<10 || !utf8.ValidString(text.Text) || utf8.RuneCountInString(text.Text) > 1500 {
					return preview, errors.New("creation text preview cache unavailable")
				}
				preview.State, preview.Text, preview.Truncated = "ready", text.Text, text.Truncated
			}
		} else if part.Kind == "image" && (state == "sending" || state == "unknown") {
			preview.State = "unknown"
		}
		if part.Kind == "image" || state == "done" {
			return preview, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		if creationPreviewContextError(err) {
			return preview, nil
		}
		return preview, errors.New("creation preview journal unavailable")
	}
	reader, ok := m.Gateway.(creationResourceReader)
	if !ok {
		return preview, nil
	}
	if part.Kind == "file" {
		data, _, truncated, err := creationPreviewBytes(ctx, reader, d.SourceMessageID, part, creationTextPreviewBytes, true)
		if err != nil {
			return preview, nil
		}
		// The byte limit may split the final rune. Invalid UTF-8 elsewhere is
		// never repaired into a misleading preview of a binary file.
		if truncated {
			start := len(data) - 1
			for start >= 0 && !utf8.RuneStart(data[start]) {
				start--
			}
			if start >= 0 && !utf8.FullRune(data[start:]) {
				data = data[:start]
			}
		}
		if !utf8.Valid(data) || strings.IndexFunc(string(data), func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) >= 0 {
			preview.State = "unsupported"
			return preview, nil
		}
		text := string(data)
		runes := []rune(text)
		if len(runes) > 1500 {
			text, truncated = string(runes[:1500]), true
		}
		frozen, _ := json.Marshal(struct {
			Text      string
			Truncated bool
		}{text, truncated})
		if _, err := m.Store.Begin(ctx, key, "creation_text_preview"); err != nil {
			if creationPreviewContextError(err) {
				return preview, nil
			}
			return preview, errors.New("creation text preview cache unavailable")
		}
		if err := m.Store.Finish(ctx, key, "done", string(frozen)); err != nil {
			if creationPreviewContextError(err) {
				return preview, nil
			}
			return preview, errors.New("creation text preview cache unavailable")
		}
		preview.State, preview.Text, preview.Truncated = "ready", text, truncated
		return preview, nil
	}
	uploader, ok := m.Gateway.(creationImageUploader)
	if !ok || m.Store == nil {
		return preview, nil
	}
	limits := m.Config.AttachmentLimits()
	data, meta, _, err := creationPreviewBytes(ctx, reader, d.SourceMessageID, part, min(limits.MaxImageBytes, 10<<20), false)
	if err != nil {
		return preview, nil
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	declared, _, _ := mime.ParseMediaType(meta.ContentType)
	if err != nil || format != "png" && format != "jpeg" && format != "webp" || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width) > limits.MaxImagePixels/int64(cfg.Height) || cfg.Width > 12000 || cfg.Height > 12000 || declared != "" && declared != "application/octet-stream" && declared != http.DetectContentType(data) {
		return preview, nil
	}
	if ctx.Err() != nil || d.ExpiresAtMS <= m.now().UnixMilli() {
		return preview, nil
	}
	started, err := m.Store.Begin(ctx, key, "creation_image_upload")
	if err != nil {
		if creationPreviewContextError(err) {
			return preview, nil
		}
		return preview, errors.New("creation image journal unavailable")
	}
	if !started {
		preview.State = "unknown"
		return preview, nil
	}
	imageKey, uploadErr := uploader.UploadImage(ctx, "attachment."+format, data)
	state = "done"
	if uploadErr != nil || imageKey == "" {
		state, imageKey = "unknown", ""
		var rejected *feishu.Rejected
		if errors.As(uploadErr, &rejected) {
			state = "rejected"
		}
	}
	if err := m.Store.Finish(ctx, key, state, imageKey); err != nil {
		if creationPreviewContextError(err) {
			preview.State = "unknown"
			return preview, nil
		}
		return preview, errors.New("creation image confirmation unavailable")
	}
	if state == "done" {
		preview.State, preview.ImageKey = "ready", imageKey
	} else if state == "unknown" {
		preview.State = "unknown"
	}
	return preview, nil
}

func creationPreviewContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func creationTextAttachment(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".md", ".markdown", ".json", ".jsonl", ".yaml", ".yml", ".toml", ".csv", ".tsv", ".log", ".xml", ".html", ".css", ".js", ".ts", ".go", ".py", ".sh", ".sql", ".ini", ".conf", ".eml":
		return true
	}
	return false
}

func creationPreviewBytes(ctx context.Context, reader creationResourceReader, message string, part store.CreationAttachment, limit int64, prefix bool) ([]byte, feishu.ResourceMetadata, bool, error) {
	stream, meta, err := reader.OpenMessageResource(ctx, message, part.ResourceKey, part.Kind)
	if err != nil {
		return nil, meta, false, err
	}
	defer stream.Close()
	if limit < 1 || !prefix && meta.ContentLength > limit {
		return nil, meta, false, errors.New("creation preview size limit")
	}
	data, err := io.ReadAll(io.LimitReader(stream, limit+1))
	if err != nil || ctx.Err() != nil || meta.ContentLength >= 0 && (int64(len(data)) < min(meta.ContentLength, limit+1) || int64(len(data)) > meta.ContentLength) {
		return nil, meta, false, errors.New("creation preview interrupted")
	}
	truncated := int64(len(data)) > limit
	if truncated {
		if !prefix {
			return nil, meta, false, errors.New("creation preview size limit")
		}
		data = data[:limit]
	}
	return data, meta, truncated, nil
}
