package codexclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func goalRecords() []map[string]any {
	return []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": "thread"}},
		{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "goal-turn", "turn_attribution": map[string]any{"turn_id": "goal-turn", "turn_trigger": "goal", "root_turn_id": "goal-turn", "parent_turn_id": nil}}},
		{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{}}},
	}
}

func TestGoalOriginRequiresNativeStartIdentity(t *testing.T) {
	for _, scenario := range []string{"goal", "turn-started", "compaction", "user-trigger", "missing-attribution", "missing-attributed-turn", "mismatched-attributed-turn", "root-only", "other-turn", "duplicate-start", "conflicting-start", "wrong-record-kind", "wrong-session", "missing-meta", "duplicate-meta", "duplicate-json-key", "truncated-tail", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			records := goalRecords()
			start := records[1]["payload"].(map[string]any)
			bad := false
			switch scenario {
			case "turn-started":
				start["type"] = "turn_started"
			case "compaction":
				records = append(records, map[string]any{"type": "compacted", "payload": map[string]any{"message": "synthetic summary"}})
			case "user-trigger":
				start["turn_attribution"].(map[string]any)["turn_trigger"] = "user"
			case "missing-attribution":
				delete(start, "turn_attribution")
			case "missing-attributed-turn":
				delete(start["turn_attribution"].(map[string]any), "turn_id")
			case "mismatched-attributed-turn":
				start["turn_attribution"].(map[string]any)["turn_id"] = "different-turn"
			case "root-only":
				delete(start["turn_attribution"].(map[string]any), "turn_trigger")
			case "other-turn":
				start["turn_id"] = "different-turn"
			case "duplicate-start":
				records = append(records, records[1])
			case "conflicting-start":
				records = append(records, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "goal-turn", "turn_attribution": map[string]any{"turn_trigger": "user"}}})
			case "wrong-record-kind":
				records[1]["type"] = "response_item"
			case "wrong-session":
				records[0]["payload"].(map[string]any)["id"] = "different-thread"
				bad = true
			case "missing-meta":
				records = records[1:]
				bad = true
			case "duplicate-meta":
				records = append(records, records[0])
				bad = true
			case "duplicate-json-key", "truncated-tail", "cancelled":
				bad = true
			}
			text := receiptLines(t, records)
			if scenario == "duplicate-json-key" {
				text = strings.Replace(text, `"turn_trigger":"goal"`, `"turn_trigger":"user","turn_trigger":"goal"`, 1)
			}
			if scenario == "truncated-tail" {
				text += `{"type":"event_msg"`
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			goals, err := readGoalRollout(ctx, strings.NewReader(text), "thread")
			if bad {
				if !errors.Is(err, ErrGoalTurnUnavailable) || goals != nil {
					t.Fatal("invalid origin record was accepted", err)
				}
				return
			}
			want := scenario == "goal" || scenario == "turn-started" || scenario == "compaction"
			if err != nil || goals["goal-turn"] != want {
				t.Fatal("wrong automatic goal classification", err)
			}
		})
	}
}

func TestGoalReaderUsesDaemonHomeAndRevalidatesChangedRollout(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessions, "rollout.jsonl")
	write := func(records []map[string]any) {
		t.Helper()
		if err := os.WriteFile(path, []byte(receiptLines(t, records)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(goalRecords())
	current := path
	metadata := func(context.Context, string) (Thread, error) {
		return Thread{ID: "thread", Path: &current}, nil
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	r := NewGoalTurnReader(metadata, root)
	read := func(want bool) {
		t.Helper()
		got, err := r.Read(context.Background(), "thread", []string{"goal-turn"})
		if err != nil || got["goal-turn"] != want {
			t.Fatal("incorrect goal evidence", err)
		}
		got["goal-turn"] = !want // Callers cannot modify cached evidence.
	}
	read(true)
	read(true)
	changed := goalRecords()
	changed[1]["payload"].(map[string]any)["turn_attribution"].(map[string]any)["turn_trigger"] = "user"
	write(changed)
	read(false)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	write(goalRecords())
	read(true)
	archived := filepath.Join(root, "archived_sessions")
	if err := os.Mkdir(archived, 0700); err != nil {
		t.Fatal(err)
	}
	current = filepath.Join(archived, "rollout.jsonl")
	if err := os.Rename(path, current); err != nil {
		t.Fatal(err)
	}
	read(true)
	if _, err := r.Read(context.Background(), "different-thread", []string{"goal-turn"}); err == nil {
		t.Fatal("cross-thread metadata accepted")
	}
	if _, err := NewGoalTurnReader(metadata, "").Read(context.Background(), "thread", []string{"goal-turn"}); err == nil {
		t.Fatal("bridge environment substituted for daemon home")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); read(true) }()
	}
	wg.Wait()
}

func TestGoalOriginSurvivesLargeUnrelatedToolOutput(t *testing.T) {
	records := goalRecords()
	output := map[string]any{"type": "response_item", "payload": map[string]any{
		"type": "function_call_output", "call_id": "synthetic-call",
		"output": strings.Repeat("x", 6<<20) + `{"type":"task_started","turn_id":"fake-goal","turn_attribution":{"turn_trigger":"goal"}}`,
	}}
	records = append(records[:1], append([]map[string]any{output}, records[1:]...)...)
	goals, err := readGoalRollout(context.Background(), strings.NewReader(receiptLines(t, records)), "thread")
	if err != nil || !goals["goal-turn"] || goals["fake-goal"] {
		t.Fatal("tool output size or embedded text changed goal attribution", err)
	}
}

func TestGoalReaderRejectsUnsafeFilesAndOversizedRecords(t *testing.T) {
	for _, scenario := range []string{"outside", "symlink", "writable", "oversized-file", "oversized-line"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			sessions := filepath.Join(root, "sessions")
			if err := os.Mkdir(sessions, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(sessions, "rollout.jsonl")
			if err := os.WriteFile(path, []byte(receiptLines(t, goalRecords())), 0600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "outside":
				outside := filepath.Join(t.TempDir(), "rollout.jsonl")
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				path = outside
			case "symlink":
				link := filepath.Join(sessions, "link.jsonl")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			case "writable":
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			case "oversized-file":
				if err := os.Truncate(path, goalRolloutMaxBytes+1); err != nil {
					t.Fatal(err)
				}
			case "oversized-line":
				if err := os.WriteFile(path, []byte(strings.Repeat(" ", goalRolloutMaxLine+1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			r := NewGoalTurnReader(func(context.Context, string) (Thread, error) { return Thread{ID: "thread", Path: &path}, nil }, root)
			if _, err := r.Read(context.Background(), "thread", []string{"goal-turn"}); !errors.Is(err, ErrGoalTurnUnavailable) {
				t.Fatal("unsafe origin source accepted", err)
			}
		})
	}
}
