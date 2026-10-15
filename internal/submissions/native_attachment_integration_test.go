package submissions

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"codex-feishu/internal/codexclient"
)

// These methods deliberately use the same ownership, counters and accepted
// reply-loss boundary as the legacy test-only methods. The native client still
// performs every RPC against the explicitly authorized fixture thread.
func (n *nativeQueueBoundary) AddInput(ctx context.Context, thread string, input json.RawMessage, client string) (codexclient.Queued, error) {
	if err := n.check(thread); err != nil {
		return codexclient.Queued{}, err
	}
	n.adds++
	q, err := n.client.AddInput(ctx, thread, input, client)
	if err == nil && n.discardAdd {
		n.discardAdd = false
		return codexclient.Queued{}, &codexclient.UnknownError{Method: "thread/queue/add", Cause: errors.New("accepted reply discarded at Controller boundary")}
	}
	return q, err
}

func (n *nativeQueueBoundary) SteerInput(ctx context.Context, thread, turn string, input json.RawMessage, client string) error {
	if err := n.check(thread); err != nil {
		return err
	}
	n.steers++
	n.steerTargets = append(n.steerTargets, turn)
	err := n.client.SteerInput(ctx, thread, turn, input, client)
	if err == nil && n.discardSteer {
		n.discardSteer = false
		return &codexclient.UnknownError{Method: "turn/steer", Cause: errors.New("accepted reply discarded at Controller boundary")}
	}
	return err
}

func (n *nativeQueueBoundary) QueueState(ctx context.Context, thread string) ([]codexclient.Queued, error) {
	if err := n.check(thread); err != nil {
		return nil, err
	}
	return n.client.QueueState(ctx, thread)
}

type nativeAttachmentFixture struct {
	input       json.RawMessage
	firstImage  string
	secondImage string
	texts       []string
}

func newNativeAttachmentFixture(t *testing.T, h *nativeQueueHarness) nativeAttachmentFixture {
	t.Helper()
	firstImage := filepath.Join(h.root, "fixture-first.png")
	secondImage := filepath.Join(h.root, "fixture-second.png")
	file := filepath.Join(h.root, "fixture-reference.txt")
	if err := writeNativeAttachmentPNG(firstImage, color.NRGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xff}); err != nil {
		t.Fatal("cannot create first native image fixture")
	}
	if err := writeNativeAttachmentPNG(secondImage, color.NRGBA{R: 0xab, G: 0xcd, B: 0xef, A: 0xff}); err != nil {
		t.Fatal("cannot create second native image fixture")
	}
	marker := "native-attachment-fixture-" + h.spec.RunID
	if err := os.WriteFile(file, []byte(marker+"\n"), 0600); err != nil {
		t.Fatal("cannot create native text file fixture")
	}
	texts := []string{
		"native multipart fixture request",
		"fixture text file reference: " + file + " marker=" + marker,
	}
	parts := []map[string]any{
		{"type": "text", "text": texts[0], "text_elements": []any{}},
		{"type": "localImage", "path": firstImage},
		{"type": "text", "text": texts[1], "text_elements": []any{}},
		{"type": "localImage", "path": secondImage},
	}
	input, err := json.Marshal(parts)
	if err != nil {
		t.Fatal("cannot encode native multipart fixture")
	}
	return nativeAttachmentFixture{input: input, firstImage: firstImage, secondImage: secondImage, texts: texts}
}

func writeNativeAttachmentPNG(path string, fill color.NRGBA) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.SetNRGBA(0, 0, fill)
	err = png.Encode(f, img)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	return errors.Join(err, closeErr)
}

type nativeObservedPart struct {
	kind string
	text string
	path string
}

func decodeNativeParts(raw json.RawMessage) ([]nativeObservedPart, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return nil, errors.New("native input is not a nonempty array")
	}
	return decodeNativePartItems(items)
}

