package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/coder/websocket"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A Unix WebSocket fixture exercises our real framing/multiplexing, not a mock Client.
func fixture(t *testing.T, handle func(context.Context, *websocket.Conn, wire)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var in wire
			if err = json.Unmarshal(b, &in); err != nil {
				return
			}
			if in.Method == "initialize" {
				send(r.Context(), c, in.ID, map[string]any{"userAgent": "fixture"})
				continue
			}
			if in.Method == "initialized" {
				continue
			}
			handle(r.Context(), c, in)
		}
	})}
	go server.Serve(l)
	t.Cleanup(func() { server.Close() })
	return path
}
func send(ctx context.Context, c *websocket.Conn, id json.RawMessage, result any) {
	b, _ := json.Marshal(map[string]any{"id": id, "result": result})
	c.Write(ctx, websocket.MessageText, b)
}
func TestConcurrentResponsesAndNotifications(t *testing.T) {
	path := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		go func() {
			if w.Method == "slow" {
				time.Sleep(20 * time.Millisecond)
			}
			send(ctx, c, w.ID, map[string]string{"method": w.Method})
		}()
		b, _ := json.Marshal(map[string]any{"method": "thread/queue/changed", "params": map[string]string{"threadId": "fixture"}})
		c.Write(ctx, websocket.MessageText, b)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var wg sync.WaitGroup
	for _, method := range []string{"slow", "fast"} {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			var got struct {
				Method string `json:"method"`
			}
			if err := c.Call(ctx, m, map[string]any{}, &got); err != nil || got.Method != m {
				t.Errorf("multiplex %s: got %q err %v", m, got.Method, err)
			}
		}(method)
	}
	wg.Wait()
	for n := 0; n < 2; n++ {
		select {
		case e := <-c.Events():
			if e.Method != "thread/queue/changed" || e.Generation == 0 {
				t.Fatal("notification lost")
			}
		case <-ctx.Done():
			t.Fatal("missing notification")
		}
	}
}
func TestDroppedMutationResponseIsUnknownAndSentOnce(t *testing.T) {
	var writes atomic.Int32
	path := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) { writes.Add(1); c.CloseNow() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Add(ctx, "fixture", "hello", "stable-correlation")
	var unknown *UnknownError
	if !errors.As(err, &unknown) || writes.Load() != 1 {
		t.Fatalf("ambiguous mutation classification: %v writes %d", err, writes.Load())
	}
}
func TestExplicitRejectionIsNotUnknown(t *testing.T) {
	path := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		b, _ := json.Marshal(map[string]any{"id": w.ID, "error": map[string]any{"code": -32602, "message": "wrong expected turn"}})
		c.Write(ctx, websocket.MessageText, b)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Steer(ctx, "fixture", "stale", "hello", "cid")
	var rejected *RPCError
	var unknown *UnknownError
	if !errors.As(err, &rejected) || errors.As(err, &unknown) {
		t.Fatalf("wrong rejection classification: %v", err)
	}
}
func TestQueuePaginationAndWireIdentifiers(t *testing.T) {
	path := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		var p map[string]any
		json.Unmarshal(w.Params, &p)
		switch w.Method {
		case "thread/queue/list":
			if p["cursor"] == "second" {
				send(ctx, c, w.ID, map[string]any{"data": []map[string]any{{"id": "q2"}}, "nextCursor": nil})
			} else {
				send(ctx, c, w.ID, map[string]any{"data": []map[string]any{{"id": "q1"}}, "nextCursor": "second"})
			}
		case "thread/queue/add":
			if p["clientUserMessageId"] != "cid" {
				t.Error("wrong client wire key")
			}
			send(ctx, c, w.ID, map[string]any{"queuedSubmission": map[string]any{"id": "native-q", "clientUserMessageId": "cid"}})
		case "thread/queue/delete":
			if p["queuedSubmissionId"] != "native-q" {
				t.Error("wrong deletion identifier")
			}
			send(ctx, c, w.ID, map[string]bool{"deleted": false})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	q, err := c.Queue(ctx, "fixture")
	if err != nil || len(q) != 2 || q[1].ID != "q2" {
		t.Fatalf("queue pagination: %v %v", q, err)
	}
	added, err := c.Add(ctx, "fixture", "hello", "cid")
	if err != nil || added.ID != "native-q" {
		t.Fatal("missing native id")
	}
	deleted, err := c.Delete(ctx, "fixture", added.ID)
	if err != nil || deleted {
		t.Fatal("false deletion must not confirm success")
	}
}
func TestPublicContentExcludesHiddenItems(t *testing.T) {
	var user Item
	if err := json.Unmarshal([]byte(`{"type":"userMessage","id":"i","clientId":"cid","content":[{"type":"text","text":"human"}]}`), &user); err != nil {
		t.Fatal(err)
	}
	if user.PublicText() != "human" || user.ClientID == nil || *user.ClientID != "cid" {
		t.Fatal("user history correlation wrong")
	}
	if (Item{Type: "reasoning", Text: "private"}).PublicText() != "" {
		t.Fatal("hidden item exposed")
	}
}
