package codexclient

import (
	"encoding/json"
	"testing"
)

func TestInteractiveMainDoesNotConfuseForkAndSubagents(t *testing.T) {
	for _, tc := range []struct {
		name, meta string
		want       bool
	}{
		{"app-compatible", `{"source":"vscode","originator":"codex-tui","threadSource":null}`, true},
		{"cli-compatible", `{"source":"cli","threadSource":"user"}`, true},
		{"human-fork", `{"source":"vscode","forkedFromId":"parent","parentThreadId":null}`, true},
		{"atlas", `{"source":{"custom":"atlas"}}`, true},
		{"chatgpt", `{"source":{"custom":"chatgpt"}}`, true},
		{"other-custom", `{"source":{"custom":"unverified"}}`, false},
		{"parent", `{"source":"vscode","parentThreadId":"parent","forkedFromId":"human"}`, false},
		{"spawn", `{"source":{"subAgent":{"thread_spawn":{"parent_thread_id":"p","depth":1}}}}`, false},
		{"review", `{"source":{"subAgent":"review"}}`, false},
		{"compact", `{"source":{"subAgent":"compact"}}`, false},
		{"memory", `{"source":{"subAgent":"memory_consolidation"}}`, false},
		{"other-agent", `{"source":{"subAgent":{"other":"new-agent"}}}`, false},
		{"legacy-subagent", `{"source":{"subagent":"review"}}`, false},
		{"subagent-label", `{"source":"vscode","threadSource":"subagent"}`, false},
		{"guardian-label", `{"source":"vscode","threadSource":"guardian_review"}`, false},
		{"memory-label", `{"source":"vscode","threadSource":"memory_consolidation"}`, false},
		{"unknown", `{"source":"unknown","preview":"real-looking preview"}`, false},
		{"exec", `{"source":"exec"}`, false},
		{"app-server", `{"source":"appServer"}`, false},
		{"ephemeral", `{"source":"vscode","ephemeral":true}`, false},
		{"ambiguous-source", `{"source":{"custom":"atlas","subAgent":"review"}}`, false},
		{"malformed-label", `{"source":"vscode","threadSource":{}}`, false},
		{"missing-source", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var meta Thread
			if err := json.Unmarshal([]byte(tc.meta), &meta); err != nil {
				t.Fatal(err)
			}
			if got := meta.InteractiveMain(); got != tc.want {
				t.Fatalf("candidate=%v, want %v", got, tc.want)
			}
		})
	}
}
