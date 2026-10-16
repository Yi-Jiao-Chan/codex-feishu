package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func scanContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), time.Second)
}

func scanThread(id string, updated int64) map[string]any {
	return map[string]any{"id": id, "updatedAt": updated}
}

func sendScanError(ctx context.Context, conn *websocket.Conn, id json.RawMessage) {
	b, _ := json.Marshal(map[string]any{
		"id": id,
		"error": map[string]any{
			"code":    -32602,
			"message": "synthetic scan failure",
		},
	})
	_ = conn.Write(ctx, websocket.MessageText, b)
}

func TestThreadSectionMetadataDecodes(t *testing.T) {
	var thread Thread
	if err := json.Unmarshal([]byte(`{"id":"thread","section":{"id":"pinned","name":"Pinned"}}`), &thread); err != nil {
		t.Fatal(err)
	}
	if thread.Section == nil || thread.Section.ID != "pinned" || thread.Section.Name != "Pinned" {
		t.Fatalf("section metadata = %#v", thread.Section)
	}
	if err := json.Unmarshal([]byte(`{"id":"thread","section":null}`), &thread); err != nil {
		t.Fatal(err)
	}
	if thread.Section != nil {
		t.Fatal("null section must remain nil")
	}
}

func TestListArchiveSinceStopsAtVerifiedBoundaryAndIncludesEquality(t *testing.T) {
	const since = int64(100)
	var calls atomic.Int32
	path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
		if w.Method != "thread/list" {
			t.Errorf("unexpected method %q", w.Method)
			send(ctx, conn, w.ID, map[string]any{"data": []any{}})
			return
		}
		calls.Add(1)
		var params struct {
			Limit         int      `json:"limit"`
			SortKey       string   `json:"sortKey"`
			SortDirection string   `json:"sortDirection"`
			UseStateDB    bool     `json:"useStateDbOnly"`
			Archived      bool     `json:"archived"`
			Providers     []string `json:"modelProviders"`
			Cursor        *string  `json:"cursor"`
		}
		if err := json.Unmarshal(w.Params, &params); err != nil {
			t.Error(err)
		}
		if params.Limit != 100 || params.SortKey != "updated_at" || params.SortDirection != "desc" || !params.UseStateDB || params.Archived || params.Providers == nil || len(params.Providers) != 0 || params.Cursor != nil {
			t.Errorf("unexpected archive-since params: %#v", params)
		}
		if calls.Load() > 1 {
			t.Error("cutoff boundary fetched another page")
		}
		send(ctx, conn, w.ID, map[string]any{
			"data": []map[string]any{
				scanThread("new", since+1),
				scanThread("equal", since),
				scanThread("old", since-1),
			},
			"nextCursor": "must-not-be-fetched",
		})
	})
	ctx, cancel := scanContext(t)
	defer cancel()
	client, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	threads, err := client.ListArchiveSince(ctx, false, since)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(threads) != 2 || threads[0].ID != "new" || threads[1].ID != "equal" {
		t.Fatalf("cutoff result = %#v, calls = %d", threads, calls.Load())
	}
}

func TestListArchiveSinceContinuesForUnknownAndOutOfOrderTimestamps(t *testing.T) {
	for _, test := range []struct {
		name   string
		first  []map[string]any
		second []map[string]any
		want   []string
	}{
		{
			name:   "unknown",
			first:  []map[string]any{scanThread("unknown", 0), scanThread("old", 99)},
			second: []map[string]any{scanThread("later", 130), scanThread("recent", 110)},
			want:   []string{"unknown", "later", "recent"},
		},
		{
			name:   "out-of-order",
			first:  []map[string]any{scanThread("recent", 110), scanThread("old", 90), scanThread("newer", 120)},
			second: []map[string]any{scanThread("later", 130)},
			want:   []string{"recent", "newer", "later"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
				if w.Method != "thread/list" {
					t.Errorf("unexpected method %q", w.Method)
				}
				page := calls.Add(1)
				if page == 1 {
					send(ctx, conn, w.ID, map[string]any{"data": test.first, "nextCursor": "next"})
					return
				}
				if page > 2 {
					t.Errorf("unexpected page %d", page)
				}
				send(ctx, conn, w.ID, map[string]any{"data": test.second, "nextCursor": nil})
			})
			ctx, cancel := scanContext(t)
			defer cancel()
			client, err := Connect(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			threads, err := client.ListArchiveSince(ctx, true, 100)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatalf("calls = %d, want 2", calls.Load())
			}
			ids := make([]string, 0, len(threads))
			for _, thread := range threads {
				ids = append(ids, thread.ID)
			}
			if !reflect.DeepEqual(ids, test.want) {
				t.Fatalf("thread IDs = %#v, want %#v", ids, test.want)
			}
		})
	}
}

