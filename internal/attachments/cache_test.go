package attachments

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type resourceFixture struct {
	data   []byte
	meta   feishu.ResourceMetadata
	calls  int
	reader func() io.ReadCloser
}

func (g *resourceFixture) OpenMessageResource(_ context.Context, message, key, kind string) (io.ReadCloser, feishu.ResourceMetadata, error) {
	g.calls++
	if message != "original-message" || key != "resource-key" || kind != "image" && kind != "file" {
		return nil, g.meta, errors.New("wrong resource provenance")
	}
	if g.reader != nil {
		return g.reader(), g.meta, nil
	}
	return io.NopCloser(bytes.NewReader(g.data)), g.meta, nil
}
func cacheFixture(t *testing.T) (*Cache, store.AttachmentPart) {
	t.Helper()
	var cfg config.Config
	limits := cfg.AttachmentLimits()
	cache, err := NewCache(filepath.Join(t.TempDir(), "assets"), limits)
	if err != nil {
		t.Fatal(err)
	}
	p := store.AttachmentPart{ID: "11111111111111111111111111111111", Kind: "image", MessageID: "original-message", ResourceKey: "resource-key", Name: "../../untrusted.png", State: "downloading", Scope: store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}}
	return cache, p
}
func imageFixture(t *testing.T, format string) []byte {
	t.Helper()
	var b bytes.Buffer
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	im.Set(0, 0, color.RGBA{R: 255, A: 255})
	var err error
	switch format {
	case "png":
		err = png.Encode(&b, im)
	case "jpeg":
		err = jpeg.Encode(&b, im, nil)
	case "webp":
		data, decodeErr := base64.StdEncoding.DecodeString("UklGRhwAAABXRUJQVlA4TA8AAAAvAUAAAAcQ/Y/+ByKi/wEA")
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return data
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestCacheStaticImagesAndFilesUsePrivateAtomicPaths(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "webp", "file"} {
		t.Run(format, func(t *testing.T) {
			cache, p := cacheFixture(t)
			data, mime := []byte("random-file-marker\n"), "text/plain; charset=utf-8"
			if format != "file" {
				data, mime = imageFixture(t, format), "image/"+format
			} else {
				p.Kind = "file"
			}
			g := &resourceFixture{data: data, meta: feishu.ResourceMetadata{ContentType: mime, ContentLength: -1}}
			ready, err := cache.Download(context.Background(), g, p)
			if err != nil {
				t.Fatal(err)
			}
			path, err := cache.Resolve(ready)
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, body) || !strings.HasPrefix(path, cache.Root+"/") || strings.Contains(path, "untrusted") {
				t.Fatal("resource bytes/path changed", err)
			}
			info, _ := os.Stat(path)
			dir, _ := os.Stat(filepath.Dir(path))
			if info.Mode().Perm() != 0600 || dir.Mode().Perm() != 0700 || ready.Size != int64(len(data)) || len(ready.Digest) != 64 {
				t.Fatal("private file metadata not persisted")
			}
			// Recreate the pre-ready DB view after a publication/save crash.
			recovered, err := cache.Download(context.Background(), g, p)
			if err != nil || recovered.Digest != ready.Digest || g.calls != 1 {
				t.Fatal("published asset was not reused", err, g.calls)
			}
		})
	}
}

type interruptedReader struct{}

func (interruptedReader) Read([]byte) (int, error) { return 0, errors.New("fixture connection reset") }

func TestCacheRejectsLimitsWrongTypesAndInterruptedDownloads(t *testing.T) {
	for _, kind := range []string{"bytes", "pixels", "type", "mime", "length", "interrupted", "quota"} {
		t.Run(kind, func(t *testing.T) {
			cache, p := cacheFixture(t)
			g := &resourceFixture{data: imageFixture(t, "png"), meta: feishu.ResourceMetadata{ContentType: "image/png", ContentLength: -1}}
			want := ""
			switch kind {
			case "bytes":
				cache.Limits.MaxImageBytes = 5
				want = "size-limit"
			case "pixels":
				cache.Limits.MaxImagePixels = 3
				want = "pixel-limit"
			case "type":
				g.data = []byte("not an image")
				want = "image-type"
			case "mime":
				g.meta.ContentType = "image/jpeg"
				want = "image-type"
			case "length":
				g.meta.ContentLength = int64(len(g.data) + 1)
				want = "download-interrupted"
			case "interrupted":
				g.reader = func() io.ReadCloser {
					return io.NopCloser(io.MultiReader(bytes.NewReader(g.data[:8]), interruptedReader{}))
				}
				want = "download-interrupted"
			case "quota":
				cache.Limits.DiskQuotaBytes = 5
				want = "disk-quota"
			}
			_, err := cache.Download(context.Background(), g, p)
			var failure *cacheError
			if !errors.As(err, &failure) || failure.code != want {
				t.Fatal("wrong failure", err, want)
			}
			if used, err := cache.Usage(); err != nil || used != 0 {
				t.Fatal("failed resource leaked bytes", used, err)
			}
		})
	}
}

func TestCacheRefusesSymlinkAndDetectsChangedReadyBytes(t *testing.T) {
	cache, p := cacheFixture(t)
	g := &resourceFixture{data: imageFixture(t, "png"), meta: feishu.ResourceMetadata{ContentType: "image/png", ContentLength: -1}}
	ready, err := cache.Download(context.Background(), g, p)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := cache.path(ready)
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Resolve(ready); err == nil {
		t.Fatal("changed ready asset accepted")
	}
	submitted := ready
	submitted.State = "ready"
	if _, err := cache.Download(context.Background(), g, submitted); err == nil || g.calls != 1 {
		t.Fatal("ready asset was silently replaced", err, g.calls)
	}
	restored, err := cache.Download(context.Background(), g, ready)
	if err != nil || restored.Digest != ready.Digest || g.calls != 2 {
		t.Fatal("pre-submission retry did not restore original bytes", err, g.calls)
	}
	if _, err := cache.Resolve(restored); err != nil {
		t.Fatal("restored resource cannot be resolved", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, g.data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Download(context.Background(), g, p); err == nil {
		t.Fatal("symlink asset accepted")
	}
	got, _ := os.ReadFile(outside)
	if !bytes.Equal(got, g.data) {
		t.Fatal("outside file modified")
	}
}

func TestCacheAnimationIsNotSilentlyFlattened(t *testing.T) {
	cache, p := cacheFixture(t)
	data := imageFixture(t, "png")
	// Insert a syntactically sized acTL before IEND. DecodeConfig still reads
	// the first-frame header, but the container check must reject animation.
	anim := []byte{0, 0, 0, 8, 'a', 'c', 'T', 'L', 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0}
	data = append(append(append([]byte{}, data[:len(data)-12]...), anim...), data[len(data)-12:]...)
	g := &resourceFixture{data: data, meta: feishu.ResourceMetadata{ContentType: "image/png", ContentLength: -1}}
	if _, err := cache.Download(context.Background(), g, p); err == nil {
		t.Fatal("animated PNG accepted as static image")
	}
}
