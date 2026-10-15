package codexclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Two image-rich queue entries together exceed the connection's frame budget.
// Each page remains bounded and metadata reads never retain either input array.
// The content is synthetic wire data, not a visual-recognition fixture.
func TestQueueStatePagesLargeInputsWithoutRetainingImageQueue(t *testing.T) {
	url := "data:image/png;base64," + strings.Repeat("A", 17<<20)
	path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
		var params struct {
			Limit  int    `json:"limit"`
			Cursor string `json:"cursor"`
		}
		if json.Unmarshal(w.Params, &params) != nil || w.Method != "thread/queue/list" || params.Limit != 1 {
			t.Error("queue must request one input per frame")
			conn.CloseNow()
			return
		}
		id, client := "first", "client-first"
		var next any = "second"
		if params.Cursor == "second" {
			id, client, next = "second", "client-second", nil
		}
		send(ctx, conn, w.ID, map[string]any{"data": []map[string]any{{"id": id, "clientUserMessageId": client, "input": []map[string]string{{"type": "image", "url": url}}}}, "nextCursor": next})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	queue, err := client.QueueState(ctx, "owned-thread")
	if err != nil || len(queue) != 2 {
		t.Fatal("large queue pagination failed", err)
	}
	for _, item := range queue {
		if item.Input != nil || item.ClientUserMessageID == nil {
			t.Fatal("queue metadata retained input or lost correlation")
		}
	}
	if queue[0].ID != "first" || queue[1].ID != "second" {
		t.Fatal("queue pagination changed order")
	}
	full, err := client.Queue(ctx, "owned-thread")
	if err != nil || len(full) != 2 || len(full[0].Input) < 17<<20 || len(full[1].Input) < 17<<20 {
		t.Fatal("explicit full queue inspection lost input", err)
	}
}
