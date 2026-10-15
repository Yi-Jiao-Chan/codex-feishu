package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

const maxHumanImageBytes = 10 << 20

type ImageUploader interface {
	UploadImage(context.Context, string, []byte) (string, error)
}

// HumanMedia prepares image keys before the pure snapshot renderer. Unknown
// uploads are durably retained and never retried as another remote mutation.
type HumanMedia struct {
	Store          *store.Store
	Uploader       ImageUploader
	AppID          string
	AttachmentRoot string
	BeginWrite     func(context.Context, string) (context.Context, func(), error)
}

func (m *HumanMedia) Resolve(ctx context.Context, thread string, refs []codexclient.AttachmentReference) ([]cards.AttachmentView, error) {
	views := attachmentPlaceholders(refs)
	keep := make([]bool, len(refs))
	for n := range keep {
		keep[n] = true
	}
	wrappers, native := map[string][]int{}, map[string][]int{}
	for n, ref := range refs {
		if ref.Kind != "image" || m == nil || m.Store == nil || m.Uploader == nil || m.AppID == "" {
			continue
		}
		data, name, err := m.readImage(ref)
		if err != nil {
			continue // Preserve the request and attachment metadata on local failure.
		}
		hash := sha256.New()
		io.WriteString(hash, m.AppID+"\x00")
		hash.Write(data)
		key := fmt.Sprintf("human-image:%x", hash.Sum(nil))
		// The App may supply the same image as both a copied attachment and
		// native image content. Pair those representations, retaining filename
		// metadata, while keeping deliberate duplicates within either source.
		if ref.Source == "native" {
			if prior := wrappers[key]; len(prior) > 0 {
				keep[n], wrappers[key] = false, prior[1:]
			} else {
				native[key] = append(native[key], n)
			}
		} else {
			if prior := native[key]; len(prior) > 0 {
				keep[prior[0]], native[key] = false, prior[1:]
			} else {
				wrappers[key] = append(wrappers[key], n)
			}
		}
		state, imageKey, err := m.Store.Operation(ctx, key)
		if err == nil {
			if state == "done" {
				views[n].ImageKey = imageKey
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("image journal unavailable")
		}
		opctx, release := ctx, func() {}
		if m.BeginWrite != nil {
			opctx, release, err = m.BeginWrite(ctx, thread)
			if err != nil {
				continue
			}
		}
		started, err := m.Store.Begin(opctx, key, "human_image_upload")
		if err != nil {
			release()
			return nil, errors.New("image journal unavailable")
		}
		if !started {
			release()
			continue
		}
		imageKey, uploadErr := m.Uploader.UploadImage(opctx, name, data)
		state = "done"
		if uploadErr != nil || imageKey == "" {
			state = "unknown"
			var rejection *feishu.Rejected
			if errors.As(uploadErr, &rejection) {
				state = "rejected"
			}
		}
		err = m.Store.Finish(opctx, key, state, imageKey)
		release()
		if err != nil {
			return nil, errors.New("image confirmation unavailable")
		}
		if state == "done" {
			views[n].ImageKey = imageKey
		}
	}
	filtered := views[:0]
	for n, view := range views {
		if keep[n] {
			filtered = append(filtered, view)
		}
	}
	return filtered, nil
}

func attachmentPlaceholders(refs []codexclient.AttachmentReference) []cards.AttachmentView {
	views := make([]cards.AttachmentView, len(refs))
	for n, ref := range refs {
		views[n] = cards.AttachmentView{Kind: ref.Kind, Name: ref.Name, Path: ref.Path}
	}
	return views
}

func (m *HumanMedia) readImage(ref codexclient.AttachmentReference) ([]byte, string, error) {
	path := ref.Path
	if path == "" && strings.HasPrefix(ref.URL, "file:") && ref.Source == "native" {
		u, err := url.Parse(ref.URL)
		if err != nil || u.Host != "" {
			return nil, "", errors.New("unsupported image reference")
		}
		path = u.Path
	}
	var data []byte
	name := "image.png"
	if path != "" {
		if !filepath.IsAbs(path) {
			return nil, "", errors.New("image path must be absolute")
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, "", errors.New("image unavailable")
		}
		if ref.Source != "native" {
			root := m.AttachmentRoot
			if root == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return nil, "", errors.New("image unavailable")
				}
				root = filepath.Join(home, ".codex", "attachments")
			}
			root, err = filepath.EvalSymlinks(root)
			rel, relErr := filepath.Rel(root, canonical)
			if err != nil || relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, "", errors.New("untrusted image reference")
			}
		}
		info, err := os.Lstat(canonical)
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxHumanImageBytes {
			return nil, "", errors.New("image unavailable or too large")
		}
		f, err := os.Open(canonical)
		if err != nil {
			return nil, "", errors.New("image unavailable")
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return nil, "", errors.New("image changed before opening")
		}
		data, err = io.ReadAll(io.LimitReader(f, maxHumanImageBytes+1))
		if err != nil {
			return nil, "", errors.New("image unavailable")
		}
		name = filepath.Base(canonical)
	} else {
		if ref.Source != "native" || !strings.HasPrefix(ref.URL, "data:image/") || len(ref.URL) > maxHumanImageBytes*4/3+1024 {
			return nil, "", errors.New("unsupported image reference")
		}
		header, encoded, ok := strings.Cut(ref.URL, ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return nil, "", errors.New("unsupported image reference")
		}
		var err error
		data, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, "", errors.New("invalid inline image")
		}
	}
	if len(data) == 0 || len(data) > maxHumanImageBytes {
		return nil, "", errors.New("image unavailable or too large")
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", errors.New("not an image")
	}
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		maxDimension := 12000
		if format == "gif" {
			maxDimension = 2000
		}
		if cfg.Width > maxDimension || cfg.Height > maxDimension {
			return nil, "", errors.New("image resolution too large")
		}
	}
	return data, name, nil
}
