package main

import (
	"encoding/json"
	"testing"

	"codex-feishu/internal/control"
	"codex-feishu/internal/store"
)

func TestLiveStatusSeparatesPrivateTestRoutesWithoutHidingOrdinaryFailures(t *testing.T) {
	base := control.Snapshot{Running: true, NativeConnected: true, FeishuConnected: true, HydratedBindings: 1,
		Persistent:            store.PersistentHealth{Bindings: 2},
		BindingHydrationScope: &control.BindingHydrationScope{OrdinaryBindings: 1, VerificationBindings: 1}}
	for _, name := range []string{"private-test-frozen", "legacy-provider", "ordinary-unhydrated", "negative-count", "missing-route", "extra-route", "unknown-write", "dispatching-write"} {
		t.Run(name, func(t *testing.T) {
			s := base
			scope := *base.BindingHydrationScope
			s.BindingHydrationScope = &scope
			switch name {
			case "legacy-provider":
				s.BindingHydrationScope = nil
			case "ordinary-unhydrated":
				s.HydratedBindings = 0
			case "negative-count":
				scope.OrdinaryBindings = -1
			case "missing-route":
				scope.VerificationBindings = 0
			case "extra-route":
				scope.VerificationBindings = 2
			case "unknown-write", "dispatching-write":
				state := "unknown"
				if name == "dispatching-write" {
					state = "dispatching"
				}
				s.Persistent.Outbound = map[string]int64{state: 1}
			}
			o, code := liveStatus(s)
			if name == "private-test-frozen" {
				if code != 0 || !o.OK {
					t.Fatal("private fixture permanently blocked ordinary service readiness", o, code)
				}
			} else if code != 5 || o.OK {
				t.Fatal("unknown or missing ordinary work was hidden", o, code)
			}
		})
	}
	// An explicit empty ordinary scope is distinct from an older provider
	// which did not report scope and must still reconcile the entire total.
	base.HydratedBindings = 0
	base.BindingHydrationScope = &control.BindingHydrationScope{VerificationBindings: 2}
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	var decoded control.Snapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.BindingHydrationScope == nil {
		t.Fatal("explicit zero scope lost on wire")
	}
	if o, code := liveStatus(decoded); code != 0 || !o.OK {
		t.Fatal("all-private routes required ordinary hydration", o, code)
	}
}

func TestLiveStatusRequiresConnectionsHydrationAndKnownOperations(t *testing.T) {
	base := control.Snapshot{Running: true, NativeConnected: true, FeishuConnected: true, HydratedBindings: 1, Persistent: store.PersistentHealth{Bindings: 1}}
	for _, field := range []string{"healthy", "stopped", "native", "feishu", "hydration", "unknown"} {
		t.Run(field, func(t *testing.T) {
			s := base
			switch field {
			case "stopped":
				s.Running = false
			case "native":
				s.NativeConnected = false
			case "feishu":
				s.FeishuConnected = false
			case "hydration":
				s.HydratedBindings = 0
			case "unknown":
				s.Persistent.Outbound = map[string]int64{"unknown": 1}
			}
			o, code := liveStatus(s)
			if field == "healthy" {
				if code != 0 || !o.OK || o.State != "running" {
					t.Fatal("healthy instance not recognized", o, code)
				}
			} else if code != 5 || o.OK || o.State != "reconciling" {
				t.Fatal("partial health claimed successful", o, code)
			}
		})
	}
}

