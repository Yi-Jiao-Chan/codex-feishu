package submissions

import (
	"encoding/json"
	"image"
	"image/png"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	nativeBudgetImageWidth  = 1800
	nativeBudgetImageHeight = 1800
	nativeBudgetMiB         = 1024 * 1024
	nativeBudgetMaxPixels   = 40_000_000
)

// TestNativeAttachmentImageBudget is an opt-in protocol boundary probe. It
// uses only the owned local-model thread created by newNativeQueueHarness and
// keeps the model busy so the large queued input cannot be consumed.
func TestNativeAttachmentImageBudget(t *testing.T) {
	h := newNativeQueueHarness(t, true)
	first := h.root + "/budget-first.png"
	second := h.root + "/budget-second.png"
	firstBytes, err := writeNativeBudgetPNG(first, 0x6d65746164617461)
	if err != nil {
		t.Fatal("could not create first native budget image")
	}
	secondBytes, err := writeNativeBudgetPNG(second, 0x6174746163686d74)
	if err != nil {
		t.Fatal("could not create second native budget image")
	}
	totalImageBytes := firstBytes + secondBytes
	if nativeBudgetImageWidth*nativeBudgetImageHeight > nativeBudgetMaxPixels {
		t.Fatal("native budget image exceeds the configured pixel cap")
	}
	if firstBytes < 9*nativeBudgetMiB || firstBytes > 9.5*nativeBudgetMiB || secondBytes < 9*nativeBudgetMiB || secondBytes > 9.5*nativeBudgetMiB {
		t.Fatal("native budget images are outside the requested per-image range")
	}
	if totalImageBytes > 20*nativeBudgetMiB {
		t.Fatal("native budget images exceed the configured image batch cap")
	}

	input, err := json.Marshal([]map[string]any{
		{"type": "text", "text": "native image budget fixture", "text_elements": []any{}},
		{"type": "localImage", "path": first},
		{"type": "localImage", "path": second},
	})
	if err != nil {
		t.Fatal("could not encode native budget input")
	}
	clientID := "owned-budget-" + h.spec.RunID
	started := time.Now()
	queued, err := h.boundary.AddInput(h.ctx, h.thread, input, clientID)
	if err != nil {
		t.Fatal("native budget queue/add failed")
	}
	deleted := false
	deleteOwned := func() {
		if deleted || queued.ID == "" {
			return
		}
		ok, err := h.boundary.Delete(h.ctx, h.thread, queued.ID)
		if err != nil || !ok {
			t.Errorf("native budget queue cleanup failed")
			return
		}
		deleted = true
	}
	defer deleteOwned()
	assertNativeBudgetCanonicalInput(t, queued.Input)
	responseBytes := len(queued.Input)
	if responseBytes <= 20*nativeBudgetMiB || responseBytes >= 32*nativeBudgetMiB {
		t.Fatal("native budget queue/add response crossed the expected transport bounds")
	}

	state, err := h.boundary.QueueState(h.ctx, h.thread)
	if err != nil || len(state) != 1 || state[0].ID != queued.ID || state[0].ClientUserMessageID == nil || *state[0].ClientUserMessageID != clientID || len(state[0].Input) != 0 {
		t.Fatal("native budget metadata queue state did not retain only the owned identity")
	}
	full, err := h.boundary.Queue(h.ctx, h.thread)
	if err != nil {
		t.Fatal("native budget full queue read failed")
	}
	var found bool
	for _, item := range full {
		if item.ID == queued.ID {
			found = true
			assertNativeBudgetCanonicalInput(t, item.Input)
			if len(item.Input) != responseBytes {
				t.Fatal("native budget queue read changed the canonical response size")
			}
		}
	}
	if !found {
		t.Fatal("native budget queue read did not retain the owned input")
	}
	if len(h.history(t, clientID)) != 0 {
		t.Fatal("native budget input was consumed by the busy fixture model")
	}

	deleteOwned()
	remaining, err := h.boundary.Queue(h.ctx, h.thread)
	if err != nil {
		t.Fatal("native budget post-delete queue read failed")
	}
	for _, item := range remaining {
		if item.ID == queued.ID {
			t.Fatal("native budget input remained after owned delete")
		}
	}
	if len(h.history(t, clientID)) != 0 {
		t.Fatal("native budget input was consumed while deleting the queued item")
	}

	t.Logf("native image budget total_bytes=%d response_bytes=%d elapsed_ms=%d", totalImageBytes, responseBytes, time.Since(started).Milliseconds())
}

func writeNativeBudgetPNG(path string, seed int64) (int64, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return 0, err
	}
	img := image.NewNRGBA(image.Rect(0, 0, nativeBudgetImageWidth, nativeBudgetImageHeight))
	rng := rand.New(rand.NewSource(seed))
	if _, err := rng.Read(img.Pix); err != nil {
		_ = file.Close()
		return 0, err
	}
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xff
	}
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(file, img); err != nil {
		_ = file.Close()
		return 0, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return 0, err
	}
	if err := file.Close(); err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func assertNativeBudgetCanonicalInput(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var parts []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) != 3 {
		t.Fatal("native budget input did not retain three parts")
	}
	if parts[0].Type != "text" || parts[1].Type != "image" || parts[2].Type != "image" {
		t.Fatal("native budget input was not canonicalized to text plus two images")
	}
	for _, part := range parts[1:] {
		if !strings.HasPrefix(part.URL, "data:image/") {
			t.Fatal("native budget image was not canonicalized to a data URL")
		}
	}
}
