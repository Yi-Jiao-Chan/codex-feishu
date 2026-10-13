package sync

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type humanImageUploader struct {
	calls int
	key   string
	err   error
}

func (u *humanImageUploader) UploadImage(_ context.Context, _ string, _ []byte) (string, error) {
	u.calls++
	if u.err != nil {
		return "", u.err
	}
	return u.key, nil
}

func humanPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.SetRGBA(0, 0, c)
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func writeHumanPNG(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func openHumanMedia(t *testing.T, uploader ImageUploader, root string) (*HumanMedia, *store.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &HumanMedia{Store: db, Uploader: uploader, AppID: "app-test", AttachmentRoot: root}, db, dbPath
}

func humanOperationCount(t *testing.T, db *store.Store) int {
	t.Helper()
	var count int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM m0_operations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestHumanMediaUploadsNativeLocalFileURLAndDataOnceAcrossRestart(t *testing.T) {
	data := humanPNG(t, color.RGBA{R: 220, G: 40, B: 90, A: 255})
	nativePath := filepath.Join(t.TempDir(), "native.png")
	writeHumanPNG(t, nativePath, data)
	fileURL := (&url.URL{Scheme: "file", Path: nativePath}).String()
	inline := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	refs := []codexclient.AttachmentReference{
		{Kind: "image", Name: "local.png", Path: nativePath, Source: "native"},
		{Kind: "image", Name: "file-url.png", URL: fileURL, Source: "native"},
		{Kind: "image", Name: "inline.png", URL: inline, Source: "native"},
	}
	uploader := &humanImageUploader{key: "img_v3_persisted"}
	media, db, dbPath := openHumanMedia(t, uploader, "")

	first, err := media.Resolve(context.Background(), "thread", refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(refs) || humanOperationCount(t, db) != 1 || uploader.calls != 1 {
		t.Fatalf("first resolve did not journal one shared upload: views=%d operations=%d calls=%d", len(first), humanOperationCount(t, db), uploader.calls)
	}
	for n, view := range first {
		if view.ImageKey != uploader.key {
			t.Fatalf("native reference %d lost uploaded image key: %+v", n, view)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	media.Store = reopened
	second, err := media.Resolve(context.Background(), "thread", refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(refs) || uploader.calls != 1 {
		t.Fatalf("reopened resolve retried or dropped native references: views=%d calls=%d", len(second), uploader.calls)
	}
	for n, view := range second {
		if view.ImageKey != uploader.key {
			t.Fatalf("reopened native reference %d lost persisted image key: %+v", n, view)
		}
	}
}

func TestHumanMediaUnknownUploadIsRetainedAndNeverRetried(t *testing.T) {
	uploader := &humanImageUploader{err: errors.New("transport response unknown")}
	media, db, _ := openHumanMedia(t, uploader, "")
	data := humanPNG(t, color.RGBA{G: 180, A: 255})
	ref := codexclient.AttachmentReference{
		Kind: "image", Name: "uncertain.png", Source: "native",
		URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(data),
	}

	first, err := media.Resolve(context.Background(), "thread", []codexclient.AttachmentReference{ref})
	if err != nil {
		t.Fatal(err)
	}
	second, err := media.Resolve(context.Background(), "thread", []codexclient.AttachmentReference{ref})
	if err != nil {
		t.Fatal(err)
	}
	if uploader.calls != 1 || len(first) != 1 || len(second) != 1 || first[0].ImageKey != "" || second[0].ImageKey != "" {
		t.Fatalf("unknown upload was retried or exposed a false image key: calls=%d first=%+v second=%+v", uploader.calls, first, second)
	}
	var state string
	if err := db.DB.QueryRow("SELECT state FROM m0_operations").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" {
		t.Fatalf("unknown upload was not durably retained: state=%q", state)
	}
}

func TestHumanMediaBeginWriteRejectionDoesNotUploadOrJournal(t *testing.T) {
	uploader := &humanImageUploader{key: "should-not-exist"}
	media, db, _ := openHumanMedia(t, uploader, "")
	beginCalls := 0
	media.BeginWrite = func(ctx context.Context, _ string) (context.Context, func(), error) {
		beginCalls++
		return ctx, nil, errors.New("write window rejected")
	}
	data := humanPNG(t, color.RGBA{B: 210, A: 255})
	ref := codexclient.AttachmentReference{
		Kind: "image", Name: "quiet.png", Source: "native",
		URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(data),
	}

	views, err := media.Resolve(context.Background(), "thread", []codexclient.AttachmentReference{ref})
	if err != nil {
		t.Fatal(err)
	}
	if beginCalls != 1 || uploader.calls != 0 || humanOperationCount(t, db) != 0 || len(views) != 1 || views[0].ImageKey != "" {
		t.Fatalf("rejected write crossed upload/journal boundary: begin=%d uploads=%d operations=%d views=%+v", beginCalls, uploader.calls, humanOperationCount(t, db), views)
	}
}

func TestHumanMediaWrapperRootAndNativePathTrustBoundaries(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	insideData := humanPNG(t, color.RGBA{R: 220, A: 255})
	outsideData := humanPNG(t, color.RGBA{B: 220, A: 255})
	insidePath := filepath.Join(root, "inside.png")
	outsidePath := filepath.Join(outside, "outside.png")
	linkPath := filepath.Join(root, "outside-link.png")
	writeHumanPNG(t, insidePath, insideData)
	writeHumanPNG(t, outsidePath, outsideData)
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Fatal(err)
	}
	uploader := &humanImageUploader{key: "img_v3_boundary"}
	media, _, _ := openHumanMedia(t, uploader, root)
	refs := []codexclient.AttachmentReference{
		{Kind: "image", Name: "inside.png", Path: insidePath, Source: "app_wrapper"},
		{Kind: "image", Name: "outside.png", Path: outsidePath, Source: "app_wrapper"},
		{Kind: "image", Name: "link.png", Path: linkPath, Source: "app_wrapper"},
		{Kind: "image", Name: "native-outside.png", Path: outsidePath, Source: "native"},
	}

	views, err := media.Resolve(context.Background(), "thread", refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != len(refs) || uploader.calls != 2 {
		t.Fatalf("unexpected boundary handling: views=%d uploads=%d", len(views), uploader.calls)
	}
	if views[0].ImageKey == "" || views[3].ImageKey == "" {
		t.Fatalf("trusted wrapper or native path did not upload: %+v", views)
	}
	for _, n := range []int{1, 2} {
		if views[n].ImageKey != "" || views[n].Name == "" || views[n].Path == "" {
			t.Fatalf("untrusted wrapper path was read or metadata was lost: view %d=%+v", n, views[n])
		}
	}
}

func TestHumanMediaInvalidReferencesDoNotUpload(t *testing.T) {
	tmp := t.TempDir()
	tooLarge := filepath.Join(tmp, "too-large.png")
	f, err := os.OpenFile(tooLarge, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxHumanImageBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	uploader := &humanImageUploader{key: "should-not-exist"}
	media, db, _ := openHumanMedia(t, uploader, "")
	refs := []codexclient.AttachmentReference{
		{Kind: "text", Name: "text.txt", Path: filepath.Join(tmp, "does-not-exist.txt"), Source: "app_wrapper"},
		{Kind: "image", Name: "empty.png", Source: "native"},
		{Kind: "image", Name: "bad.png", URL: "data:image/png;base64,%%%", Source: "native"},
		{Kind: "image", Name: "remote.png", URL: "https://example.invalid/image.png", Source: "native"},
		{Kind: "image", Name: "large.png", Path: tooLarge, Source: "native"},
	}

	views, err := media.Resolve(context.Background(), "thread", refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != len(refs) || uploader.calls != 0 || humanOperationCount(t, db) != 0 {
		t.Fatalf("invalid references crossed the upload boundary: views=%d uploads=%d operations=%d", len(views), uploader.calls, humanOperationCount(t, db))
	}
	for n, view := range views {
		if view.Name != refs[n].Name || view.Path != refs[n].Path || view.ImageKey != "" {
			t.Fatalf("invalid reference %d lost safe metadata or gained a key: got=%+v want=%+v", n, view, refs[n])
		}
	}
}

func TestHumanMediaFileReferencesStayMetadataOnly(t *testing.T) {
	uploader := &humanImageUploader{key: "should-not-exist"}
	media, db, _ := openHumanMedia(t, uploader, "")
	ref := codexclient.AttachmentReference{
		Kind: "file", Name: "report.pdf", Path: filepath.Join(t.TempDir(), "missing.pdf"), Source: "app_wrapper",
	}

	views, err := media.Resolve(context.Background(), "thread", []codexclient.AttachmentReference{ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != ref.Name || views[0].Path != ref.Path || uploader.calls != 0 || humanOperationCount(t, db) != 0 {
		t.Fatalf("file metadata was read/uploaded or dropped: views=%+v uploads=%d operations=%d", views, uploader.calls, humanOperationCount(t, db))
	}
}

func TestHumanMediaPairsOnlyNativeAndWrapperRepresentations(t *testing.T) {
	data := humanPNG(t, color.RGBA{R: 90, G: 120, B: 220, A: 255})

	t.Run("wrapper and native become one view with wrapper metadata", func(t *testing.T) {
		root := t.TempDir()
		wrapperPath := filepath.Join(root, "copied-name.png")
		writeHumanPNG(t, wrapperPath, data)
		uploader := &humanImageUploader{key: "img_v3_pair"}
		media, _, _ := openHumanMedia(t, uploader, root)
		nativeURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
		views, err := media.Resolve(context.Background(), "thread", []codexclient.AttachmentReference{
			{Kind: "image", Name: "copied-name.png", Path: wrapperPath, Source: "app_wrapper"},
			{Kind: "image", URL: nativeURL, Source: "native"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 1 || views[0].Name != "copied-name.png" || views[0].Path != wrapperPath || views[0].ImageKey != uploader.key || uploader.calls != 1 {
			t.Fatalf("dual representation was not paired with wrapper metadata: views=%+v calls=%d", views, uploader.calls)
		}
	})

	t.Run("same-source duplicates remain visible while sharing upload", func(t *testing.T) {
		root := t.TempDir()
		firstPath := filepath.Join(root, "first.png")
		secondPath := filepath.Join(root, "second.png")
		writeHumanPNG(t, firstPath, data)
		writeHumanPNG(t, secondPath, data)
		uploader := &humanImageUploader{key: "img_v3_duplicate"}
		media, _, _ := openHumanMedia(t, uploader, root)
		views, err := media.Resolve(context.Background(), "thread", []codexclient.AttachmentReference{
			{Kind: "image", Name: "first.png", Path: firstPath, Source: "app_wrapper"},
			{Kind: "image", Name: "second.png", Path: secondPath, Source: "app_wrapper"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 2 || uploader.calls != 1 {
			t.Fatalf("same-source wrapper attachments were merged or uploaded repeatedly: views=%d calls=%d", len(views), uploader.calls)
		}
		for n, want := range []struct{ name, path string }{{"first.png", firstPath}, {"second.png", secondPath}} {
			if views[n].Name != want.name || views[n].Path != want.path || views[n].ImageKey != uploader.key {
				t.Fatalf("wrapper metadata/key lost for attachment %d: got=%+v", n, views[n])
			}
		}
	})

	t.Run("same-source native duplicates remain visible", func(t *testing.T) {
		firstPath := filepath.Join(t.TempDir(), "first.png")
		secondPath := filepath.Join(t.TempDir(), "second.png")
		writeHumanPNG(t, firstPath, data)
		writeHumanPNG(t, secondPath, data)
		uploader := &humanImageUploader{key: "img_v3_native_duplicates"}
		media, _, _ := openHumanMedia(t, uploader, "")
		views, err := media.Resolve(context.Background(), "thread", []codexclient.AttachmentReference{
			{Kind: "image", Name: "first.png", Path: firstPath, Source: "native"},
			{Kind: "image", Name: "second.png", Path: secondPath, Source: "native"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 2 || uploader.calls != 1 || views[0].ImageKey != uploader.key || views[1].ImageKey != uploader.key {
			t.Fatalf("same-source native attachments were merged or uploaded repeatedly: views=%+v calls=%d", views, uploader.calls)
		}
	})
}

func TestEngineRejectsConflictingDuplicateBeforePrepareHuman(t *testing.T) {
	e, _ := engineFixture(t)
	prepareCalls := 0
	e.PrepareHuman = func(context.Context, string, []codexclient.AttachmentReference) ([]cards.AttachmentView, error) {
		prepareCalls++
		return nil, nil
	}
	item := input("duplicate", "original")
	imageContent, err := json.Marshal(map[string]string{"type": "localImage", "path": filepath.Join(t.TempDir(), "image.png")})
	if err != nil {
		t.Fatal(err)
	}
	item.Content = append(item.Content, imageContent)
	conflict := input("duplicate", "changed")
	err = e.Apply(context.Background(), "thread", []codexclient.Turn{{
		ID: "turn", Status: "completed", Items: []codexclient.Item{item, conflict, agent("final", "answer", "final_answer")},
	}}, false, 200)
	if err == nil || prepareCalls != 0 {
		t.Fatalf("invalid duplicate snapshot reached human preparation: err=%v prepareCalls=%d", err, prepareCalls)
	}
}