func TestListArchiveSinceDoesNotReturnPartialResultsOnPageErrorOrCursorLoop(t *testing.T) {
	for _, test := range []struct {
		name   string
		handle func(context.Context, *websocket.Conn, wire, *atomic.Int32)
	}{
		{
			name: "page error",
			handle: func(ctx context.Context, conn *websocket.Conn, w wire, calls *atomic.Int32) {
				if calls.Add(1) == 1 {
					send(ctx, conn, w.ID, map[string]any{"data": []map[string]any{scanThread("first", 110)}, "nextCursor": "next"})
					return
				}
				sendScanError(ctx, conn, w.ID)
			},
		},
		{
			name: "cursor loop",
			handle: func(ctx context.Context, conn *websocket.Conn, w wire, calls *atomic.Int32) {
				calls.Add(1)
				send(ctx, conn, w.ID, map[string]any{"data": []map[string]any{scanThread("first", 110)}, "nextCursor": "loop"})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
				if w.Method != "thread/list" {
					t.Errorf("unexpected method %q", w.Method)
				}
				test.handle(ctx, conn, w, &calls)
			})
			ctx, cancel := scanContext(t)
			defer cancel()
			client, err := Connect(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			threads, err := client.ListArchiveSince(ctx, false, 100)
			if err == nil || threads != nil {
				t.Fatalf("partial scan result = %#v, err = %v", threads, err)
			}
			if test.name == "cursor loop" && calls.Load() != 2 {
				t.Fatalf("cursor loop calls = %d, want 2", calls.Load())
			}
		})
	}
}

func TestSectionsPaginateAndRejectEmptyIdentity(t *testing.T) {
	var calls atomic.Int32
	path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
		if w.Method != "threadSection/list" {
			t.Errorf("unexpected method %q", w.Method)
		}
		var params struct {
			Limit  int     `json:"limit"`
			Cursor *string `json:"cursor"`
		}
		if err := json.Unmarshal(w.Params, &params); err != nil {
			t.Error(err)
		}
		if params.Limit != 100 {
			t.Errorf("limit = %d", params.Limit)
		}
		page := calls.Add(1)
		if page == 1 {
			if params.Cursor != nil {
				t.Error("first section page unexpectedly had a cursor")
			}
			send(ctx, conn, w.ID, map[string]any{"data": []map[string]string{{"id": "pinned", "name": "Pinned"}}, "nextCursor": "next"})
			return
		}
		if params.Cursor == nil || *params.Cursor != "next" {
			t.Errorf("second section cursor = %#v", params.Cursor)
		}
		send(ctx, conn, w.ID, map[string]any{"data": []map[string]string{{"id": "work", "name": "Work"}}, "nextCursor": nil})
	})
	ctx, cancel := scanContext(t)
	defer cancel()
	client, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sections, err := client.Sections(ctx)
	if err != nil || len(sections) != 2 || sections[0].ID != "pinned" || sections[1].Name != "Work" {
		t.Fatalf("sections = %#v, err = %v", sections, err)
	}

	var emptyCalls atomic.Int32
	emptyPath := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
		emptyCalls.Add(1)
		send(ctx, conn, w.ID, map[string]any{"data": []map[string]string{{"id": "", "name": "Unnamed"}}})
	})
	emptyClient, err := Connect(ctx, emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer emptyClient.Close()
	got, err := emptyClient.Sections(ctx)
	if err == nil || got != nil || emptyCalls.Load() != 1 {
		t.Fatalf("empty section identity = %#v, err = %v, calls = %d", got, err, emptyCalls.Load())
	}
}

func TestListSectionUsesSectionFilterAndRejectsEmptyID(t *testing.T) {
	wantSourceKinds := []string{"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown"}
	var calls atomic.Int32
	path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
		if w.Method != "thread/list" {
			t.Errorf("unexpected method %q", w.Method)
		}
		var params struct {
			SectionID     string   `json:"sectionId"`
			Limit         int      `json:"limit"`
			SortKey       string   `json:"sortKey"`
			SortDirection string   `json:"sortDirection"`
			UseStateDB    bool     `json:"useStateDbOnly"`
			Archived      bool     `json:"archived"`
			Providers     []string `json:"modelProviders"`
			SourceKinds   []string `json:"sourceKinds"`
			Cursor        *string  `json:"cursor"`
		}
		if err := json.Unmarshal(w.Params, &params); err != nil {
			t.Error(err)
		}
		if params.SectionID != "pinned" || params.Limit != 100 || params.SortKey != "section_position" || params.SortDirection != "asc" || !params.UseStateDB || !params.Archived || params.Providers == nil || len(params.Providers) != 0 || !reflect.DeepEqual(params.SourceKinds, wantSourceKinds) {
			t.Errorf("unexpected section params: %#v", params)
		}
		page := calls.Add(1)
		if page == 1 {
			if params.Cursor != nil {
				t.Error("first section thread page unexpectedly had a cursor")
			}
			send(ctx, conn, w.ID, map[string]any{"data": []map[string]any{{"id": "first", "updatedAt": 1, "source": "appServer"}}, "nextCursor": "next"})
			return
		}
		if params.Cursor == nil || *params.Cursor != "next" {
			t.Errorf("second section thread cursor = %#v", params.Cursor)
		}
		send(ctx, conn, w.ID, map[string]any{"data": []map[string]any{scanThread("second", 2)}, "nextCursor": nil})
	})
	ctx, cancel := scanContext(t)
	defer cancel()
	client, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	threads, err := client.ListSection(ctx, "pinned", true)
	if err != nil || len(threads) != 2 || threads[0].ID != "first" || string(threads[0].Source) != `"appServer"` || threads[1].ID != "second" {
		t.Fatalf("section threads = %#v, err = %v", threads, err)
	}

	var emptyCalls atomic.Int32
	emptyPath := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
		emptyCalls.Add(1)
		t.Errorf("empty section ID reached native method %q", w.Method)
	})
	emptyClient, err := Connect(ctx, emptyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer emptyClient.Close()
	for _, sectionID := range []string{"", "   "} {
		got, err := emptyClient.ListSection(ctx, sectionID, false)
		if err == nil || got != nil {
			t.Fatalf("section ID %q result = %#v, err = %v", sectionID, got, err)
		}
	}
	if emptyCalls.Load() != 0 {
		t.Fatal("empty section ID triggered native RPC")
	}
}

