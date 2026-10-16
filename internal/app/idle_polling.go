package app

import (
	"time"

	"codex-feishu/internal/codexclient"
)

// Unknown activity and unfinished work stay on the normal reconciliation path.
// Only a successfully read, old and idle thread can reduce native polling.
func idleThread(meta codexclient.Thread, turns []codexclient.Turn, now time.Time) bool {
	if meta.UpdatedAt <= 0 || meta.UpdatedAt >= now.Add(-recentActivityWindow).Unix() {
		return false
	}
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
