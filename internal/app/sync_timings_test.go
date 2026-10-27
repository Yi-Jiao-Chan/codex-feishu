package app

import (
	"sync"
	"testing"
	"time"
)

func TestSyncTimingsConcurrentSnapshotsAreIndependent(t *testing.T) {
	var metrics syncTimings
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				metrics.observe("projection", 2*time.Millisecond)
				metrics.reusedHistory()
				metrics.ignoredReasoning()
				_ = metrics.snapshot()
			}
		}()
	}
	wg.Wait()
	snapshot := metrics.snapshot()
	stage := snapshot.Stages["projection"]
	if snapshot.HistoryReuses != 800 || snapshot.IgnoredReasoningEvents != 800 || stage.Count != 800 || stage.TotalMS != 1600 || stage.MaxMS != 2 || stage.LastMS != 2 {
		t.Fatal("concurrent aggregate lost measurements", snapshot)
	}
	delete(snapshot.Stages, "projection")
	if metrics.snapshot().Stages["projection"].Count != 800 {
		t.Fatal("diagnostic snapshot shared mutable state")
	}
	metrics.reset()
	if got := metrics.snapshot(); got.HistoryReuses != 0 || got.IgnoredReasoningEvents != 0 || len(got.Stages) != 0 {
		t.Fatal("native generation retained old timing samples", got)
	}
}
