package creation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"codex-feishu/internal/displaypath"
	"codex-feishu/internal/store"
)

func TestInlineSearchCombinesHomeAndSelectedDirectory(t *testing.T) {
	h := newManagerHarness(t)
	t.Setenv("HOME", h.root)
	parent := filepath.Join(h.root, "goProject", "projects")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	selected := directory(t, parent, "selected-project")
	sibling := directory(t, parent, "data-access")
	shared := directory(t, selected, "data-access")
	deep := directory(t, directory(t, directory(t, selected, "a"), "b"), "data-access")
	want := []string{sibling, shared, deep}
	sort.Strings(want)
	d := h.receive("dual-root-search", "original task")
	reads := h.n.reads
	if err := h.action(d, "creation_find", map[string]any{"creation_query": "data-access", "creation_directory": selected, "creation_body": "edited task", "creation_use_worktree": false}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	position, err := parseInlineDirectoryPosition(d.BrowseCursor)
	if err != nil || !reflect.DeepEqual(position.Roots, []string{h.root, selected}) || position.Root != selected {
		t.Fatal("home and current roots were not frozen", position, err)
	}
	if got := inlineDirectoryValues(t, h, d); !reflect.DeepEqual(got, want) || d.Directory != selected || d.Body != "edited task" || h.n.reads != reads {
		t.Fatal("merged search lost a sibling/deep result, duplicated a path, changed the selection, or queried the catalog", got)
	}
	h.restart()
	d = h.draft(d.SourceMessageID)
	page, err := inlineDirectoryPage(context.Background(), h.m.directories(nil), d)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), want) || page.Total != 3 {
		t.Fatal("restart lost the merged result scope", page, err)
	}
	h.g.expectedDirectory = displaypath.Shorten(sibling)
	if err := h.action(d, "creation_submit", map[string]any{"creation_directory": sibling, "creation_use_worktree": false}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if len(h.n.starts) != 1 || h.n.starts[0].CWD != sibling || h.intent(d).ExecutionDirectory() != sibling {
		t.Fatal("home-route selection did not reach the canonical execution directory")
	}
}

func TestInlineSearchPrefixAfterSelectingLargeProject(t *testing.T) {
	h := newManagerHarness(t)
	t.Setenv("HOME", h.root)
	projects := directory(t, h.root, "projects")
	selected := directory(t, projects, "data-access")
	want := directory(t, projects, "smtp-in-postfix-milter")
	homeEntries := directory(t, h.root, "home-entries")
	deepEntries := directory(t, directory(t, selected, "a"), "b")
	// The combined routes exceed the former shared candidate limit; depth
	// and time bound recursive name search instead.
	for i := 0; i < maxDirectoryCandidates/2+4; i++ {
		name := fmt.Sprintf("entry-%04d", i)
		directory(t, homeEntries, name)
		directory(t, deepEntries, name)
	}
	d := h.receive("prefix-after-selection", "original task")
	reads := h.n.reads
	for _, query := range []string{"data-", "data-acc", "smtp-in-post"} {
		if err := h.action(d, "creation_find", map[string]any{"creation_query": query, "creation_directory": selected, "creation_body": "edited task", "creation_use_worktree": false}); err != nil {
			t.Fatal(err)
		}
		h.tick()
		d = h.draft(d.SourceMessageID)
		expected := selected
		if query == "smtp-in-post" {
			expected = want
		}
		if got := inlineDirectoryValues(t, h, d); !reflect.DeepEqual(got, []string{expected}) || d.ErrorCode != "" || d.Directory != selected || d.Body != "edited task" || h.n.reads != reads {
			t.Fatal("prefix search lost results after selecting a large project", query, got, d.ErrorCode)
		}
	}
}

func TestInlineSearchRootsRespectHomeAuthorizationAndAliases(t *testing.T) {
	home := t.TempDir()
	selected := directory(t, t.TempDir(), "selected")
	t.Setenv("HOME", home)
	m := &Manager{}
	m.Config.Creation.AllowedRoots = []string{home, selected}
	roots, err := m.inlineSearchRoots(context.Background(), selected)
	if err != nil || !reflect.DeepEqual(roots, []string{home, selected}) {
		t.Fatal("selected directories outside home lost their own route", roots, err)
	}
	m.Config.Creation.AllowedRoots = []string{selected}
	roots, err = m.inlineSearchRoots(context.Background(), selected)
	if err != nil || !reflect.DeepEqual(roots, []string{selected}) {
		t.Fatal("home bypassed configured authorization", roots, err)
	}
	m.Config.Creation.AllowedRoots = []string{home}
	alias := filepath.Join(t.TempDir(), "home-alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", alias)
	roots, err = m.inlineSearchRoots(context.Background(), home)
	if err != nil || !reflect.DeepEqual(roots, []string{home}) {
		t.Fatal("logical and physical home aliases created separate routes", roots, err)
	}
	roots, err = m.inlineSearchRoots(context.Background(), "")
	if err != nil || !reflect.DeepEqual(roots, []string{home}) {
		t.Fatal("an absent current selection widened the home search", roots, err)
	}
	t.Setenv("HOME", "")
	roots, err = m.inlineSearchRoots(context.Background(), home)
	if err != nil || !reflect.DeepEqual(roots, []string{home}) {
		t.Fatal("an unavailable home broke the current-directory route", roots, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.inlineSearchRoots(ctx, home); !errors.Is(err, context.Canceled) {
		t.Fatal("root resolution ignored cancellation", err)
	}
}

func TestInlineSearchUsesConfiguredDepthForBothRoots(t *testing.T) {
	home, selected := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	var atTwo, atThree []string
	for _, root := range []string{home, selected} {
		parent := directory(t, root, "parent")
		atTwo = append(atTwo, directory(t, parent, "match-two"))
		atThree = append(atThree, directory(t, directory(t, parent, "nested"), "match-three"))
	}
	m := &Manager{}
	m.Config.Creation.AllowedRoots = []string{home, selected}
	d := store.CreationDraft{Directory: selected, BrowsePath: "match"}
	for _, depth := range []int{2, 3} {
		m.Config.Creation.DirectorySearchDepth = depth
		if err := m.positionInlineDirectory(context.Background(), &d, "creation_find", nil); err != nil {
			t.Fatal(err)
		}
		page, err := inlineDirectoryPage(context.Background(), m.directories(nil), d)
		want := append([]string{}, atTwo...)
		if depth == 3 {
			want = append(want, atThree...)
		}
		sort.Strings(want)
		if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), want) {
			t.Fatal("configured depth did not apply to both roots", depth, page, err)
		}
		m.Config.Creation.DirectorySearchDepth = depth + 1
		frozen, err := inlineDirectoryPage(context.Background(), m.directories(nil), d)
		if err != nil || !reflect.DeepEqual(directoryPaths(frozen.Choices), want) {
			t.Fatal("an existing search changed depth without a new find", frozen, err)
		}
	}
}

