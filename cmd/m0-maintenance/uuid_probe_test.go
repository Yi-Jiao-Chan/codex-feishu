package main

import (
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"context"
	"path/filepath"
	"testing"
)

func TestProbeRestartNeverReissuesCompletedOrUnknownCall(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		err         error
	}{
		{"confirmed", "confirmed", nil},
		{"lost-response", "unknown", &feishu.Unknown{Operation: "test"}},
		{"rejected", "rejected", &feishu.Rejected{Code: 99991672}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "probe.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			call := func() (string, error) { calls++; return "remote-id", tc.err }
			id, fresh, err := confirmedProbe(ctx, db, "first", "test", call)
			if !fresh || (err == nil) != (tc.err == nil) || (tc.err == nil && id != "remote-id") {
				t.Fatal("incorrect initial result")
			}
			state, _, err := db.Operation(ctx, "first")
			if err != nil || state != tc.state {
				t.Fatal("remote outcome was not persisted")
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			id, fresh, err = confirmedProbe(ctx, db, "first", "test", call)
			if fresh || calls != 1 || (err == nil) != (tc.err == nil) || (tc.err == nil && id != "remote-id") {
				t.Fatal("restart reissued a call or lost its outcome")
			}
		})
	}
}
