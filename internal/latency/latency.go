// Package latency records content-free checkpoints for the local service log.
package latency

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Event contains only fixed stage/outcome labels, timings and hashed identities.
// It must never contain native payloads, error messages, titles or file paths.
type Event struct {
	AtMS       int64   `json:"at_ms"`
	Stage      string  `json:"stage"`
	ThreadRef  string  `json:"thread_ref,omitempty"`
	TurnRef    string  `json:"turn_ref,omitempty"`
	WorkRef    string  `json:"work_ref,omitempty"`
	DurationMS float64 `json:"duration_ms"`
	Outcome    string  `json:"outcome"`
}

// Observer may be called concurrently by discovery and independent actors.
type Observer func(Event)

// Record hashes each identity before it reaches the log sink. Stage and outcome
// must be labels chosen by the caller, never values copied from native content.
func (o Observer) Record(stage, host, thread, turn, work string, elapsed time.Duration, outcome string) {
	if o == nil {
		return
	}
	e := Event{AtMS: time.Now().UnixMilli(), Stage: stage, DurationMS: float64(max(0, elapsed).Microseconds()) / 1000, Outcome: outcome}
	if thread != "" {
		e.ThreadRef = reference("thread", host, thread)
		if turn != "" {
			e.TurnRef = reference("turn", host, thread, turn)
		}
		if work != "" {
			e.WorkRef = reference("work", host, thread, work)
		}
	}
	o(e)
}

func reference(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// Result deliberately discards error text and wrapped payloads.
func Result(err error) string {
	if err == nil {
		return "ok"
	}
	return "error"
}
