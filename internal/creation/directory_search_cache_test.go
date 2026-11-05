package creation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func cacheTestService(t *testing.T, scan directorySearchScan) (DirectoryService, *directorySearchCache, string) {
	t.Helper()
	c := newDirectorySearchCache(context.Background())
	if scan != nil {
		c.scan = scan
	}
	t.Cleanup(c.Close)
	root := t.TempDir()
	return DirectoryService{Roots: []string{root}, searchCache: c}, c, root
}

func waitDirectoryCacheIdle(t *testing.T, c *directorySearchCache) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.inflight)
		c.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("directory cache did not become idle")
		case <-time.After(time.Millisecond):
		}
	}
}

func cacheTestSnapshot(root string, count int) directorySearchSnapshot {
	s := directorySearchSnapshot{Path: root}
	for i := 0; i < count; i++ {
		s.Choices = append(s.Choices, DirectoryChoice{Path: filepath.Join(root, fmt.Sprintf("match-%03d", i)), Label: "match"})
	}
	return s
}

func TestDirectorySearchCacheRefreshesFilesystemChanges(t *testing.T) {
	s, c, root := cacheTestService(t, nil)
	old := directory(t, root, "match-old")
	page, err := s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil || len(page.Choices) != 1 || page.Choices[0].Path != old {
		t.Fatal("initial filesystem result was not cached", err)
	}
	added := directory(t, root, "match-new")
	page, err = s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil || len(page.Choices) != 1 || page.Choices[0].Path != old {
		t.Fatal("cache hit did not return its existing complete snapshot", err)
	}
	waitDirectoryCacheIdle(t, c)
	page, err = s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil || len(page.Choices) != 2 || page.Choices[0].Path != added || page.Choices[1].Path != old {
		t.Fatal("next search did not include the newly discovered directory", err)
	}
	waitDirectoryCacheIdle(t, c)
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	page, err = s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil || len(page.Choices) != 2 {
		t.Fatal("cache hit unexpectedly discarded its last complete result", err)
	}
	waitDirectoryCacheIdle(t, c)
	page, err = s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil || len(page.Choices) != 1 || page.Choices[0].Path != added {
		t.Fatal("next search retained the deleted directory after refresh", err)
	}
}

func TestDirectorySearchCacheFullSnapshotPagingAndRefresh(t *testing.T) {
	started, release := make(chan struct{}, 4), make(chan struct{})
	var foreground atomic.Int32
	s, c, root := cacheTestService(t, func(ctx context.Context, s DirectoryService, roots []string, query string, workers int) (directorySearchSnapshot, error) {
		if query != "match" {
			t.Error("scan did not receive normalized query")
		}
		if workers == directorySearchWorkers {
			foreground.Add(1)
			return cacheTestSnapshot(roots[0], 105), nil
		}
		started <- struct{}{}
		select {
		case <-release:
			return cacheTestSnapshot(roots[0], 106), nil
		case <-ctx.Done():
			return directorySearchSnapshot{}, ctx.Err()
		}
	})
	page, err := s.SearchRecursive(context.Background(), root, " MaTcH ", "", 100)
	if err != nil || page.Total != 105 || len(page.Choices) != 100 || page.Next == "" {
		t.Fatal("initial complete scan was not cached", page, err)
	}
	after := page.Next
	page.Choices[0].Path = "/mutated-caller-copy"
	page, err = s.SearchRecursive(context.Background(), root, "MATCH", after, 100)
	if err != nil || page.Total != 105 || page.Offset != 100 || len(page.Choices) != 5 || page.Next != "" {
		t.Fatal("cache retained only first page or waited for refresh", page, err)
	}
	<-started
	first, err := s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil || first.Choices[0].Path != filepath.Join(root, "match-000") || foreground.Load() != 1 {
		t.Fatal("caller mutated immutable snapshot or equivalent query rescanned", err)
	}
	close(release)
	waitDirectoryCacheIdle(t, c)
	page, err = s.SearchRecursive(context.Background(), root, "match", after, 100)
	if err != nil || page.Total != 106 || len(page.Choices) != 6 || foreground.Load() != 1 {
		t.Fatal("changed background snapshot was not used by next request", page, err)
	}
}

