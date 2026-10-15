package creation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func selectionMkdir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCombinedSelectionPreservesDraftAndCreatesOnlyAfterSubmit(t *testing.T) {
	h := newManagerHarness(t)
	ctx := context.Background()
	alpha := selectionMkdir(t, h.root, "alpha")
	beta := selectionMkdir(t, h.root, "beta")
	sub := selectionMkdir(t, beta, "child")
	h.n.projects = []codexclient.Project{{ID: "beta-project", Name: "Beta Project", Roots: []codexclient.ProjectRoot{{Path: beta}}}}
	d := h.receive("combined", "/new original task")
	if err := h.action(d, "creation_search", map[string]any{"creation_body": "edited task", "creation_directory": alpha, "creation_query": "BETA PROJECT"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("combined")
	if d.View != "search" || d.BrowsePath != "BETA PROJECT" || d.Body != "edited task" || d.Directory != alpha {
		t.Fatal("search lost draft fields", d.View)
	}
	if !strings.Contains(h.g.cards[d.CardID].Content, beta) {
		t.Fatal("project search did not render its result")
	}
	if err := h.action(d, "creation_search_pick", map[string]any{"creation_search_choice": beta, "creation_directory": alpha}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("combined")
	if d.Directory != beta || d.ProjectID != "beta-project" || d.View != "main" || d.Body != "edited task" {
		t.Fatal("search result did not return to same draft")
	}
	if err := h.action(d, "creation_panel", map[string]any{"creation_body": "task after browsing"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("combined")
	if d.View != "directories" || d.BrowsePath != beta {
		t.Fatal("fallback panel did not start at selection")
	}
	if err := h.action(d, "creation_browse", map[string]any{"creation_entry": sub}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("combined")
	if d.Directory != beta || d.BrowsePath != sub {
		t.Fatal("browsing implicitly selected child")
	}
	if err := h.m.Action(ctx, d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_pick", map[string]any{"path": sub}, nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("combined")
	if d.Directory != sub || d.ProjectID != "" || d.View != "main" || d.Body != "task after browsing" {
		t.Fatal("panel selection lost fields or inherited parent project")
	}
	if len(h.n.starts) != 0 || len(h.n.adds) != 0 || h.g.groups != 0 {
		t.Fatal("directory navigation created remote objects")
	}
	h.submit(d)
	if len(h.n.starts) != 1 || len(h.n.adds) != 1 || h.n.starts[0].CWD != sub || h.n.adds[0].body != "task after browsing" {
		t.Fatal("explicit creation did not use combined draft")
	}
}

func TestFailedDirectorySearchKeepsTaskEditsAndSafeSelection(t *testing.T) {
	h := newManagerHarness(t)
	d := h.receive("invalid-search", "/new original")
	outside := t.TempDir()
	if err := h.action(d, "creation_search", map[string]any{"creation_body": "keep these edits", "creation_query": outside}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("invalid-search")
	if d.Body != "keep these edits" || d.Directory != h.root || d.ErrorCode != "directory-scope" || d.Revision != 2 {
		t.Fatal("failed search discarded edits or persisted invalid path")
	}
	if len(h.n.starts) != 0 || h.g.groups != 0 {
		t.Fatal("failed search created a task")
	}
}

func TestSelectionHistoryIsScopedToCreatorAndDeployment(t *testing.T) {
	h := newManagerHarness(t)
	ctx := context.Background()
	mine := selectionMkdir(t, h.root, "mine")
	other := selectionMkdir(t, h.root, "other")
	for _, v := range []struct{ user, path, source string }{{"ou_creator", mine, "mine"}, {"ou_other", other, "other"}} {
		d, _, err := h.s.ReceiveCreationDraft(ctx, store.CreationDraft{HostID: "host", AppID: "app", CreatorID: v.user, SourceMessageID: v.source, SourceChatID: "oc_private", SourceChatType: "p2p", Body: "fixture", Directory: v.path, FollowMode: "group", CreatedAtMS: h.clock.UnixMilli(), ExpiresAtMS: h.clock.Add(1e12).UnixMilli()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = h.s.DB.Exec(`UPDATE creation_drafts SET state='created' WHERE draft_id=?`, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	dirs, err := h.m.selectionDirectories(ctx, h.n, "ou_creator")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs.Recent) != 1 || dirs.Recent[0] != mine {
		t.Fatal("history crossed creator boundary")
	}
}

func TestNativeDirectoryPanelFiltersPagesAndNavigatesWithoutSelecting(t *testing.T) {
	h := newManagerHarness(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		selectionMkdir(t, h.root, fmt.Sprintf("match-%02d", i))
	}
	selectionMkdir(t, h.root, "unrelated")
	d := h.receive("panel", "/new original")
	if err := h.action(d, "creation_panel", map[string]any{"creation_body": "keep me"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("panel")
	if err := h.action(d, "creation_filter", map[string]any{"creation_filter": "match"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("panel")
	filter, after, err := parseDirectoryCursor(d.BrowseCursor)
	if err != nil || filter != "match" || after != "" {
		t.Fatal("filter not persisted", err)
	}
	page, err := h.m.directories(h.n).ListMatching(ctx, d.BrowsePath, filter, after, 10)
	if err != nil || len(page.Choices) != 10 || page.Next == "" {
		t.Fatal("filter pagination unavailable", err)
	}
	if err := h.m.Action(ctx, d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_browse", map[string]any{"path": d.BrowsePath, "after": page.Next}, map[string]any{"creation_filter": "match"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("panel")
	filter, after, err = parseDirectoryCursor(d.BrowseCursor)
	if err != nil || filter != "match" || after != page.Next {
		t.Fatal("filter changed across next page")
	}
	if err := h.m.Action(ctx, d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_browse", map[string]any{"path": d.BrowsePath, "after": page.Next}, map[string]any{"creation_filter": "unrelated"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("panel")
	filter, after, err = parseDirectoryCursor(d.BrowseCursor)
	if err != nil || filter != "unrelated" || after != "" {
		t.Fatal("changed filter reused old cursor")
	}
	target := filepath.Join(h.root, "match-01")
	if err := h.action(d, "creation_goto", map[string]any{"creation_path": target, "creation_filter": "unrelated"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("panel")
	if d.BrowsePath != target || d.BrowseCursor != "" || d.Directory != h.root || d.Body != "keep me" {
		t.Fatal("goto selected path or lost draft/position")
	}
	crumbs := directoryBreadcrumbs(ctx, h.m.directories(h.n), target)
	if len(crumbs) < 2 || len(crumbs) > 4 || crumbs[0].Value != h.root || crumbs[len(crumbs)-1].Value != target {
		t.Fatal("breadcrumbs escaped scope or omitted target")
	}
	if len(h.n.starts) != 0 || len(h.n.adds) != 0 || h.g.groups != 0 {
		t.Fatal("panel browsing created remote objects")
	}
	if err := h.action(d, "creation_path", map[string]any{"creation_path": target}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("panel")
	if d.Directory != target || d.View != "main" || d.Body != "keep me" {
		t.Fatal("explicit path choice did not return original draft")
	}
}

func TestSearchResultsCannotCreateInPreviousDirectory(t *testing.T) {
	h := newManagerHarness(t)
	target := selectionMkdir(t, h.root, "target")
	d := h.receive("search-submit", "/new task")
	if err := h.action(d, "creation_search", map[string]any{"creation_query": target}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("search-submit")
	if err := h.action(d, "creation_submit", map[string]any{"creation_search_choice": target}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("search-submit")
	if d.State != "draft" || d.ErrorCode != "directory-selection" || d.Directory != h.root || len(h.n.starts) != 0 || h.g.groups != 0 {
		t.Fatal("search selection implicitly created in previous cwd")
	}
	if err := h.action(d, "creation_search_pick", map[string]any{"creation_search_choice": target}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("search-submit")
	h.submit(d)
	if len(h.n.starts) != 1 || h.n.starts[0].CWD != target {
		t.Fatal("selected search cwd not used")
	}
}

func TestQuickSelectionOfDefaultDirectoryResolvesExactProject(t *testing.T) {
	h := newManagerHarness(t)
	h.n.projects = []codexclient.Project{{ID: "default-project", Name: "Default Project", Roots: []codexclient.ProjectRoot{{Path: h.root}}}}
	d := h.receive("same-project", "/new task")
	if err := h.action(d, "creation_submit", map[string]any{"creation_directory": h.root}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if len(h.n.starts) != 1 || h.n.starts[0].ProjectID != "default-project" || h.n.starts[0].CWD != h.root {
		t.Fatal("same default cwd omitted known project")
	}
}

func TestDirectoryPositionLegacyCursorAndValidation(t *testing.T) {
	filter, after, err := parseDirectoryCursor("/allowed/next")
	if err != nil || filter != "" || after != "/allowed/next" {
		t.Fatal("legacy cursor rejected")
	}
	for _, raw := range []string{`{"filter":"x","after":"relative"}`, `{"filter":"line\nbreak"}`, `not-json`} {
		if _, _, err := parseDirectoryCursor(raw); err == nil {
			t.Fatal("invalid directory position accepted")
		}
	}
}
