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
	return fixtureWithHome(t, t.TempDir(), handle)
}

func fixtureWithHome(t *testing.T, home string, handle func(context.Context, *websocket.Conn, wire)) string {
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
				send(r.Context(), c, in.ID, map[string]any{"userAgent": "fixture", "codexHome": home})
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

func TestCodexHomeComesFromConnectedServer(t *testing.T) {
	serverHome, clientHome := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", clientHome)
	path := fixtureWithHome(t, serverHome, func(context.Context, *websocket.Conn, wire) {})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.CodexHome() != serverHome || client.CodexHome() == clientHome {
		t.Fatal("worktree home was guessed from the bridge environment")
	}
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

func TestIncompleteTurnItemsAreLoadedAcrossOpaquePages(t *testing.T) {
	for _, view := range []string{"summary", "notLoaded"} {
		t.Run(view, func(t *testing.T) {
			path := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				var p map[string]any
				json.Unmarshal(w.Params, &p)
				switch w.Method {
				case "thread/turns/list":
					if p["itemsView"] != "full" {
						t.Error("full view was not requested")
					}
					send(ctx, c, w.ID, map[string]any{"data": []map[string]any{{"id": "t", "itemsView": view, "items": []any{}}}, "nextCursor": nil})
				case "thread/items/list":
					if p["threadId"] != "thread" || p["turnId"] != "t" || p["sortDirection"] != "asc" {
						t.Error("item history has incorrect scope")
					}
					if p["cursor"] == "opaque-next" {
						send(ctx, c, w.ID, map[string]any{"data": []map[string]any{{"turnId": "t", "item": map[string]any{"type": "userMessage", "id": "u", "content": TextInput("first actual input")}}}, "nextCursor": nil})
					} else {
						// Even an empty page must continue while a cursor exists.
						send(ctx, c, w.ID, map[string]any{"data": []any{}, "nextCursor": "opaque-next"})
					}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client, err := Connect(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			turns, err := client.Turns(ctx, "thread")
			if err != nil || len(turns) != 1 || turns[0].ItemsView != "full" || len(turns[0].Items) != 1 || turns[0].Items[0].PublicText() != "first actual input" {
				t.Fatalf("incomplete view silently treated as no input: %v", err)
			}
		})
	}
}

func TestItemHistoryCannotCrossTurnScope(t *testing.T) {
	path := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		if w.Method == "thread/turns/list" {
			send(ctx, c, w.ID, map[string]any{"data": []map[string]any{{"id": "t", "itemsView": "notLoaded"}}, "nextCursor": nil})
		} else {
			send(ctx, c, w.ID, map[string]any{"data": []map[string]any{{"turnId": "different", "item": map[string]string{"type": "userMessage", "id": "wrong"}}}, "nextCursor": nil})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Turns(ctx, "thread"); err == nil {
		t.Fatal("wrong turn items were accepted")
	}
}

// Ordinary server requests share a callback with App. Even an error reply from
// an observer can consume it, so pass requests through without auto-answering.
func TestServerRequestIsNotRPCResponseOrAutomaticallyAnswered(t *testing.T) {
	for _, requestID := range []json.RawMessage{json.RawMessage(`2`), json.RawMessage(`"request-2"`)} {
		t.Run(string(requestID), func(t *testing.T) {
			var replies atomic.Int32
			path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
				if w.Method == "" {
					replies.Add(1)
					return
				}
				if w.Method == "probe" {
					frame, _ := json.Marshal(map[string]any{"id": requestID, "method": "item/tool/requestUserInput", "params": map[string]string{"threadId": "fixture"}})
					if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
						t.Error(err)
					}
				}
				send(ctx, conn, w.ID, map[string]string{"method": w.Method})
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client, err := Connect(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var response struct{ Method string }
			if err = client.Call(ctx, "probe", nil, &response); err != nil || response.Method != "probe" {
				t.Fatalf("request collided with RPC reply: %v", err)
			}
			select {
			case event := <-client.Events():
				if event.Method != "item/tool/requestUserInput" || string(event.RequestID) != string(requestID) {
					t.Fatal("server request ID not preserved")
				}
			case <-ctx.Done():
				t.Fatal("server request lost")
			}
			if err = client.Call(ctx, "barrier", nil, nil); err != nil {
				t.Fatal(err)
			}
			if replies.Load() != 0 {
				t.Fatal("observer answered server request")
			}
		})
	}
}
