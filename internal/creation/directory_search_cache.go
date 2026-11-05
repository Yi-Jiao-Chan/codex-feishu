package creation

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"codex-feishu/internal/rules"
)

const (
	directorySearchCacheEntries      = 32
	directorySearchCacheBytes        = 64 << 20
	directorySearchQueueSize         = 16
	directorySearchBackgroundTimeout = 30 * time.Second
	directorySearchBackgroundWorkers = 2
)

// Snapshots contain the complete sorted result, never a page or partial scan.
// Stored snapshots are immutable; page returns its own choice slice.
type directorySearchSnapshot struct {
	Path, Parent string
	ReadOnly     bool
	Choices      []DirectoryChoice
}

func (s directorySearchSnapshot) page(ctx context.Context, after string, limit int) (DirectoryPage, error) {
	if err := ctx.Err(); err != nil {
		return DirectoryPage{}, err
	}
	offset := sort.Search(len(s.Choices), func(i int) bool { return s.Choices[i].Path > after })
	end := min(offset+directoryLimit(limit), len(s.Choices))
	page := DirectoryPage{Path: s.Path, Parent: s.Parent, ReadOnly: s.ReadOnly, Total: len(s.Choices), Offset: offset, Choices: append([]DirectoryChoice{}, s.Choices[offset:end]...)}
	if end < len(s.Choices) {
		page.Next = s.Choices[end-1].Path
	}
	if err := ctx.Err(); err != nil {
		return DirectoryPage{}, err
	}
	return page, nil
}

type directorySearchScan func(context.Context, DirectoryService, []string, string, int) (directorySearchSnapshot, error)

type directorySearchJob struct {
	key, query string
	service    DirectoryService
	roots      []string
}

type directorySearchCacheEntry struct {
	snapshot directorySearchSnapshot
	stamp    uint64
	bytes    int
}

type directorySearchCache struct {
	ctx    context.Context
	cancel context.CancelFunc
	scan   directorySearchScan

	mu       sync.Mutex
	closed   bool
	stamp    uint64
	bytes    int
	entries  map[string]directorySearchCacheEntry
	inflight map[string]*directorySearchJob
	queue    []*directorySearchJob
	wake     chan struct{}
	workers  sync.WaitGroup
}

func newDirectorySearchCache(ctx context.Context) *directorySearchCache {
	ctx, cancel := context.WithCancel(ctx)
	c := &directorySearchCache{
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1),
		entries: make(map[string]directorySearchCacheEntry), inflight: make(map[string]*directorySearchJob),
		scan: func(ctx context.Context, s DirectoryService, roots []string, query string, workers int) (directorySearchSnapshot, error) {
			return s.collectRecursiveDirectories(ctx, roots, query, workers)
		},
	}
	c.workers.Add(1)
	go c.run()
	return c
}

func (c *directorySearchCache) Close() {
	c.mu.Lock()
	c.closed = true
	c.cancel()
	c.queue = nil
	c.mu.Unlock()
	c.workers.Wait()
	c.mu.Lock()
	c.entries, c.inflight, c.queue = nil, nil, nil
	c.bytes = 0
	c.mu.Unlock()
}

