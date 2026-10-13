package store

import (
	"context"
	"testing"
)

func TestDueMirrorsHonorDeferralAndExcludeUnresolvedAttempts(t *testing.T) {
	ctx := context.Background()
	for _, outcome := range []string{"dispatching", "unknown", "deferred"} {
		t.Run(outcome, func(t *testing.T) {
			s, _, a := retryFixture(t)
			switch outcome {
			case "unknown":
				if err := s.FinishMirrorAttempt(ctx, a.ID, "unknown", ""); err != nil {
					t.Fatal(err)
				}
			case "deferred":
				if err := s.DeferMirrorAttempt(ctx, a.ID, 1000, 1000); err != nil {
					t.Fatal(err)
				}
			}
			for _, at := range []int64{1999, 2000, 5000} {
				due, err := s.HasDueMirrors(ctx, "host", "thread", at, 100)
				if err != nil || due != (outcome == "deferred" && at >= 2000) {
					t.Fatalf("due hint crossed journal/deadline boundary: outcome=%s at=%d due=%v err=%v", outcome, at, due, err)
				}
			}
		})
	}
}

func TestDueProgressWaitsForIntervalButTerminalDoesNot(t *testing.T) {
	ctx := context.Background()
	s, _ := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	m := mirrorDoc("progress", "progress", "starting")
	m.ParentKey, m.ParentMessageID = "", "input"
	stage(t, s, 100, m)
	a := claimMirror(t, s, m.Key, 1000)
	if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "original-progress"); err != nil {
		t.Fatal(err)
	}
	m.Card = `{"body":"latest progress"}`
	stage(t, s, 101, m)
	for _, interval := range []int64{0, 100, 10000} {
		delay := interval
		if delay == 0 {
			delay = 2000
		}
		for _, at := range []int64{1000 + delay - 1, 1000 + delay} {
			due, err := s.HasDueMirrors(ctx, "host", "thread", at, interval)
			if err != nil || due != (at == 1000+delay) {
				t.Fatalf("progress deadline: interval=%d at=%d due=%v err=%v", interval, at, due, err)
			}
		}
	}
	m.Terminal = true
	stage(t, s, 102, m)
	if due, err := s.HasDueMirrors(ctx, "host", "thread", 1001, 10000); err != nil || !due {
		t.Fatal("terminal progress waited for generating interval", due, err)
	}
}
