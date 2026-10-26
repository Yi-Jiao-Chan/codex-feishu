package app

import "codex-feishu/internal/codexclient"

// Only a complete terminal history with an idle native status can release its
// actor. Durable work checks in the runtime remain authoritative.
func settledThread(meta codexclient.Thread, turns []codexclient.Turn) bool {
	if meta.Status.Type != "idle" && meta.Status.Type != "notLoaded" {
		return false
	}
	for _, turn := range turns {
		switch turn.Status {
		case "completed", "failed", "interrupted":
		default:
			return false
		}
	}
	return true
}