// Include both logical aliases and their current physical identities. Each
// lookup rechecks root access before it can reuse results from an earlier scan.
func directorySearchKey(ctx context.Context, s DirectoryService, roots []string, query string) (string, error) {
	if len(roots) == 0 || s.SearchDepth < 0 || strings.IndexFunc(query, unicode.IsControl) >= 0 {
		return "", ErrDirectoryUnavailable
	}
	type identity struct {
		Logical, Canonical string
		ReadOnly           bool
	}
	key := struct {
		Roots, Scope []identity
		Query        string
		Depth        int
	}{Query: strings.ToLower(strings.TrimSpace(query)), Depth: s.SearchDepth}
	if key.Depth == 0 {
		key.Depth = defaultDirectorySearchDepth
	}
	for _, root := range roots {
		base, err := s.Browse(ctx, root)
		if err != nil {
			return "", err
		}
		key.Roots = append(key.Roots, identity{filepath.Clean(root), base.Path, base.ReadOnly})
	}
	for _, root := range s.Roots {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		real, _ := rules.CanonicalDirectory(root)
		key.Scope = append(key.Scope, identity{Logical: root, Canonical: real})
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(key)
	return string(raw), nil
}

func (c *directorySearchCache) search(ctx, callerCtx context.Context, s DirectoryService, roots []string, query, after string, limit int) (DirectoryPage, error) {
	key, err := directorySearchKey(ctx, s, roots, query)
	if err != nil {
		return DirectoryPage{}, err
	}
	s.Roots = slices.Clone(s.Roots)
	s.searchCache = nil
	job := &directorySearchJob{key: key, query: strings.ToLower(strings.TrimSpace(query)), service: s, roots: slices.Clone(roots)}
	c.mu.Lock()
	if c.closed || c.ctx.Err() != nil {
		c.mu.Unlock()
		return DirectoryPage{}, context.Canceled
	}
	if entry, ok := c.entries[key]; ok {
		c.stamp++
		entry.stamp = c.stamp
		c.entries[key] = entry
		if c.inflight[key] == nil {
			c.enqueueLocked(job)
		}
		c.mu.Unlock()
		return entry.snapshot.page(ctx, after, limit)
	}
	if c.inflight[key] != nil {
		c.mu.Unlock()
		return DirectoryPage{}, ErrDirectorySearchPending
	}
	c.inflight[key] = job
	c.workers.Add(1)
	c.mu.Unlock()
	defer c.workers.Done()

	scanCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	snapshot, scanErr := c.scan(scanCtx, s, job.roots, job.query, directorySearchWorkers)
	if scanErr == nil {
		current, err := directorySearchKey(scanCtx, s, job.roots, job.query)
		if err != nil {
			scanErr = err
		} else if current != key {
			scanErr = ErrDirectoryUnavailable
		}
	}
	stop()
	cancel()
	c.mu.Lock()
	delete(c.inflight, key)
	if !c.closed && c.ctx.Err() == nil {
		if scanErr == nil {
			c.storeLocked(key, snapshot)
		} else if errors.Is(scanErr, context.DeadlineExceeded) && callerCtx.Err() == nil {
			if c.enqueueLocked(job) {
				scanErr = ErrDirectorySearchPending
			}
		}
	}
	c.mu.Unlock()
	if scanErr != nil {
		return DirectoryPage{}, scanErr
	}
	return snapshot.page(ctx, after, limit)
}

// Caller holds mu. Failed queue admission never leaves an inflight marker.
func (c *directorySearchCache) enqueueLocked(job *directorySearchJob) bool {
	if c.closed || c.ctx.Err() != nil {
		return false
	}
	if c.inflight[job.key] != nil {
		return true
	}
	if len(c.queue) >= directorySearchQueueSize {
		return false
	}
	c.inflight[job.key] = job
	c.queue = append(c.queue, job)
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return true
}

func (c *directorySearchCache) run() {
	defer c.workers.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.wake:
		}
		for {
			c.mu.Lock()
			if c.closed || c.ctx.Err() != nil || len(c.queue) == 0 {
				c.mu.Unlock()
				break
			}
			job := c.queue[0]
			c.queue[0] = nil
			c.queue = c.queue[1:]
			c.mu.Unlock()
			ctx, cancel := context.WithTimeout(c.ctx, directorySearchBackgroundTimeout)
			key, err := directorySearchKey(ctx, job.service, job.roots, job.query)
			var snapshot directorySearchSnapshot
			if err == nil && key == job.key {
				snapshot, err = c.scan(ctx, job.service, job.roots, job.query, directorySearchBackgroundWorkers)
			} else if err == nil {
				err = ErrDirectoryUnavailable
			}
			if err == nil {
				key, err = directorySearchKey(ctx, job.service, job.roots, job.query)
				if err == nil && key != job.key {
					err = ErrDirectoryUnavailable
				}
			}
			cancel()
			c.mu.Lock()
			delete(c.inflight, job.key)
			if err == nil && !c.closed && c.ctx.Err() == nil {
				c.storeLocked(job.key, snapshot)
			}
			c.mu.Unlock()
		}
	}
}

// Count retained strings and choice storage conservatively. Oversized complete
// results can be returned synchronously, but are never truncated to fit cache.
func directorySnapshotBytes(key string, s directorySearchSnapshot) int {
	n := 512 + len(key) + len(s.Path) + len(s.Parent)
	for _, c := range s.Choices {
		n += 64 + len(c.Path) + len(c.ProjectID) + len(c.Label)
	}
	return n
}

func (c *directorySearchCache) storeLocked(key string, snapshot directorySearchSnapshot) {
	weight := directorySnapshotBytes(key, snapshot)
	if weight > directorySearchCacheBytes {
		if old, ok := c.entries[key]; ok {
			c.bytes -= old.bytes
			delete(c.entries, key)
		}
		return
	}
	if old, ok := c.entries[key]; ok {
		if old.snapshot.Path == snapshot.Path && old.snapshot.Parent == snapshot.Parent && old.snapshot.ReadOnly == snapshot.ReadOnly && slices.Equal(old.snapshot.Choices, snapshot.Choices) {
			return
		}
		c.bytes -= old.bytes
		delete(c.entries, key)
	}
	for len(c.entries) >= directorySearchCacheEntries || c.bytes+weight > directorySearchCacheBytes {
		var oldest string
		var stamp uint64
		for key, entry := range c.entries {
			if oldest == "" || entry.stamp < stamp {
				oldest, stamp = key, entry.stamp
			}
		}
		c.bytes -= c.entries[oldest].bytes
		delete(c.entries, oldest)
	}
	snapshot.Choices = slices.Clone(snapshot.Choices)
	c.stamp++
	c.entries[key] = directorySearchCacheEntry{snapshot: snapshot, stamp: c.stamp, bytes: weight}
	c.bytes += weight
}
