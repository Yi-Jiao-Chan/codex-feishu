package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in integration against the running daemon and a disposable local model.
// This proves the Go adapter, and deliberately does not claim App UI validation.
func TestNativeDaemonQueueAndSteer(t *testing.T) {
	if os.Getenv("CODEX_FEISHU_NATIVE_TEST") != "1" {
		t.Skip("set CODEX_FEISHU_NATIVE_TEST=1 to create an isolated native test thread")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	d, err := Discover(ctx, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if d.AppServerVersion != "0.162.0" {
		t.Fatal("unverified daemon version")
	}
	var serial atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		io.Copy(io.Discard, r.Body)
		n := serial.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "close")
		event := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b); w.(http.Flusher).Flush() }
		event(map[string]any{"type": "response.created", "response": map[string]string{"id": fmt.Sprint("fixture-response-", n)}})
		if n == 1 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		id := fmt.Sprint("fixture-message-", n)
		text := fmt.Sprint("fixture reply ", n)
		item := map[string]any{"type": "message", "id": id, "role": "assistant", "phase": "final_answer", "content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}}
		event(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": id, "role": "assistant", "phase": "final_answer", "content": []any{}}})
		event(map[string]any{"type": "response.output_text.delta", "item_id": id, "output_index": 0, "content_index": 0, "delta": text})
		event(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprint("fixture-response-", n), "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	}))
	defer fixture.Close()
	a, err := Connect(ctx, d.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Connect(ctx, d.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	provider := fmt.Sprintf("cfb_go_fixture_%d", time.Now().UnixNano())
	var created struct {
		Thread Thread `json:"thread"`
	}
	err = a.Call(ctx, "thread/start", map[string]any{"cwd": t.TempDir(), "model": "fixture-model", "modelProvider": provider, "approvalPolicy": "never", "sandbox": "read-only", "threadSource": "feishu.bridge.validation", "config": map[string]any{"model_providers." + provider: map[string]any{"name": "Isolated Go fixture", "base_url": fixture.URL + "/v1", "wire_api": "responses", "requires_openai_auth": false, "supports_websockets": false, "request_max_retries": 0}, "model_reasoning_effort": "low"}}, &created)
	if err != nil {
		t.Fatal(err)
	}
	id := created.Thread.ID
	defer func() {
		once.Do(func() { close(release) })
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if e := a.Call(cleanup, "thread/archive", map[string]string{"threadId": id}, nil); e != nil {
			t.Errorf("archive isolated test: %v", e)
		}
	}()
	if _, err = a.Add(ctx, id, "initial fixture input", "cfb-test-initial"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("fixture was not invoked")
	}
	if _, err = b.Attach(ctx, id); err != nil {
		t.Fatal(err)
	}
	turns, err := b.Turns(ctx, id)
	if err != nil || len(turns) != 1 || turns[0].Status != "inProgress" {
		t.Fatalf("live turn: %v %v", turns, err)
	}
	turnID := turns[0].ID
	steer, err := b.Add(ctx, id, "fixture steering input", "cfb-test-steer")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := b.Add(ctx, id, "fixture queued input", "cfb-test-queued")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := a.Queue(ctx, id)
	if err != nil || len(queue) != 2 || queue[0].ID != steer.ID || queue[1].ID != queued.ID || serial.Load() != 1 {
		t.Fatalf("native shared queue does not preserve current turn: %v %v", queue, err)
	}
	deleted, err := b.Delete(ctx, id, steer.ID)
	if err != nil || !deleted {
		t.Fatal("exact delete claim failed")
	}
	err = b.Steer(ctx, id, "wrong-expected-turn", "must not inject", "cfb-test-rejected")
	var rejected *RPCError
	if !errors.As(err, &rejected) {
		t.Fatalf("stale turn was not rejected: %v", err)
	}
	if err = b.Steer(ctx, id, turnID, "fixture steering input", "cfb-test-steer"); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		turns, err = a.Turns(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		complete := 0
		for _, turn := range turns {
			if turn.Status == "completed" {
				complete++
			}
		}
		if complete == 2 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("native queue did not drain")
		}
	}
	correlations := map[string][]string{}
	for _, turn := range turns {
		for _, item := range turn.Items {
			if item.ClientID != nil {
				correlations[*item.ClientID] = append(correlations[*item.ClientID], turn.ID)
			}
		}
	}
	for _, cid := range []string{"cfb-test-initial", "cfb-test-steer", "cfb-test-queued"} {
		if len(correlations[cid]) != 1 {
			t.Fatalf("input persisted %d times: %s", len(correlations[cid]), cid)
		}
	}
	if correlations["cfb-test-steer"][0] != turnID || correlations["cfb-test-queued"][0] == turnID {
		t.Fatal("wrong steering / queued turn association")
	}
	if _, exists := correlations["cfb-test-rejected"]; exists {
		t.Fatal("rejected input injected")
	}
	queue, err = a.Queue(ctx, id)
	if err != nil || len(queue) != 0 {
		t.Fatal("native queue not empty")
	}
	t.Logf("daemon=%s thread=%s current_turn=%s checks=idle queue/add, shared queue, exact delete, stale rejection, expected-turn steer, ordered drain, clientId persistence; App UI NOT asserted", d.AppServerVersion, id, turnID)
	if strings.TrimSpace(id) == "" {
		t.Fatal("missing isolated thread ID")
	}
}
