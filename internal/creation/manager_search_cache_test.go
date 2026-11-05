package creation

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInlineSearchPendingPreservesFormAndUsesCompletedCache(t *testing.T) {
	h := newManagerHarness(t)
	want := directory(t, h.root, "cache-match")
	choice, err := h.m.directories(nil).Browse(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := newDirectorySearchCache(ctx)
	defer cache.Close()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cache.scan = func(ctx context.Context, _ DirectoryService, _ []string, _ string, workers int) (directorySearchSnapshot, error) {
		if workers == directorySearchWorkers {
			<-ctx.Done()
			return directorySearchSnapshot{}, ctx.Err()
		}
		once.Do(func() { close(started) })
		select {
		case <-release:
			return directorySearchSnapshot{Path: h.root, Choices: []DirectoryChoice{choice}}, nil
		case <-ctx.Done():
			return directorySearchSnapshot{}, ctx.Err()
		}
	}
	h.m.directorySearchCache.Store(cache)
	d := h.receive("cached-search", "original task")
	if err := h.action(d, "creation_find", map[string]any{"creation_query": "cache-match", "creation_body": "keep these edits", "creation_use_worktree": false}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.ErrorCode != "directory-searching" || d.Body != "keep these edits" || d.Directory != h.root || d.WorkspaceMode != "local" || d.BrowsePath != "cache-match" {
		t.Fatal("pending search lost valid form edits or changed the execution directory")
	}
	if got := inlineDirectoryValues(t, h, d); len(got) != 0 || len(h.n.starts) != 0 || h.g.groups != 0 {
		t.Fatal("pending search offered a partial result or created a task")
	}
	if !strings.Contains(h.g.cards[d.CardID].Content, "正在后台查找目录") {
		t.Fatal("pending search did not explain how to retry")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed-out search did not enter the background lane")
	}
	cardBefore := h.g.cards[d.CardID].Content
	close(release)
	deadline, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	for {
		page, err := inlineDirectoryPage(deadline, h.m.directories(nil), d)
		if err == nil {
			if len(page.Choices) != 1 || page.Choices[0].Path != want {
				t.Fatal("completed search did not return its cached result")
			}
			break
		}
		if !errors.Is(err, ErrDirectorySearchPending) || deadline.Err() != nil {
			t.Fatal("background result did not become available", err)
		}
		time.Sleep(time.Millisecond)
	}
	if h.g.cards[d.CardID].Content != cardBefore || h.draft(d.SourceMessageID).Revision != d.Revision {
		t.Fatal("background completion rewrote the card or persisted draft")
	}
	if err := h.action(d, "creation_find", map[string]any{"creation_query": "cache-match", "creation_body": "newer phone edits", "creation_use_worktree": false}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if got := inlineDirectoryValues(t, h, d); !reflect.DeepEqual(got, []string{want}) || d.ErrorCode != "" || d.Body != "newer phone edits" {
		t.Fatal("next search did not use the completed cache and retain newer edits")
	}
	// A cached display choice is never sufficient authority to execute in a
	// directory which has since disappeared.
	if err := os.Remove(want); err != nil {
		t.Fatal(err)
	}
	if err := h.action(d, "creation_submit", map[string]any{"creation_directory": want, "creation_use_worktree": false}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.State != "draft" || d.ErrorCode != "directory" || d.Directory != h.root || len(h.n.starts) != 0 || h.g.groups != 0 {
		t.Fatal("stale cached choice bypassed current directory validation")
	}
}

func TestManagerRunOwnsSearchCacheLifetime(t *testing.T) {
	var m Manager // Disabled creation needs no database or remote dependencies.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("manager search cache prevented shutdown")
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for m.directories(nil).searchCache == nil {
		if time.Now().After(deadline) {
			t.Fatal("Run did not install the shared search cache")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("manager did not stop")
	}
	if m.directories(nil).searchCache != nil {
		t.Fatal("stopped manager still exposes its closed cache")
	}
}
