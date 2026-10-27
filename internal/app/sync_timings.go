package app

import (
	"sync"
	"time"

	"codex-feishu/internal/control"
)

// Only aggregate timings cross the local control socket. No route, payload,
// file path or native error is retained in diagnostics.
type syncTimings struct {
	mu      sync.Mutex
	stages  map[string]control.SyncDuration
	reused  uint64
	ignored uint64
}

func (s *syncTimings) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stages = nil
	s.reused = 0
	s.ignored = 0
}

func (s *syncTimings) observe(stage string, elapsed time.Duration) {
	ms := float64(elapsed.Microseconds()) / 1000
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stages == nil {
		s.stages = make(map[string]control.SyncDuration)
	}
	v := s.stages[stage]
	v.Count++
	v.LastMS = ms
	v.TotalMS += ms
	if ms > v.MaxMS {
		v.MaxMS = ms
	}
	s.stages[stage] = v
}

func (s *syncTimings) reusedHistory() {
	s.mu.Lock()
	s.reused++
	s.mu.Unlock()
}

func (s *syncTimings) ignoredReasoning() {
	s.mu.Lock()
	s.ignored++
	s.mu.Unlock()
}

func (s *syncTimings) snapshot() *control.SyncTimings {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := &control.SyncTimings{HistoryReuses: s.reused, IgnoredReasoningEvents: s.ignored, Stages: make(map[string]control.SyncDuration, len(s.stages))}
	for stage, value := range s.stages {
		result.Stages[stage] = value
	}
	return result
}
