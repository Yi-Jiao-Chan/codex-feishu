package codexclient

import "encoding/json"

// InteractiveMain identifies known interactive source candidates, not App UI
// provenance or activation. Callers must separately verify cwd and user input.
// A human fork is independent; forkedFromId does not make it a subagent.
func (t Thread) InteractiveMain() bool {
	if t.Ephemeral || t.ParentThreadID != nil {
		return false
	}
	var label string
	if len(t.ThreadSource) != 0 && string(t.ThreadSource) != "null" {
		if json.Unmarshal(t.ThreadSource, &label) != nil {
			return false
		}
		switch label {
		case "subagent", "guardian_review", "memory_consolidation", "feishu.bridge.validation", "feishu.bridge.creation":
			return false
		}
	}
	var source string
	if json.Unmarshal(t.Source, &source) == nil {
		return source == "cli" || source == "vscode"
	}
	// Decode the v2 tagged enum strictly; unknown/new variants stay excluded.
	var tags map[string]json.RawMessage
	if json.Unmarshal(t.Source, &tags) != nil || len(tags) != 1 {
		return false
	}
	var custom string
	if json.Unmarshal(tags["custom"], &custom) != nil {
		return false
	}
	return custom == "atlas" || custom == "chatgpt"
}