func TestInlineDualRootPagingFreezesHomeAndCurrentDirectory(t *testing.T) {
	base := t.TempDir()
	home := directory(t, base, "a-home")
	selected := directory(t, base, "b-selected")
	newHome := directory(t, base, "c-new-home")
	for i := 0; i < 105; i++ {
		directory(t, home, fmt.Sprintf("match-%03d", i))
	}
	for i := 0; i < 5; i++ {
		directory(t, selected, fmt.Sprintf("match-%03d", i))
	}
	directory(t, newHome, "match-not-in-original-search")
	t.Setenv("HOME", home)
	m := &Manager{}
	m.Config.Creation.AllowedRoots = []string{base}
	d := store.CreationDraft{Directory: selected, BrowsePath: "match"}
	ctx := context.Background()
	if err := m.findInlineDirectory(ctx, &d, "creation_find", nil); err != nil {
		t.Fatal(err)
	}
	page, err := inlineDirectoryPage(ctx, m.directories(nil), d)
	if err != nil || page.Total != 110 || len(page.Choices) != 100 || page.Next == "" {
		t.Fatal("merged search did not produce one global first page", page, err)
	}
	d.Directory = page.Choices[0].Path
	t.Setenv("HOME", newHome)
	if err := m.findInlineDirectory(ctx, &d, "creation_find_next", map[string]any{"after": page.Next}); err != nil {
		t.Fatal(err)
	}
	page, err = inlineDirectoryPage(ctx, m.directories(nil), d)
	position, parseErr := parseInlineDirectoryPosition(d.BrowseCursor)
	if err != nil || parseErr != nil || !reflect.DeepEqual(position.Roots, []string{home, selected}) || page.Total != 110 || page.Offset != 100 || len(page.Choices) != 10 || page.Next != "" {
		t.Fatal("next page changed either frozen route", page, position, err, parseErr)
	}
	// An explicit new search of the same text takes the new HOME and selection.
	if err := m.positionInlineDirectory(ctx, &d, "creation_find", nil); err != nil {
		t.Fatal(err)
	}
	position, err = parseInlineDirectoryPosition(d.BrowseCursor)
	if err != nil || !reflect.DeepEqual(position.Roots, []string{newHome, d.Directory}) {
		t.Fatal("a new find reused stale roots", position, err)
	}
}

func TestInlineLegacySearchScopeAndAbsoluteQueriesStayExact(t *testing.T) {
	home := t.TempDir()
	selected := directory(t, home, "selected")
	inside := directory(t, selected, "data-access")
	outside := directory(t, home, "data-access")
	t.Setenv("HOME", home)
	m := &Manager{}
	m.Config.Creation.AllowedRoots = []string{home}
	raw, _ := json.Marshal(inlineDirectoryPosition{Query: "data-access", Root: selected})
	d := store.CreationDraft{Directory: selected, BrowsePath: "data-access", BrowseCursor: string(raw)}
	page, err := inlineDirectoryPage(context.Background(), m.directories(nil), d)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), []string{inside}) {
		t.Fatal("a frozen legacy cursor silently broadened its result set", page, err)
	}
	d.BrowsePath = outside
	if err := m.findInlineDirectory(context.Background(), &d, "creation_find", nil); err != nil {
		t.Fatal(err)
	}
	page, err = inlineDirectoryPage(context.Background(), m.directories(nil), d)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), []string{outside}) || d.Directory != selected {
		t.Fatal("an absolute path expanded into a name search or changed the selected cwd", page, err)
	}
	for _, cursor := range []string{
		`{"query":"match","roots":[""]}`,
		`{"query":"match","roots":["relative"]}`,
		`{"query":"match","roots":["/a","/b","/c"]}`,
		`{"query":"match","roots":["/a/../b"]}`,
		`{"query":"match","depth":-1}`,
	} {
		if _, err := parseInlineDirectoryPosition(cursor); !errors.Is(err, ErrDirectoryCursor) {
			t.Fatal("invalid search roots were accepted", cursor, err)
		}
	}
}