func TestLiveStatusRequiresEveryJournalAndRecoveredRequestToBeKnown(t *testing.T) {
	base := control.Snapshot{Running: true, NativeConnected: true, FeishuConnected: true, HydratedBindings: 1, Persistent: store.PersistentHealth{Bindings: 1}}
	for _, test := range []struct {
		name         string
		selectCounts func(*store.PersistentHealth) *map[string]int64
		pending      []string
	}{
		{"inputs", func(h *store.PersistentHealth) *map[string]int64 { return &h.Inputs }, []string{"unknown", "submitting", "steering", "removing"}},
		{"outbound", func(h *store.PersistentHealth) *map[string]int64 { return &h.Outbound }, []string{"unknown", "dispatching"}},
		{"outbound_attempts", func(h *store.PersistentHealth) *map[string]int64 { return &h.OutboundAttempts }, []string{"unknown", "dispatching"}},
		{"native_actions", func(h *store.PersistentHealth) *map[string]int64 { return &h.NativeActions }, []string{"unknown", "running"}},
		{"native_attempts", func(h *store.PersistentHealth) *map[string]int64 { return &h.NativeAttempts }, []string{"awaiting_reconciliation", "dispatching"}},
		{"command_replies", func(h *store.PersistentHealth) *map[string]int64 { return &h.CommandReplies }, []string{"unknown", "dispatching"}},
		{"reactions", func(h *store.PersistentHealth) *map[string]int64 { return &h.Reactions }, []string{"unknown", "changing"}},
		{"group_intents", func(h *store.PersistentHealth) *map[string]int64 { return &h.GroupIntents }, []string{"unknown", "dispatching"}},
		{"task_controls", func(h *store.PersistentHealth) *map[string]int64 { return &h.TaskControls }, []string{"unknown", "pending"}},
		{"task_claims", func(h *store.PersistentHealth) *map[string]int64 { return &h.TaskClaims }, []string{"unknown", "pending"}},
		{"requests", func(h *store.PersistentHealth) *map[string]int64 { return &h.Requests }, []string{"awaiting_reconciliation", "reminder_unknown", "reminder_dispatching"}},
		{"reminder_attempts", func(h *store.PersistentHealth) *map[string]int64 { return &h.ReminderAttempts }, []string{"unknown", "dispatching"}},
	} {
		for _, state := range test.pending {
			t.Run(test.name+"/"+state, func(t *testing.T) {
				s := base
				*test.selectCounts(&s.Persistent) = map[string]int64{state: 1}
				o, code := liveStatus(s)
				if code != 5 || o.OK || o.State != "reconciling" {
					t.Fatal("unverified journal claimed healthy", o, code)
				}
			})
		}
	}
	base.Persistent.Inputs = map[string]int64{"received": 1, "queued": 2}
	base.Persistent.NativeActions = map[string]int64{"ready": 1, "confirmed": 2}
	base.Persistent.NativeAttempts = map[string]int64{"unknown": 2, "awaiting_reconciliation": 0}
	base.Persistent.CommandReplies = map[string]int64{"ready": 1, "confirmed": 2}
	base.Persistent.GroupIntents = map[string]int64{"ready": 1, "confirmed": 2}
	base.Persistent.TaskControls = map[string]int64{"available": 1, "unavailable": 2}
	base.Persistent.TaskClaims = map[string]int64{"ready": 1, "confirmed": 2}
	base.Persistent.Requests = map[string]int64{"pending": 1, "reminder_ready": 2}
	if o, code := liveStatus(base); code != 0 || !o.OK {
		t.Fatal("known queued work blocked healthy status", o, code)
	}
}

func TestLiveStatusRequiresBaselineOnlyWhenAutomaticDiscoveryEnabled(t *testing.T) {
	s := control.Snapshot{Running: true, NativeConnected: true, FeishuConnected: true, DiscoveryEnabled: true}
	if o, code := liveStatus(s); code != 5 || o.OK {
		t.Fatal("empty bindings and connected sockets concealed incomplete discovery", o, code)
	}
	s.Persistent.BaselineComplete = true
	if o, code := liveStatus(s); code != 0 || !o.OK {
		t.Fatal("completed initial baseline not recognized", o, code)
	}
	s.DiscoveryEnabled, s.Persistent.BaselineComplete = false, false
	if o, code := liveStatus(s); code != 0 || !o.OK {
		t.Fatal("disabled automatic discovery required its baseline", o, code)
	}
}
