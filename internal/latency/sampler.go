package latency

import (
	"strings"
	"time"
)

const sampleWindow = 5 * time.Second
const maxSampleKeys = 4096

// Sampler bounds repeated setup diagnostics during failed activation loops.
// The sink must serialize Allow calls. Native messages, committed documents and
// every delivery operation always pass through, including multiple per second.
type Sampler struct {
	window     time.Time
	seen       map[string]bool
	suppressed uint64
}

// Allow returns the decision and the prior window's omitted count, if it ended.
// Keys contain only already-hashed references and fixed diagnostic labels.
func (s *Sampler) Allow(event Event, now time.Time) (allow bool, omitted uint64) {
	if s.window.IsZero() || now.Sub(s.window) >= sampleWindow {
		omitted, s.suppressed = s.suppressed, 0
		s.window = now
		clear(s.seen)
	}
	setup := event.Stage == "actor.started" || event.Stage == "actor.workspace" || strings.HasPrefix(event.Stage, "actor.attach") ||
		strings.HasPrefix(event.Stage, "discovery.candidate") || event.Stage == "discovery.metadata" || event.Stage == "discovery.scope" || event.Stage == "discovery.history"
	if !setup {
		return true, omitted
	}
	key := event.ThreadRef + "\x00" + event.Stage + "\x00" + event.Outcome
	if s.seen[key] || len(s.seen) >= maxSampleKeys {
		s.suppressed++
		return false, omitted
	}
	if s.seen == nil {
		s.seen = make(map[string]bool)
	}
	s.seen[key] = true
	return true, omitted
}