func TestDirectorySearchCacheTimeoutDeduplicatesForegroundAndBackground(t *testing.T) {
	foregroundStarted, backgroundStarted, release := make(chan struct{}), make(chan struct{}, 4), make(chan struct{})
	var foreground, background atomic.Int32
	s, c, root := cacheTestService(t, func(ctx context.Context, s DirectoryService, roots []string, query string, workers int) (directorySearchSnapshot, error) {
		if workers == directorySearchWorkers {
			foreground.Add(1)
			close(foregroundStarted)
			<-ctx.Done()
			return directorySearchSnapshot{}, ctx.Err()
		}
		background.Add(1)
		deadline, ok := ctx.Deadline()
		if workers != 2 || !ok || time.Until(deadline) > 30*time.Second || time.Until(deadline) < 28*time.Second {
			t.Error("background scan did not receive two workers and 30-second budget")
		}
		backgroundStarted <- struct{}{}
		select {
		case <-release:
			return directorySearchSnapshot{Path: roots[0]}, nil
		case <-ctx.Done():
			return directorySearchSnapshot{}, ctx.Err()
		}
	})
	budget, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := c.search(budget, context.Background(), s, []string{root}, "match", "", 100)
		finished <- err
	}()
	<-foregroundStarted
	if _, err := s.SearchRecursive(context.Background(), root, "match", "", 100); !errors.Is(err, ErrDirectorySearchPending) {
		t.Fatal("same-key foreground scan was duplicated", err)
	}
	if err := <-finished; !errors.Is(err, ErrDirectorySearchPending) {
		t.Fatal("foreground budget exhaustion did not queue background", err)
	}
	<-backgroundStarted
	for i := 0; i < 3; i++ {
		if _, err := s.SearchRecursive(context.Background(), root, "match", "", 100); !errors.Is(err, ErrDirectorySearchPending) {
			t.Fatal("same-key background scan was duplicated", err)
		}
	}
	if foreground.Load() != 1 || background.Load() != 1 {
		t.Fatal("duplicate foreground/background scans", foreground.Load(), background.Load())
	}
	close(release)
	waitDirectoryCacheIdle(t, c)
	page, err := s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil || page.Total != 0 || len(page.Choices) != 0 || foreground.Load() != 1 {
		t.Fatal("successful empty background result was not cached", page, err)
	}
}

func TestDirectorySearchCacheKeyIsolationAndRootRevalidation(t *testing.T) {
	var foreground atomic.Int32
	s, _, root := cacheTestService(t, func(ctx context.Context, s DirectoryService, roots []string, query string, workers int) (directorySearchSnapshot, error) {
		if workers == directorySearchWorkers {
			foreground.Add(1)
			return directorySearchSnapshot{Path: roots[0]}, nil
		}
		<-ctx.Done()
		return directorySearchSnapshot{}, ctx.Err()
	})
	parent := directory(t, root, "parent")
	a, b := directory(t, parent, "a"), directory(t, parent, "b")
	alias := filepath.Join(parent, "visible-alias")
	if err := os.Symlink(a, alias); err != nil {
		t.Fatal(err)
	}
	s.Roots = []string{parent}
	for i, test := range []struct {
		root, query string
		depth       int
		scope       []string
	}{
		{a, "match", 0, []string{parent}},
		{a, "other", 0, []string{parent}},
		{b, "match", 0, []string{parent}},
		{a, "match", defaultDirectorySearchDepth + 1, []string{parent}},
		{a, "match", 0, []string{root}},
		{alias, "match", 0, []string{parent}},
	} {
		x := s
		x.Roots, x.SearchDepth = test.scope, test.depth
		if _, err := x.SearchRecursive(context.Background(), test.root, test.query, "", 100); err != nil || foreground.Load() != int32(i+1) {
			t.Fatal("root/query/depth/scope identities shared a stale snapshot", i, err)
		}
	}
	x := s
	x.SearchDepth = defaultDirectorySearchDepth
	if _, err := x.SearchRecursive(context.Background(), a, " MATCH ", "", 100); err != nil || foreground.Load() != 6 {
		t.Fatal("equivalent query/default depth missed complete cache", err)
	}
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SearchRecursive(context.Background(), a, "match", "", 100); !errors.Is(err, ErrDirectoryUnavailable) || foreground.Load() != 6 {
		t.Fatal("cached result bypassed root existence/access check", err)
	}
}

