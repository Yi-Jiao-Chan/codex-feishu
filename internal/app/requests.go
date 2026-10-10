package app

import (
	"bytes"
	"codex-feishu/internal/codexclient"
	"encoding/json"
	"fmt"
)

type appRequest struct{ turnID string }

// Numeric and string IDs are distinct; preserve large integer IDs and normalize
// JSON string escaping. Generation prevents stale replies resolving new requests.
func appRequestKey(generation uint64, raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return fmt.Sprintf("%d:s:%s", generation, text)
	}
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		return fmt.Sprintf("%d:n:%d", generation, number)
	}
	return ""
}

// Track only ordinary requests whose owner can remain the subscribed App.
// No questions, command contents, permissions, or answers enter this state.
// Never reply, even with method-not-found: the first reply consumes the callback.
func (m *M0) trackAppRequest(event codexclient.Event) {
	var params struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(event.Params, &params) != nil || params.ThreadID != m.thread {
		return
	}
	if event.Method == "serverRequest/resolved" {
		delete(m.appRequests, appRequestKey(event.Generation, params.RequestID))
		return
	}
	switch event.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput":
	default:
		return
	}
	key := appRequestKey(event.Generation, event.RequestID)
	if key == "" || params.TurnID == "" {
		return
	}
	if m.appRequests == nil {
		m.appRequests = map[string]appRequest{}
	}
	m.appRequests[key] = appRequest{turnID: params.TurnID}
}

func (m *M0) waitingInApp(turnID string) bool {
	for _, request := range m.appRequests {
		if request.turnID == turnID {
			return true
		}
	}
	return false
}
