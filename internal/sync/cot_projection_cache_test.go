package sync

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
)

func cachedCOTDocument(t *testing.T, cache *COTProjectionCache, item codexclient.Item, version int) string {
	t.Helper()
	raw, err := cotDocumentVersionWithCache("thread", codexclient.Turn{
		ID: "turn", Status: "completed", ItemsView: "full", Items: []codexclient.Item{item},
	}, map[string]int64{item.ID: 1000}, 2000, version, cache)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func benchmarkCOTHistory() codexclient.Turn {
	turn := codexclient.Turn{ID: "turn", Status: "completed", ItemsView: "full"}
	for n := range 256 {
		output := strings.Repeat("fixture result line\n", 120) + "token=synthetic_secret"
		turn.Items = append(turn.Items, codexclient.Item{
			ID: fmt.Sprintf("tool-%d", n), Type: "commandExecution", Status: "completed",
			Command: "go test ./...", AggregatedOutput: &output,
		})
	}
	return turn
}

func BenchmarkCOTProjectionCacheHistory(b *testing.B) {
	turn := benchmarkCOTHistory()
	b.Run("cold", func(b *testing.B) {
		for range b.N {
			if _, err := cotDocumentVersionWithCache("thread", turn, nil, 1000, cot.Version, newCOTProjectionCache(32<<20, 8192)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("warm", func(b *testing.B) {
		cache := newCOTProjectionCache(32<<20, 8192)
		if _, err := cotDocumentVersionWithCache("thread", turn, nil, 1000, cot.Version, cache); err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for range b.N {
			if _, err := cotDocumentVersionWithCache("thread", turn, nil, 1000, cot.Version, cache); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestCOTProjectionCacheReusesSafeProjectionAndInvalidatesCorrection(t *testing.T) {
	cache := newCOTProjectionCache(1<<20, 16)
	firstOutput := "matched 3 files\napi_key=synthetic_first_secret"
	item := codexclient.Item{ID: "tool", Type: "commandExecution", Status: "completed", Command: "go test ./...", AggregatedOutput: &firstOutput}
	first := cachedCOTDocument(t, cache, item, cot.Version)
	if strings.Contains(first, "synthetic_first_secret") || !strings.Contains(first, "matched 3 files") {
		t.Fatal("cold projection was not safely redacted")
	}
	if stats := cache.stats(); stats.entries != 1 || stats.misses != 1 || stats.hits != 0 {
		t.Fatalf("unexpected cold cache state: %+v", stats)
	}
	if again := cachedCOTDocument(t, cache, item, cot.Version); again != first {
		t.Fatal("cache hit changed the public document")
	}
	if stats := cache.stats(); stats.entries != 1 || stats.misses != 1 || stats.hits != 1 {
		t.Fatalf("unchanged item did not hit cache: %+v", stats)
	}

	correctedOutput := "matched 4 files\napi_key=synthetic_second_secret"
	item.AggregatedOutput = &correctedOutput
	corrected := cachedCOTDocument(t, cache, item, cot.Version)
	if corrected == first || !strings.Contains(corrected, "matched 4 files") || strings.Contains(corrected, "synthetic_second_secret") {
		t.Fatal("same-ID result correction did not replace the cached projection")
	}
	if stats := cache.stats(); stats.entries != 1 || stats.misses != 2 || stats.hits != 1 {
		t.Fatalf("correction did not replace one cache entry: %+v", stats)
	}
	legacy := cachedCOTDocument(t, cache, item, cot.LegacyVersion)
	if strings.Contains(legacy, "input_preview") || strings.Contains(legacy, "output_preview") {
		t.Fatal("projection versions shared cached V2 fields")
	}
	if stats := cache.stats(); stats.entries != 2 || stats.misses != 3 {
		t.Fatalf("projection version was not isolated: %+v", stats)
	}
}

func TestCOTProjectionCacheEnforcesEntryAndByteLimits(t *testing.T) {
	cache := newCOTProjectionCache(1<<20, 2)
	for n, id := range []string{"one", "two", "three"} {
		output := strings.Repeat(string(rune('a'+n)), 200)
		cachedCOTDocument(t, cache, codexclient.Item{ID: id, Type: "commandExecution", Status: "completed", Command: "go test ./...", AggregatedOutput: &output}, cot.Version)
	}
	if stats := cache.stats(); stats.entries != 2 || stats.evictions != 1 || stats.bytes > cache.maxBytes {
		t.Fatalf("entry-bounded LRU was not enforced: %+v", stats)
	}

	tooSmall := newCOTProjectionCache(400, 8)
	output := strings.Repeat("public output ", 100)
	cachedCOTDocument(t, tooSmall, codexclient.Item{ID: "oversized", Type: "commandExecution", Status: "completed", Command: "go test ./...", AggregatedOutput: &output}, cot.Version)
	if stats := tooSmall.stats(); stats.entries != 0 || stats.bytes != 0 {
		t.Fatalf("oversized projection entered byte-bounded cache: %+v", stats)
	}
}

func TestCOTProjectionCacheConcurrentUseStaysBounded(t *testing.T) {
	cache := newCOTProjectionCache(1<<20, 32)
	var workers sync.WaitGroup
	for worker := range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for n := range 40 {
				output := "synthetic output"
				item := codexclient.Item{ID: string(rune('a'+worker)) + string(rune('A'+n%20)), Type: "commandExecution", Status: "completed", Command: "go test ./...", AggregatedOutput: &output}
				if _, err := cotDocumentVersionWithCache("thread", codexclient.Turn{ID: "turn", Status: "completed", ItemsView: "full", Items: []codexclient.Item{item}}, nil, 1000, cot.Version, cache); err != nil {
					t.Errorf("concurrent projection failed: %v", err)
					return
				}
			}
		}()
	}
	workers.Wait()
	if stats := cache.stats(); stats.entries > cache.maxEntries || stats.bytes > cache.maxBytes {
		t.Fatalf("concurrent cache exceeded limits: %+v", stats)
	}
}

func TestEngineApplyKeepsProjectionCacheAcrossSnapshots(t *testing.T) {
	e, _ := engineFixture(t)
	e.NativeCOT = true
	e.COTProjectionCache = newCOTProjectionCache(1<<20, 16)
	output := "public result"
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{
		input("user", "synthetic request"),
		{ID: "tool", Type: "commandExecution", Status: "completed", Command: "go test ./...", AggregatedOutput: &output},
	}}
	if err := e.Apply(context.Background(), "thread", []codexclient.Turn{turn}, false, 1000); err != nil {
		t.Fatal(err)
	}
	if stats := e.COTProjectionCache.stats(); stats.misses != 1 || stats.hits != 0 {
		t.Fatalf("unexpected first snapshot cache state: %+v", stats)
	}
	if err := e.Apply(context.Background(), "thread", []codexclient.Turn{turn}, false, 2000); err != nil {
		t.Fatal(err)
	}
	if stats := e.COTProjectionCache.stats(); stats.misses != 1 || stats.hits != 1 {
		t.Fatalf("second snapshot did not reuse the projection: %+v", stats)
	}
	output = "corrected public result"
	turn.Items[1].AggregatedOutput = &output
	if err := e.Apply(context.Background(), "thread", []codexclient.Turn{turn}, false, 3000); err != nil {
		t.Fatal(err)
	}
	if stats := e.COTProjectionCache.stats(); stats.misses != 2 || stats.hits != 1 || stats.entries != 1 {
		t.Fatalf("same-ID correction did not invalidate one entry: %+v", stats)
	}
}