func TestDirectorySearchCacheFailedAndEqualRefreshKeepsSnapshot(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	var count atomic.Int32
	count.Store(1)
	s, c, root := cacheTestService(t, func(ctx context.Context, s DirectoryService, roots []string, query string, workers int) (directorySearchSnapshot, error) {
		calls.Add(1)
		if workers == directorySearchBackgroundWorkers && fail.Load() {
			return directorySearchSnapshot{}, ErrDirectoryUnavailable
		}
		return cacheTestSnapshot(roots[0], int(count.Load())), nil
	})
	_, err := s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := directorySearchKey(context.Background(), s, []string{root}, "match")
	c.mu.Lock()
	original := c.entries[key].snapshot.Choices
	c.mu.Unlock()
	_, err = s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	waitDirectoryCacheIdle(t, c)
	c.mu.Lock()
	equal := &original[0] == &c.entries[key].snapshot.Choices[0]
	c.mu.Unlock()
	if !equal {
		t.Fatal("equal complete refresh needlessly replaced immutable snapshot")
	}
	fail.Store(true)
	_, err = s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	waitDirectoryCacheIdle(t, c)
	c.mu.Lock()
	retained := c.entries[key].snapshot
	c.mu.Unlock()
	if !reflect.DeepEqual(retained, cacheTestSnapshot(root, 1)) || calls.Load() != 3 {
		t.Fatal("failed refresh discarded old complete results", calls.Load())
	}
	fail.Store(false)
	count.Store(0)
	page, err := s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil || page.Total != 1 {
		t.Fatal("refresh blocked the existing result", page, err)
	}
	waitDirectoryCacheIdle(t, c)
	page, err = s.SearchRecursive(context.Background(), root, "match", "", 100)
	if err != nil || page.Total != 0 || len(page.Choices) != 0 {
		t.Fatal("complete empty refresh failed to replace stale nonempty results", page, err)
	}
}

func TestDirectorySearchCacheQueueBoundAndClose(t *testing.T) {
	started := make(chan struct{})
	var calls, active, maximum atomic.Int32
	var foreground atomic.Int32
	s, c, root := cacheTestService(t, func(ctx context.Context, s DirectoryService, roots []string, query string, workers int) (directorySearchSnapshot, error) {
		if workers == directorySearchWorkers {
			foreground.Add(1)
			return directorySearchSnapshot{}, context.DeadlineExceeded
		}
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		maximum.Store(n)
		if workers != directorySearchBackgroundWorkers {
			t.Error("queued scan used foreground workers")
		}
		close(started)
		<-ctx.Done()
		return directorySearchSnapshot{}, ctx.Err()
	})
	job := func(query string) *directorySearchJob {
		key, err := directorySearchKey(context.Background(), s, []string{root}, query)
		if err != nil {
			t.Fatal(err)
		}
		return &directorySearchJob{key: key, query: query, roots: []string{root}, service: s}
	}
	first := job("active")
	c.mu.Lock()
	c.enqueueLocked(first)
	c.mu.Unlock()
	<-started
	for i := 0; i < directorySearchQueueSize; i++ {
		j := job(fmt.Sprint(i))
		c.mu.Lock()
		accepted := c.enqueueLocked(j)
		c.mu.Unlock()
		if !accepted {
			t.Fatal("bounded queue rejected an available slot")
		}
	}
	rejected := job("overflow")
	c.mu.Lock()
	accepted := c.enqueueLocked(rejected)
	inflight := c.inflight[rejected.key]
	c.mu.Unlock()
	if accepted || inflight != nil {
		t.Fatal("full queue left a permanent pending marker")
	}
	for i := 0; i < 2; i++ {
		if _, err := s.SearchRecursive(context.Background(), root, "overflow", "", 100); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("miss without background queue admission returned permanent pending", err)
		}
	}
	if foreground.Load() != 2 {
		t.Fatal("failed queue admission prevented the next miss from retrying")
	}
	cached := job("cached")
	c.mu.Lock()
	c.storeLocked(cached.key, cacheTestSnapshot(root, 1))
	c.mu.Unlock()
	page, err := s.SearchRecursive(context.Background(), root, "cached", "", 100)
	if err != nil || page.Total != 1 {
		t.Fatal("full refresh queue prevented cached results from returning", page, err)
	}
	c.Close()
	if active.Load() != 0 || calls.Load() != 1 || maximum.Load() != 1 {
		t.Fatal("background scans overlapped or survived Close")
	}
	if _, err := s.SearchRecursive(context.Background(), root, "overflow", "", 100); !errors.Is(err, context.Canceled) {
		t.Fatal("closed cache accepted new work", err)
	}
}

