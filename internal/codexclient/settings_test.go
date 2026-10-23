package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
)

func settingsString(s string) *string { return &s }

// Synthetic fixtures follow the locally generated 0.162.1 experimental schema.
func TestSettingsPartialUpdatesMatchFrozenRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch SettingsPatch
		want  string
	}{
		{"model", SettingsPatch{Model: settingsString("model-a")}, `{"model":"model-a","threadId":"thread-a"}`},
		{"effort", SettingsPatch{ReasoningEffort: settingsString("future-effort")}, `{"effort":"future-effort","threadId":"thread-a"}`},
		{"tier", SettingsPatch{ServiceTier: &ServiceTierChange{Value: settingsString("priority")}}, `{"serviceTier":"priority","threadId":"thread-a"}`},
		{"standard", SettingsPatch{ServiceTier: &ServiceTierChange{Value: settingsString("default")}}, `{"serviceTier":"default","threadId":"thread-a"}`},
		{"clear-tier", SettingsPatch{ServiceTier: &ServiceTierChange{}}, `{"serviceTier":null,"threadId":"thread-a"}`},
		{"combined", SettingsPatch{Model: settingsString("model-a"), ReasoningEffort: settingsString("high")}, `{"effort":"high","model":"model-a","threadId":"thread-a"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frozen, err := SettingsRequestJSON("thread-a", tc.patch)
			if err != nil || frozen != tc.want {
				t.Fatalf("frozen params: %q %v", frozen, err)
			}
			var calls atomic.Int32
			c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				calls.Add(1)
				if w.Method != "thread/settings/update" || string(w.Params) != frozen {
					t.Error("request differs from journaled patch")
				}
				send(ctx, c, w.ID, map[string]any{})
			})
			if err := c.UpdateThreadSettings(ctx, "thread-a", tc.patch); err != nil || calls.Load() != 1 {
				t.Fatalf("update: %v (%d calls)", err, calls.Load())
			}
		})
	}
}

func TestSettingsInvalidChangesNeverDispatch(t *testing.T) {
	var calls atomic.Int32
	c, ctx := creationClient(t, func(context.Context, *websocket.Conn, wire) { calls.Add(1) })
	for _, patch := range []SettingsPatch{{}, {Model: settingsString("")}, {Model: settingsString(" model-a")}, {ReasoningEffort: settingsString("high\n")}, {ServiceTier: &ServiceTierChange{Value: settingsString("")}}} {
		if _, err := SettingsRequestJSON("thread-a", patch); !errors.Is(err, ErrInvalidSettings) {
			t.Fatal("invalid request frozen")
		}
		if err := c.UpdateThreadSettings(ctx, "thread-a", patch); !errors.Is(err, ErrInvalidSettings) {
			t.Fatal("invalid request dispatched")
		}
	}
	if err := c.UpdateThreadSettings(ctx, "", SettingsPatch{Model: settingsString("model-a")}); !errors.Is(err, ErrInvalidSettings) {
		t.Fatal("invalid identity accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("local invalid settings reached RPC")
	}
}

func TestSettingsMutationPreservesRejectionAndAmbiguity(t *testing.T) {
	for _, kind := range []string{"rejected", "dropped", "null", "array", "scalar"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				calls.Add(1)
				switch kind {
				case "rejected":
					b, _ := json.Marshal(map[string]any{"id": w.ID, "error": map[string]any{"code": -32601, "message": "unsupported"}})
					c.Write(ctx, websocket.MessageText, b)
				case "dropped":
					c.CloseNow()
				case "null":
					send(ctx, c, w.ID, nil)
				case "array":
					send(ctx, c, w.ID, []any{})
				case "scalar":
					send(ctx, c, w.ID, true)
				}
			})
			err := c.UpdateThreadSettings(ctx, "thread-a", SettingsPatch{Model: settingsString("model-a")})
			var rejected *RPCError
			var unknown *UnknownError
			if kind == "rejected" {
				if !errors.As(err, &rejected) || errors.As(err, &unknown) {
					t.Fatalf("lost rejection: %v", err)
				}
			} else if !errors.As(err, &unknown) {
				t.Fatalf("lost ambiguity: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatal("mutation retried")
			}
		})
	}
}

func TestSettingsObservationNeverOverridesThread(t *testing.T) {
	for _, kind := range []string{"priority", "default", "null", "missing", "bad-tier", "wrong-thread", "bad-model", "null-effort"} {
		t.Run(kind, func(t *testing.T) {
			c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				if w.Method != "thread/resume" || string(w.Params) != `{"excludeTurns":true,"threadId":"thread-a"}` {
					t.Error("observation sent overrides")
				}
				r := map[string]any{"thread": map[string]any{"id": "thread-a"}, "model": "model-a", "modelProvider": "provider-a", "reasoningEffort": "high"}
				switch kind {
				case "priority", "default":
					r["serviceTier"] = kind
				case "null":
					r["serviceTier"] = nil
				case "bad-tier":
					r["serviceTier"] = false
				case "wrong-thread":
					r["thread"] = map[string]any{"id": "thread-b"}
				case "bad-model":
					r["model"] = nil
				case "null-effort":
					r["reasoningEffort"] = nil
				}
				send(ctx, c, w.ID, r)
			})
			r, err := c.ReadThreadSettings(ctx, "thread-a")
			if kind == "bad-tier" || kind == "wrong-thread" || kind == "bad-model" {
				if !errors.Is(err, ErrSettingsUnavailable) {
					t.Fatalf("bad observation accepted: %v", err)
				}
				return
			}
			tierKnown := kind == "priority" || kind == "default" || kind == "null"
			if err != nil || r.Model != "model-a" || r.ServiceTierKnown != tierKnown {
				t.Fatalf("lost setting evidence: %+v %v", r, err)
			}
			if kind == "null-effort" {
				if r.ReasoningEffort != nil {
					t.Fatal("null effort invented")
				}
			} else if r.ReasoningEffort == nil || *r.ReasoningEffort != "high" {
				t.Fatal("lost effort")
			}
			if !tierKnown || kind == "null" {
				if r.ServiceTier != nil {
					t.Fatal("unavailable or clear tier became explicit off")
				}
			} else if r.ServiceTier == nil || *r.ServiceTier != kind {
				t.Fatal("lost explicit tier")
			}
		})
	}
}

func TestSettingsUpdatedUsesNativeEffortKey(t *testing.T) {
	r, err := ParseThreadSettingsUpdated(Event{Method: "thread/settings/updated", Params: json.RawMessage(`{"threadId":"thread-a","threadSettings":{"model":"model-a","modelProvider":"provider-a","effort":"ultra","serviceTier":null}}`)})
	if err != nil || r.ReasoningEffort == nil || *r.ReasoningEffort != "ultra" || !r.ServiceTierKnown || r.ServiceTier != nil {
		t.Fatalf("notification: %+v %v", r, err)
	}
	if _, err := ParseThreadSettingsUpdated(Event{Method: "turn/started", Params: json.RawMessage(`{}`)}); !errors.Is(err, ErrSettingsUnavailable) {
		t.Fatal("non-settings event accepted")
	}
	var thread Thread
	if err := json.Unmarshal([]byte(`{"model":"model-a","reasoningEffort":"future-effort"}`), &thread); err != nil || thread.Model == nil || thread.ReasoningEffort == nil {
		t.Fatal("thread metadata dropped settings")
	}
}

func catalogModel(id string) Model {
	return Model{ID: id, Model: "rpc-" + id, DisplayName: "Model " + id, DefaultReasoningEffort: "high", SupportedReasoningEfforts: []ReasoningEffortOption{{ReasoningEffort: "high"}, {ReasoningEffort: "ultra"}}, ServiceTiers: []ModelServiceTier{{ID: "priority", Name: "Fast"}, {ID: "default", Name: "Standard"}}}
}

func TestModelCatalogOpaquePaginationAndOptions(t *testing.T) {
	var cursors []any
	c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		if w.Method != "model/list" {
			t.Error("wrong catalog method")
		}
		var p map[string]any
		json.Unmarshal(w.Params, &p)
		if p["limit"] != float64(100) || p["includeHidden"] != false {
			t.Error("wrong catalog selection")
		}
		cursors = append(cursors, p["cursor"])
		switch p["cursor"] {
		case nil:
			send(ctx, c, w.ID, map[string]any{"data": []Model{catalogModel("a")}, "nextCursor": "opaque-a"})
		case "opaque-a":
			send(ctx, c, w.ID, map[string]any{"data": []Model{}, "nextCursor": "opaque-b"})
		case "opaque-b":
			send(ctx, c, w.ID, map[string]any{"data": []Model{catalogModel("b")}})
		}
	})
	models, err := c.ListModels(ctx)
	if err != nil || len(models) != 2 || !reflect.DeepEqual(cursors, []any{nil, "opaque-a", "opaque-b"}) {
		t.Fatalf("catalog: %d %v", len(models), err)
	}
	if !models[0].SupportsReasoningEffort("ultra") || models[0].SupportsReasoningEffort("guessed") || !models[0].SupportsServiceTier("priority") || models[0].SupportsServiceTier("fast") {
		t.Fatal("catalog option validation guessed aliases")
	}
}

func TestModelCatalogRejectsIncompleteAndLoopingPages(t *testing.T) {
	for _, kind := range []string{"missing", "null", "bad-effort", "loop", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				r := map[string]any{"data": []Model{catalogModel("a")}}
				switch kind {
				case "missing":
					delete(r, "data")
				case "null":
					r["data"] = nil
				case "bad-effort":
					m := catalogModel("a")
					m.SupportedReasoningEfforts = nil
					r["data"] = []Model{m}
				case "loop":
					r["data"] = []Model{}
					r["nextCursor"] = "same"
				case "duplicate":
					r["data"] = []Model{catalogModel("a"), catalogModel("a")}
				}
				send(ctx, c, w.ID, r)
			})
			if models, err := c.ListModels(ctx); err == nil || models != nil {
				t.Fatalf("partial/malformed catalog accepted: %+v %v", models, err)
			}
		})
	}
}