func TestSectionAdaptersRejectMissingOrNullLaterData(t *testing.T) {
	for _, malformed := range []struct {
		name string
		data any
	}{
		{name: "missing", data: struct{}{}},
		{name: "null", data: nil},
	} {
		t.Run("sections/"+malformed.name, func(t *testing.T) {
			var calls atomic.Int32
			path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
				if w.Method != "threadSection/list" {
					t.Errorf("unexpected method %q", w.Method)
				}
				if calls.Add(1) == 1 {
					send(ctx, conn, w.ID, map[string]any{"data": []map[string]string{{"id": "pinned", "name": "Pinned"}}, "nextCursor": "next"})
					return
				}
				response := map[string]any{"nextCursor": nil}
				if malformed.name == "null" {
					response["data"] = malformed.data
				}
				send(ctx, conn, w.ID, response)
			})
			ctx, cancel := scanContext(t)
			defer cancel()
			client, err := Connect(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			sections, err := client.Sections(ctx)
			if err == nil || sections != nil {
				t.Fatalf("malformed section result = %#v, err = %v", sections, err)
			}
		})

		t.Run("section/"+malformed.name, func(t *testing.T) {
			var calls atomic.Int32
			path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
				if w.Method != "thread/list" {
					t.Errorf("unexpected method %q", w.Method)
				}
				if calls.Add(1) == 1 {
					send(ctx, conn, w.ID, map[string]any{"data": []map[string]any{scanThread("first", 1)}, "nextCursor": "next"})
					return
				}
				response := map[string]any{"nextCursor": nil}
				if malformed.name == "null" {
					response["data"] = malformed.data
				}
				send(ctx, conn, w.ID, response)
			})
			ctx, cancel := scanContext(t)
			defer cancel()
			client, err := Connect(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			threads, err := client.ListSection(ctx, "pinned", false)
			if err == nil || threads != nil {
				t.Fatalf("malformed section thread result = %#v, err = %v", threads, err)
			}
		})
	}
}

func TestListArchiveSinceNonPositiveUsesFullArchiveList(t *testing.T) {
	var calls atomic.Int32
	path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
		if w.Method != "thread/list" {
			t.Errorf("unexpected method %q", w.Method)
		}
		calls.Add(1)
		send(ctx, conn, w.ID, map[string]any{"data": []map[string]any{scanThread("full", 0)}, "nextCursor": nil})
	})
	ctx, cancel := scanContext(t)
	defer cancel()
	client, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, since := range []int64{0, -1} {
		threads, err := client.ListArchiveSince(ctx, false, since)
		if err != nil || len(threads) != 1 || threads[0].ID != "full" {
			t.Fatalf("since %d result = %#v, err = %v", since, threads, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("full archive calls = %d, want 2", calls.Load())
	}
}

func TestSectionsPageErrorDoesNotReturnPartialResults(t *testing.T) {
	var calls atomic.Int32
	path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
		if w.Method != "threadSection/list" {
			t.Errorf("unexpected method %q", w.Method)
		}
		if calls.Add(1) == 1 {
			send(ctx, conn, w.ID, map[string]any{"data": []map[string]string{{"id": "first", "name": "First"}}, "nextCursor": "next"})
			return
		}
		sendScanError(ctx, conn, w.ID)
	})
	ctx, cancel := scanContext(t)
	defer cancel()
	client, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sections, err := client.Sections(ctx)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || sections != nil {
		t.Fatalf("partial sections = %#v, err = %v", sections, err)
	}
}
