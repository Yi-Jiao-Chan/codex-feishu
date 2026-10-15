package codexclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
)

func TestMultipartInputAddAndSteerPreserveFullWireArray(t *testing.T) {
	wantInput := json.RawMessage(`[{"type":"text","text":"inspect these files","text_elements":[]},{"type":"localImage","path":"/tmp/fixture-image.png"},{"type":"text","text":"report.txt: /tmp/report.txt"}]`)
	var addInput, steerInput json.RawMessage
	path := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		var params map[string]json.RawMessage
		if err := json.Unmarshal(w.Params, &params); err != nil {
			t.Errorf("decode %s params: %v", w.Method, err)
			return
		}
		switch w.Method {
		case "thread/queue/add":
			addInput = append(json.RawMessage(nil), params["input"]...)
			send(ctx, c, w.ID, map[string]any{"queuedSubmission": map[string]any{"id": "q-multipart", "clientUserMessageId": "cid-add"}})
		case "turn/steer":
			steerInput = append(json.RawMessage(nil), params["input"]...)
			send(ctx, c, w.ID, map[string]any{})
		}
	})
	client, err := Connect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got, err := client.AddInput(context.Background(), "thread", wantInput, "cid-add"); err != nil || got.ID != "q-multipart" {
		t.Fatalf("multipart add: q=%#v err=%v", got, err)
	}
	if err := client.SteerInput(context.Background(), "thread", "turn", wantInput, "cid-steer"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(addInput, wantInput) || !bytes.Equal(steerInput, wantInput) {
		t.Fatalf("full input was not preserved: add=%s steer=%s want=%s", addInput, steerInput, wantInput)
	}
}

func TestLegacyTextAddAndSteerKeepTextInputWire(t *testing.T) {
	want, err := json.Marshal(TextInput("legacy text"))
	if err != nil {
		t.Fatal(err)
	}
	var addInput, steerInput json.RawMessage
	path := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		var params map[string]json.RawMessage
		if err := json.Unmarshal(w.Params, &params); err != nil {
			t.Errorf("decode %s params: %v", w.Method, err)
			return
		}
		switch w.Method {
		case "thread/queue/add":
			addInput = append(json.RawMessage(nil), params["input"]...)
			send(ctx, c, w.ID, map[string]any{"queuedSubmission": map[string]any{"id": "q-legacy"}})
		case "turn/steer":
			steerInput = append(json.RawMessage(nil), params["input"]...)
			send(ctx, c, w.ID, map[string]any{})
		}
	})
	client, err := Connect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Add(context.Background(), "thread", "legacy text", "cid"); err != nil {
		t.Fatal(err)
	}
	if err := client.Steer(context.Background(), "thread", "turn", "legacy text", "cid"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(addInput, want) || !bytes.Equal(steerInput, want) {
		t.Fatalf("legacy input changed: add=%s steer=%s want=%s", addInput, steerInput, want)
	}
}

func TestInvalidNativeInputDoesNotIssueMutationRPC(t *testing.T) {
	var mutations atomic.Int32
	path := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		if w.Method == "thread/queue/add" || w.Method == "turn/steer" {
			mutations.Add(1)
		}
	})
	client, err := Connect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, input := range []json.RawMessage{
		nil,
		json.RawMessage(`[]`),
		json.RawMessage(`{"type":"text","text":"not an array"}`),
		json.RawMessage(`[{"type":"text","text":7}]`),
		json.RawMessage(`[{"type":"text","text":"x","text_elements":null}]`),
		json.RawMessage(`[{"type":"text","text":"x","path":"/tmp/not-a-text-part"}]`),
		json.RawMessage(`[{"type":"localImage","path":"relative.png"}]`),
		json.RawMessage(`[{"type":"image","url":"https://example.invalid/image.png"}]`),
	} {
		if _, err := client.AddInput(context.Background(), "thread", input, "cid"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid add input error=%v", err)
		}
		if err := client.SteerInput(context.Background(), "thread", "turn", input, "cid"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid steer input error=%v", err)
		}
	}
	if mutations.Load() != 0 {
		t.Fatalf("invalid input reached native RPC: %d mutations", mutations.Load())
	}
}