func decodeNativePartItems(items []json.RawMessage) ([]nativeObservedPart, error) {
	parts := make([]nativeObservedPart, 0, len(items))
	for _, raw := range items {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil || item == nil {
			return nil, errors.New("native input item is not an object")
		}
		var kind string
		if err := json.Unmarshal(item["type"], &kind); err != nil {
			return nil, errors.New("native input item type unavailable")
		}
		part := nativeObservedPart{kind: kind}
		switch kind {
		case "text":
			if err := json.Unmarshal(item["text"], &part.text); err != nil {
				return nil, errors.New("native text item unavailable")
			}
		case "localImage":
			if err := json.Unmarshal(item["path"], &part.path); err != nil {
				return nil, errors.New("native local image item unavailable")
			}
		case "image":
			// The App may canonicalize localImage into image in turn history.
			if url, ok := item["url"]; ok {
				_ = json.Unmarshal(url, &part.path)
			}
		default:
			return nil, errors.New("unexpected native input item type")
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func assertNativeMultipartParts(t *testing.T, raw json.RawMessage, fixture nativeAttachmentFixture, history bool) {
	t.Helper()
	parts, err := decodeNativeParts(raw)
	if err != nil {
		t.Fatal("native queue input could not be decoded")
	}
	assertNativeMultipartPartItems(t, parts, fixture, history)
}

func assertNativeMultipartHistory(t *testing.T, item codexclient.Item, fixture nativeAttachmentFixture) {
	t.Helper()
	parts, err := decodeNativePartItems(item.Content)
	if err != nil {
		t.Fatal("native history input could not be decoded")
	}
	assertNativeMultipartPartItems(t, parts, fixture, true)
}

func assertNativeMultipartPartItems(t *testing.T, parts []nativeObservedPart, fixture nativeAttachmentFixture, history bool) {
	t.Helper()
	if len(parts) != 4 || parts[0].kind != "text" || parts[2].kind != "text" || parts[0].text != fixture.texts[0] || parts[2].text != fixture.texts[1] {
		t.Fatal("native multipart text or part count changed")
	}
	if history {
		if (parts[1].kind != "localImage" && parts[1].kind != "image") || (parts[3].kind != "localImage" && parts[3].kind != "image") {
			t.Fatalf("native history did not retain both image parts: types=%v", nativePartKinds(parts))
		}
		return
	}
	if (parts[1].kind != "localImage" && parts[1].kind != "image") || (parts[3].kind != "localImage" && parts[3].kind != "image") || parts[1].kind == "localImage" && parts[1].path != fixture.firstImage || parts[3].kind == "localImage" && parts[3].path != fixture.secondImage {
		t.Fatalf("native queue did not retain both local image paths: types=%v path_fields=%v", nativePartKinds(parts), nativePartPathFlags(parts))
	}
}

func nativePartKinds(parts []nativeObservedPart) []string {
	kinds := make([]string, 0, len(parts))
	for _, part := range parts {
		kinds = append(kinds, part.kind)
	}
	return kinds
}

func nativePartPathFlags(parts []nativeObservedPart) []bool {
	flags := make([]bool, 0, len(parts))
	for _, part := range parts {
		flags = append(flags, part.path != "")
	}
	return flags
}

func nativeQueuedInput(t *testing.T, h *nativeQueueHarness, clientID string) codexclient.Queued {
	t.Helper()
	queue, err := h.other.Queue(h.ctx, h.thread)
	if err != nil {
		t.Fatal("second native client could not read the owned queue")
	}
	for _, item := range queue {
		if item.ClientUserMessageID != nil && *item.ClientUserMessageID == clientID {
			return item
		}
	}
	t.Fatal("multipart input missing from the owned native queue")
	return codexclient.Queued{}
}

func nativeHistoryInput(t *testing.T, h *nativeQueueHarness, clientID string) codexclient.Item {
	t.Helper()
	turns, err := h.other.Turns(h.ctx, h.thread)
	if err != nil {
		t.Fatal("second native client could not read owned history")
	}
	for _, turn := range turns {
		for _, item := range turn.Items {
			if item.Type == "userMessage" && item.ClientID != nil && *item.ClientID == clientID {
				return item
			}
		}
	}
	t.Fatal("multipart input missing from owned native history")
	return codexclient.Item{}
}

func TestNativeControllerAttachments(t *testing.T) {
	for _, test := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{name: "queue", fn: testNativeAttachmentQueue},
		{name: "steer", fn: testNativeAttachmentSteer},
		{name: "restore", fn: testNativeAttachmentRestore},
		{name: "unknown_add", fn: testNativeAttachmentUnknownAdd},
	} {
		t.Run(test.name, test.fn)
	}
}

func testNativeAttachmentQueue(t *testing.T) {
	h := newNativeQueueHarness(t, true)
	fixture := newNativeAttachmentFixture(t, h)
	receipt := h.claim(t)
	setStoredInput(t, h.controller, receipt.ID, string(fixture.input))
	h.tick(t)
	queued := nativeQueuedInput(t, h, receipt.ClientID)
	assertNativeMultipartParts(t, queued.Input, fixture, false)
	if h.boundary.adds != 1 || h.boundary.steers != 0 {
		t.Fatal("multipart queue dispatch count changed")
	}
	h.reopen(t)
	h.tick(t)
	if h.boundary.adds != 1 {
		t.Fatal("queue recovery resent multipart add")
	}
	nativeQueueDrain(t, h, 2)
	assertNativeMultipartHistory(t, nativeHistoryInput(t, h, receipt.ClientID), fixture)
}

func testNativeAttachmentSteer(t *testing.T) {
	h := newNativeQueueHarness(t, true)
	fixture := newNativeAttachmentFixture(t, h)
	receipt := h.claim(t)
	setStoredInput(t, h.controller, receipt.ID, string(fixture.input))
	h.tick(t)
	assertNativeMultipartParts(t, nativeQueuedInput(t, h, receipt.ClientID).Input, fixture, false)
	action := h.steerAction(t, receipt.ID)
	h.tick(t)
	result, err := h.db.SubmissionAction(h.ctx, h.controller.scope(), action.ID)
	if err != nil || result.State != "confirmed" || result.Result != "steered" || h.boundary.adds != 1 || h.boundary.deletes != 1 || h.boundary.steers != 1 || len(h.boundary.steerTargets) != 1 || h.boundary.steerTargets[0] != h.active {
		t.Fatal("multipart steer did not target the original active turn exactly once")
	}
	if queue, err := h.other.Queue(h.ctx, h.thread); err != nil || len(queue) != 0 {
		t.Fatal("multipart steer left an extra queued turn")
	}
	nativeQueueDrain(t, h, 1)
	assertNativeMultipartHistory(t, nativeHistoryInput(t, h, receipt.ClientID), fixture)
}

func testNativeAttachmentRestore(t *testing.T) {
	h := newNativeQueueHarness(t, true)
	fixture := newNativeAttachmentFixture(t, h)
	receipt := h.claim(t)
	setStoredInput(t, h.controller, receipt.ID, string(fixture.input))
	h.tick(t)
	assertNativeMultipartParts(t, nativeQueuedInput(t, h, receipt.ClientID).Input, fixture, false)
	action := h.steerAction(t, receipt.ID)
	original := h.active
	var replacement string
	h.boundary.afterDelete = func(ctx context.Context, _ string) error {
		h.model.release(1)
		if err := nativeQueueEventually(ctx, func() (bool, error) {
			turns, err := h.other.Turns(ctx, h.thread)
			for _, turn := range turns {
				if turn.ID == original {
					return turn.Status == "completed", err
				}
			}
			return false, err
		}); err != nil {
			return err
		}
		if _, err := h.other.Add(ctx, h.thread, "owned multipart replacement", "owned-multipart-replacement-"+h.spec.RunID); err != nil {
			return err
		}
		if err := h.model.wait(ctx, 2); err != nil {
			return err
		}
		replacement = h.waitActive(t)
		if replacement == original {
			return errors.New("replacement did not become a distinct native turn")
		}
		return nil
	}
	h.tick(t)
	result, err := h.db.SubmissionAction(h.ctx, h.controller.scope(), action.ID)
	if err != nil || result.State != "confirmed" || result.Result != "restored" || result.ExpectedTurnID != original || replacement == "" || h.boundary.adds != 2 || h.boundary.deletes != 1 || h.boundary.steers != 1 || len(h.boundary.steerTargets) != 1 || h.boundary.steerTargets[0] != original {
		t.Fatal("multipart restore did not reject the old target and restore once")
	}
	attempts, err := h.db.SubmissionAttempts(h.ctx, h.controller.scope())
	if err != nil {
		t.Fatal("multipart restore attempt journal unavailable")
	}
	steerRejected, restoreConfirmed := 0, 0
	for _, attempt := range attempts {
		if attempt.Stage == "steer" && attempt.State == "rejected" && attempt.ExpectedTurnID == original {
			steerRejected++
		}
		if attempt.Stage == "restore" && attempt.State == "confirmed" {
			restoreConfirmed++
		}
	}
	if steerRejected != 1 || restoreConfirmed != 1 {
		t.Fatal("multipart restore did not journal one rejected steer and one confirmed restore")
	}
	assertNativeMultipartParts(t, nativeQueuedInput(t, h, receipt.ClientID).Input, fixture, false)
	h.model.release(2)
	if err := h.model.wait(h.ctx, 3); err != nil {
		t.Fatal("restored multipart input did not reach the native model")
	}
	h.model.release(3)
	if err := nativeQueueEventually(h.ctx, func() (bool, error) {
		turns, err := h.other.Turns(h.ctx, h.thread)
		completed := 0
		for _, turn := range turns {
			if turn.Status == "completed" {
				completed++
			}
		}
		return completed == 3, err
	}); err != nil {
		t.Fatal("restored multipart sequence did not finish")
	}
	h.tick(t)
	assertNativeMultipartHistory(t, nativeHistoryInput(t, h, receipt.ClientID), fixture)
}

func testNativeAttachmentUnknownAdd(t *testing.T) {
	h := newNativeQueueHarness(t, true)
	fixture := newNativeAttachmentFixture(t, h)
	receipt := h.claim(t)
	setStoredInput(t, h.controller, receipt.ID, string(fixture.input))
	h.boundary.discardAdd = true
	if err := h.controller.Tick(h.ctx); err == nil || h.boundary.adds != 1 {
		t.Fatal("accepted multipart add reply was not held unknown")
	}
	assertNativeMultipartParts(t, nativeQueuedInput(t, h, receipt.ClientID).Input, fixture, false)
	h.reopen(t)
	if err := h.controller.Tick(h.ctx); err != nil {
		t.Fatal("next Tick did not reconcile the accepted multipart add")
	}
	if h.boundary.adds != 1 {
		t.Fatal("unknown multipart add was resent")
	}
	queued := nativeQueuedInput(t, h, receipt.ClientID)
	assertNativeMultipartParts(t, queued.Input, fixture, false)
	attempts, err := h.db.SubmissionAttempts(h.ctx, h.controller.scope())
	if err != nil || len(attempts) != 1 || attempts[0].Stage != "add" || attempts[0].State != "confirmed" {
		t.Fatal("unknown multipart add did not reconcile one confirmed native attempt")
	}
	nativeQueueDrain(t, h, 2)
	assertNativeMultipartHistory(t, nativeHistoryInput(t, h, receipt.ClientID), fixture)
}