func TestDirectorySearchCacheCloseCancelsAndWaitsForForeground(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	s, c, root := cacheTestService(t, func(ctx context.Context, s DirectoryService, roots []string, query string, workers int) (directorySearchSnapshot, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return directorySearchSnapshot{}, ctx.Err()
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	finished := make(chan error, 1)
	go func() {
		_, err := s.SearchRecursive(context.Background(), root, "match", "", 100)
		finished <- err
	}()
	<-started
	if _, err := s.SearchRecursive(context.Background(), root, "match", "", 100); !errors.Is(err, ErrDirectorySearchPending) {
		t.Fatal("same-key request did not share the foreground flight", err)
	}
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	<-canceled
	select {
	case <-closed:
		t.Fatal("Close returned before foreground scanner exited")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	<-closed
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal("cache shutdown did not cancel foreground owner", err)
	}
	if _, err := s.SearchRecursive(context.Background(), root, "match", "", 100); !errors.Is(err, context.Canceled) {
		t.Fatal("same-key lookup remained pending after shutdown", err)
	}
}

func TestDirectorySearchCacheCallerDeadlineDoesNotQueueBackground(t *testing.T) {
	var calls atomic.Int32
	s, c, root := cacheTestService(t, func(ctx context.Context, s DirectoryService, roots []string, query string, workers int) (directorySearchSnapshot, error) {
		calls.Add(1)
		<-ctx.Done()
		return directorySearchSnapshot{}, ctx.Err()
	})
	caller, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.SearchRecursive(caller, root, "match", "", 100); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("caller deadline was replaced by independent pending work", err)
	}
	waitDirectoryCacheIdle(t, c)
	if calls.Load() != 1 {
		t.Fatal("caller deadline started a background scan")
	}
}

func TestDirectorySearchCacheCallerCancellationAndShutdownRace(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	s, c, root := cacheTestService(t, func(ctx context.Context, s DirectoryService, roots []string, query string, workers int) (directorySearchSnapshot, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return directorySearchSnapshot{}, ctx.Err()
	})
	caller, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := s.SearchRecursive(caller, root, "match", "", 100)
		finished <- err
	}()
	<-started
	if _, err := s.SearchRecursive(context.Background(), root, "match", "", 100); !errors.Is(err, ErrDirectorySearchPending) {
		t.Fatal("same-key waiter started another foreground scan", err)
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation became independent background work", err)
	}
	waitDirectoryCacheIdle(t, c)
	if calls.Load() != 1 {
		t.Fatal("caller cancellation queued a background scan")
	}
	var attempts sync.WaitGroup
	for i := 0; i < 20; i++ {
		attempts.Add(1)
		go func(i int) {
			defer attempts.Done()
			j := &directorySearchJob{key: fmt.Sprint(i)}
			c.mu.Lock()
			c.enqueueLocked(j)
			c.mu.Unlock()
		}(i)
	}
	c.Close()
	attempts.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.inflight) != 0 || len(c.queue) != 0 {
		t.Fatal("enqueue/shutdown race retained pending jobs")
	}
}

func TestDirectorySearchCacheCapacityAndOversizedReplacement(t *testing.T) {
	_, c, root := cacheTestService(t, nil)
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := 0; i <= directorySearchCacheEntries; i++ {
		c.storeLocked(fmt.Sprint(i), cacheTestSnapshot(root, 1))
	}
	if len(c.entries) != directorySearchCacheEntries {
		t.Fatal("cache entry count is unbounded")
	}
	if _, ok := c.entries["0"]; ok {
		t.Fatal("oldest cache entry was not evicted")
	}
	key := "1"
	large := cacheTestSnapshot(root, 65)
	label := strings.Repeat("x", 1<<20)
	for i := range large.Choices {
		large.Choices[i].Label = label
	}
	c.storeLocked(key, large)
	if _, ok := c.entries[key]; ok {
		t.Fatal("oversized successful result retained stale same-key snapshot")
	}
	if c.bytes > directorySearchCacheBytes {
		t.Fatal("cache byte weight is unbounded")
	}
}
