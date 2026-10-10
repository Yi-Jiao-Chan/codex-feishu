package app

import (
	"codex-feishu/internal/codexclient"
	"encoding/json"
	"testing"
)

func TestM0RequiresMarkerInFirstActualUserInput(t *testing.T) {
	marked := codexclient.Item{Type: "userMessage", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"M0_APP_ORIGIN_V2_20261010: isolated test"}`)}}
	unmarked := codexclient.Item{Type: "userMessage", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"ordinary work"}`)}}
	fake := codexclient.Item{Type: "agentMessage", Text: "M0_APP_ORIGIN_V2_20261010"}
	for _, tc := range []struct {
		name  string
		items []codexclient.Item
		want  bool
	}{
		{"draft", nil, false},
		{"agent-output-is-not-input", []codexclient.Item{fake}, false},
		{"real-input-after-system-item", []codexclient.Item{{Type: "reasoning"}, marked}, true},
		{"marker-in-later-input-does-not-authorize-existing-work", []codexclient.Item{unmarked, marked}, false},
		{"agent-marker-before-ordinary-user", []codexclient.Item{fake, unmarked}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := m0MarkedInput([]codexclient.Turn{{Items: tc.items}}); got != tc.want {
				t.Fatalf("marked first input=%v, want %v", got, tc.want)
			}
		})
	}
}
