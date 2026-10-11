package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestDiscoveryListsAllProvidersArchivesAndLoadedDrafts(t *testing.T) {
	path := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		var p map[string]json.RawMessage
		json.Unmarshal(w.Params, &p)
		switch w.Method {
		case "thread/list":
			if string(p["modelProviders"]) != "[]" {
				t.Error("omitted provider filter hides history")
			}
			if string(p["archived"]) == "true" {
				send(ctx, c, w.ID, map[string]any{"data": []Thread{{ID: "archived"}}, "nextCursor": nil})
			} else {
				send(ctx, c, w.ID, map[string]any{"data": []Thread{{ID: "active"}}, "nextCursor": nil})
			}
		case "thread/loaded/list":
			if string(p["cursor"]) == `"second"` {
				send(ctx, c, w.ID, map[string]any{"data": []string{"draft-two"}, "nextCursor": nil})
			} else {
				send(ctx, c, w.ID, map[string]any{"data": []string{"draft-one"}, "nextCursor": "second"})
			}
		default:
			t.Errorf("unexpected mutation or request: %s", w.Method)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	active, err := c.List(ctx)
	if err != nil || len(active) != 1 || active[0].ID != "active" {
		t.Fatalf("active list: %v %v", active, err)
	}
	archived, err := c.ListArchive(ctx, true)
	if err != nil || len(archived) != 1 || archived[0].ID != "archived" {
		t.Fatalf("archive list: %v %v", archived, err)
	}
	loaded, err := c.LoadedIDs(ctx)
	if err != nil || len(loaded) != 2 || loaded[1] != "draft-two" {
		t.Fatalf("draft pagination: %v %v", loaded, err)
	}
}

func TestBlankDraftRequiresExactVersionedError(t *testing.T) {
	message := "thread fixture is not materialized yet; thread/turns/list is unavailable before first user message"
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&RPCError{Code: -32600, Message: message}, true},
		{&RPCError{Code: -32602, Message: message}, false},
		{&RPCError{Code: -32600, Message: "thread has no rollout"}, false},
		{&UnknownError{Method: "thread/turns/list", Cause: errors.New(message)}, false},
		{nil, false},
	} {
		if got := BeforeFirstInput(tc.err); got != tc.want {
			t.Fatalf("blank classification %v: %v", tc.err, got)
		}
	}
}
