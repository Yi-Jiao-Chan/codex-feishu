package submissions

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// This opt-in probe inspects only this test's owned local-model thread. It
// reports payload lengths and shape, never image data, input text or paths.
func TestNativeAttachmentWireEnvelopes(t *testing.T) {
	h := newNativeQueueHarness(t, true)
	fixture := newNativeAttachmentFixture(t, h)
	q, err := h.other.AddInput(h.ctx, h.thread, fixture.input, "owned-wire-queued-"+h.spec.RunID)
	if err != nil {
		t.Fatal("owned wire fixture add failed")
	}
	assertNativeWireQueueInput(t, q.Input)
	t.Logf("queue/add input %s", attachmentWireParts(q.Input))
	var queue json.RawMessage
	if err := h.other.Call(h.ctx, "thread/queue/list", map[string]any{"threadId": h.thread, "limit": 1}, &queue); err != nil {
		t.Fatal("owned wire queue page failed")
	}
	assertNativeWireQueuePage(t, queue)
	t.Logf("queue/list limit=1 result_bytes=%d", len(queue))
	if deleted, err := h.other.Delete(h.ctx, h.thread, q.ID); err != nil || !deleted {
		t.Fatal("owned wire queue cleanup failed")
	}
	for i := 0; i < 2; i++ {
		if err := h.other.SteerInput(h.ctx, h.thread, h.active, fixture.input, fmt.Sprintf("owned-wire-steer-%s-%d", h.spec.RunID, i)); err != nil {
			t.Fatal("owned wire steer failed")
		}
	}
	nativeQueueDrain(t, h, 1)
	for _, view := range []string{"notLoaded", "summary", "full"} {
		var raw json.RawMessage
		if err := h.other.Call(h.ctx, "thread/turns/list", map[string]any{"threadId": h.thread, "limit": 1, "itemsView": view}, &raw); err != nil {
			t.Fatal("owned wire turn page failed")
		}
		var response struct {
			Data []struct {
				Items     []json.RawMessage `json:"items"`
				ItemsView string            `json:"itemsView"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &response) != nil || len(response.Data) != 1 {
			t.Fatal("owned wire turn page shape invalid")
		}
		turn := response.Data[0]
		if imageBytes := attachmentWireItemImages(turn.Items); imageBytes != 0 {
			t.Fatalf("history turn page retained data-url image bytes: view=%s bytes=%d", view, imageBytes)
		}
		t.Logf("turns/list requested=%s returned=%s result_bytes=%d items=%d image_data_url_bytes=%d", view, turn.ItemsView, len(raw), len(turn.Items), attachmentWireItemImages(turn.Items))
		if view == "full" {
			t.Logf("full-history image shapes=%v", attachmentWireItemShapes(turn.Items))
		}
		if view == "notLoaded" && len(turn.Items) != 0 {
			t.Fatal("notLoaded unexpectedly included turn items")
		}
	}
	var cursor any
	for {
		var raw json.RawMessage
		params := map[string]any{"threadId": h.thread, "turnId": h.active, "limit": 1, "sortDirection": "asc"}
		if cursor != nil {
			params["cursor"] = cursor
		}
		if err := h.other.Call(h.ctx, "thread/items/list", params, &raw); err != nil {
			t.Fatal("owned wire item page failed")
		}
		var response struct {
			Data []struct {
				Item json.RawMessage `json:"item"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &response) != nil {
			t.Fatal("owned wire item page shape invalid")
		}
		var items []json.RawMessage
		for _, entry := range response.Data {
			items = append(items, entry.Item)
		}
		if imageBytes := attachmentWireItemImages(items); imageBytes != 0 {
			t.Fatalf("history item page retained data-url image bytes: bytes=%d", imageBytes)
		}
		t.Logf("items/list limit=1 result_bytes=%d items=%d image_data_url_bytes=%d", len(raw), len(items), attachmentWireItemImages(items))
		if response.NextCursor == nil || *response.NextCursor == "" {
			break
		}
		cursor = *response.NextCursor
	}
	foundQueue, foundComplete := false, false
	for {
		select {
		case event, ok := <-h.other.Events():
			if !ok {
				t.Fatal("owned wire fixture connection ended")
			}
			var p struct {
				ThreadID string          `json:"threadId"`
				Item     json.RawMessage `json:"item"`
				Turn     struct {
					Items     []json.RawMessage `json:"items"`
					ItemsView string            `json:"itemsView"`
				} `json:"turn"`
			}
			if json.Unmarshal(event.Params, &p) != nil || p.ThreadID != h.thread {
				continue
			}
			switch event.Method {
			case "thread/queue/changed":
				foundQueue = true
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(event.Params, &fields); err != nil {
					t.Fatal("owned queue changed event shape invalid")
				}
				keys := make([]string, 0, len(fields))
				for key := range fields {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				if len(keys) != 1 || keys[0] != "threadId" {
					t.Fatalf("queue changed event expanded beyond threadId: keys=%v", keys)
				}
				t.Logf("event=%s params_bytes=%d keys=%v", event.Method, len(event.Params), keys)
			case "turn/started", "turn/completed":
				foundComplete = foundComplete || event.Method == "turn/completed"
				if imageBytes := attachmentWireItemImages(p.Turn.Items); imageBytes != 0 {
					t.Fatalf("turn event retained data-url image bytes: event=%s bytes=%d", event.Method, imageBytes)
				}
				t.Logf("event=%s params_bytes=%d items_view=%s items=%d image_data_url_bytes=%d", event.Method, len(event.Params), p.Turn.ItemsView, len(p.Turn.Items), attachmentWireItemImages(p.Turn.Items))
			case "item/started", "item/completed":
				if imageBytes := attachmentWireItemImages([]json.RawMessage{p.Item}); imageBytes != 0 {
					t.Fatalf("item event retained data-url image bytes: event=%s bytes=%d", event.Method, imageBytes)
				}
				t.Logf("event=%s params_bytes=%d image_data_url_bytes=%d", event.Method, len(event.Params), attachmentWireItemImages([]json.RawMessage{p.Item}))
			}
		default:
			if !foundQueue || !foundComplete {
				t.Fatal("owned wire fixture notifications missing")
			}
			return
		}
	}
}

func assertNativeWireQueueInput(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var parts []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) != 4 {
		t.Fatal("queue/add input shape did not retain four parts")
	}
	wantTypes := []string{"text", "image", "text", "image"}
	imageCount, dataURLCount := 0, 0
	for i, part := range parts {
		if part.Type != wantTypes[i] {
			t.Fatalf("queue/add input type changed at position %d: got=%s", i, part.Type)
		}
		if part.Type == "image" {
			imageCount++
			if !strings.HasPrefix(part.URL, "data:image/") {
				t.Fatal("queue/add image was not canonicalized to an image data URL")
			}
			dataURLCount++
		}
	}
	if imageCount != 2 || dataURLCount != 2 {
		t.Fatalf("queue/add image count changed: images=%d data_urls=%d", imageCount, dataURLCount)
	}
}

func assertNativeWireQueuePage(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var response struct {
		Data []struct {
			Input json.RawMessage `json:"input"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || len(response.Data) != 1 {
		t.Fatal("queue/list response shape invalid")
	}
	assertNativeWireQueueInput(t, response.Data[0].Input)
}

func attachmentWireParts(raw json.RawMessage) string {
	var parts []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	_ = json.Unmarshal(raw, &parts)
	kinds := make([]string, 0, len(parts))
	var dataBytes int
	for _, p := range parts {
		kinds = append(kinds, p.Type)
		if strings.HasPrefix(p.URL, "data:") {
			dataBytes += len(p.URL)
		}
	}
	return fmt.Sprintf("bytes=%d kinds=%v image_data_url_bytes=%d", len(raw), kinds, dataBytes)
}

func attachmentWireItemImages(items []json.RawMessage) int {
	var size int
	for _, raw := range items {
		var item struct {
			Content []struct {
				URL string `json:"url"`
			} `json:"content"`
		}
		_ = json.Unmarshal(raw, &item)
		for _, p := range item.Content {
			if strings.HasPrefix(p.URL, "data:") {
				size += len(p.URL)
			}
		}
	}
	return size
}

func attachmentWireItemShapes(items []json.RawMessage) []string {
	var shapes []string
	for _, raw := range items {
		var item struct {
			Content []map[string]json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(raw, &item)
		for _, part := range item.Content {
			var kind string
			_ = json.Unmarshal(part["type"], &kind)
			if kind == "text" {
				continue
			}
			keys := make([]string, 0, len(part))
			for key, value := range part {
				var text string
				if json.Unmarshal(value, &text) == nil {
					class := "string"
					for _, prefix := range []string{"data:", "file:", "http:", "https:"} {
						if strings.HasPrefix(text, prefix) {
							class = prefix
						}
					}
					keys = append(keys, fmt.Sprintf("%s=%s(%d)", key, class, len(text)))
				} else {
					keys = append(keys, fmt.Sprintf("%s=json(%d)", key, len(value)))
				}
			}
			sort.Strings(keys)
			shapes = append(shapes, fmt.Sprintf("%s:%v", kind, keys))
		}
	}
	return shapes
}
