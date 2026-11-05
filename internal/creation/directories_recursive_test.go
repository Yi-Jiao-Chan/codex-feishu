package creation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
)

func TestDirectorySearchRecursiveDepthNamesAndNoCatalog(t *testing.T) {
	root := directory(t, t.TempDir(), "MATCH-root")
	want := []string{root}
	parent := root
	for depth := 1; depth <= 11; depth++ {
		parent = directory(t, parent, fmt.Sprintf("match-level-%d", depth))
		if depth <= 10 {
			want = append(want, parent)
		}
	}
	directory(t, root, "unrelated")
	s := DirectoryService{Roots: []string{root}, SearchDepth: 10, Projects: func(context.Context) ([]codexclient.Project, error) {
		t.Fatal("recursive filesystem search called the native catalog")
		return nil, nil
	}}
	page, err := s.SearchRecursive(context.Background(), root, "  MaTcH  ", "", 100)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), want) || page.Path != root || page.Total != 11 || page.Next != "" {
		t.Fatal("recursive search lost root/tenth level or included eleventh level", page, err)
	}
	page, err = s.SearchRecursive(context.Background(), root, "MATCH-root", "", 100)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), []string{root}) {
		t.Fatal("query matched parent path instead of visible basename", page, err)
	}
}

func TestDirectorySearchRecursiveStreamsFilesWithoutLosingProjectMatches(t *testing.T) {
	root := t.TempDir()
	want := directory(t, root, "sdlc-flow")
	files := directory(t, root, "source-files")
	for i := 0; i < maxDirectoryCandidates+128; i++ {
		if err := os.WriteFile(filepath.Join(files, fmt.Sprintf("file-%04d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := DirectoryService{Roots: []string{root}}
	page, err := s.SearchRecursive(context.Background(), root, "sdlc-flow", "", 100)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), []string{want}) || page.Total != 1 {
		t.Fatal("unrelated files hid a matching project", page, err)
	}
}

func TestDirectorySearchRecursiveExceedsLegacyCandidateLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= maxDirectoryCandidates; i++ {
		directory(t, root, fmt.Sprintf("child-%04d", i))
	}
	s := DirectoryService{Roots: []string{root}}
	// Verify count/pagination independently of the interactive time budget;
	// instrumented or loaded test hosts can legitimately take over 500ms.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, err := s.searchRecursiveRoots(ctx, []string{root}, "child", "", 100)
	if err != nil || page.Total != maxDirectoryCandidates+1 || len(page.Choices) != 100 || page.Next != filepath.Join(root, "child-0099") {
		t.Fatal("recursive name search retained the legacy candidate limit", page, err)
	}
	page, err = s.searchRecursiveRoots(ctx, []string{root}, "child", filepath.Join(root, fmt.Sprintf("child-%04d", maxDirectoryCandidates-1)), 100)
	if err != nil || page.Total != maxDirectoryCandidates+1 || page.Offset != maxDirectoryCandidates || !reflect.DeepEqual(directoryPaths(page.Choices), []string{filepath.Join(root, fmt.Sprintf("child-%04d", maxDirectoryCandidates))}) || page.Next != "" {
		t.Fatal("matches beyond the legacy candidate limit were unreachable", page, err)
	}
	scanCtx, cancelScan := context.WithCancel(context.Background())
	defer cancelScan()
	timer := time.AfterFunc(time.Millisecond, cancelScan)
	defer timer.Stop()
	page, err = s.SearchRecursive(scanCtx, root, "child", "", 100)
	if !errors.Is(err, context.Canceled) || len(page.Choices) != 0 || page.Total != 0 || page.Offset != 0 || page.Next != "" {
		t.Fatal("cancellation during a large scan returned partial results", page, err)
	}
}

func TestDirectorySearchRecursiveWorkerReadFailurePreservesError(t *testing.T) {
	s := DirectoryService{Roots: []string{t.TempDir()}}
	missing := filepath.Join(s.Roots[0], "removed")
	err := s.searchRecursiveRoot(context.Background(), missing, "match", map[string]DirectoryChoice{})
	if !errors.Is(err, ErrDirectoryUnavailable) {
		t.Fatal("worker read failure was replaced by pool cancellation", err)
	}
}

func TestDirectorySearchRecursivePrunesGeneratedTreesButAllowsExplicitRoots(t *testing.T) {
	root := t.TempDir()
	want := directory(t, root, "sdlc-flow")
	s := DirectoryService{Roots: []string{root}}
	for _, relative := range []string{".config", "node_modules", "vendor", "__pycache__", "pkg/mod"} {
		tree := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(tree, 0700); err != nil {
			t.Fatal(err)
		}
		nested := directory(t, tree, "sdlc-flow")
		page, err := s.SearchRecursive(context.Background(), tree, "sdlc-flow", "", 100)
		if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), []string{nested}) {
			t.Fatal("explicitly selected generated/hidden root was not searched", relative, page, err)
		}
		page, err = s.SearchRecursive(context.Background(), root, filepath.Base(tree), "", 100)
		if err != nil || !slices.Contains(directoryPaths(page.Choices), tree) {
			t.Fatal("generated/hidden directory name could not be selected", relative, page, err)
		}
	}
	cache := directory(t, root, ".cache")
	for i := 0; i <= maxDirectoryCandidates; i++ {
		directory(t, cache, fmt.Sprintf("cache-%04d", i))
	}
	if err := os.Symlink(cache, filepath.Join(root, "cache-alias")); err != nil {
		t.Fatal(err)
	}
	deep := directory(t, cache, "deep")
	directory(t, deep, "sdlc-flow")
	if err := os.Symlink(deep, filepath.Join(root, "cache-descendant-alias")); err != nil {
		t.Fatal(err)
	}
	// A project simply named mod is still searched outside a pkg/mod cache.
	mod := directory(t, root, "mod")
	second := directory(t, mod, "sdlc-flow")
	page, err := s.SearchRecursive(context.Background(), root, "sdlc-flow", "", 100)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), []string{second, want}) || page.Total != 2 {
		t.Fatal("name search descended into metadata/dependencies or pruned a normal project", page, err)
	}
}

func TestDirectorySearchRecursivePaginationAndCancellation(t *testing.T) {
	base := t.TempDir()
	root := directory(t, base, "search-base")
	var want []string
	for i := 0; i < 105; i++ {
		want = append(want, directory(t, root, fmt.Sprintf("item-%03d", i)))
	}
	s := DirectoryService{Roots: []string{base}}
	page, err := s.SearchRecursive(context.Background(), root, "item", "", 200)
	if err != nil || len(page.Choices) != 100 || page.Next != want[99] || page.Total != len(want) || page.Offset != 0 || page.Parent != base {
		t.Fatal("recursive first page/cap/root metadata incorrect", page, err)
	}
	page, err = s.SearchRecursive(context.Background(), root, "item", page.Next, 200)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), want[100:]) || page.Total != len(want) || page.Offset != 100 || page.Next != "" {
		t.Fatal("recursive next page incorrect", page, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if page, err = s.SearchRecursive(ctx, root, "", "", 100); !errors.Is(err, context.Canceled) || len(page.Choices) != 0 {
		t.Fatal("recursive search ignored cancellation", page, err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err = s.SearchRecursive(ctx, root, "", "", 100); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("recursive search replaced shorter caller deadline", err)
	}
	for _, after := range []string{"relative", root + "/../other", root + "\n"} {
		if _, err = s.SearchRecursive(context.Background(), root, "", after, 100); !errors.Is(err, ErrDirectoryCursor) {
			t.Fatal("recursive search accepted invalid cursor", after, err)
		}
	}
	if _, err = s.SearchRecursive(context.Background(), root, "\nitem", "", 100); !errors.Is(err, ErrDirectoryUnavailable) {
		t.Fatal("recursive search accepted control characters", err)
	}
}

func TestDirectorySearchRecursiveSymlinkScopeAliasesAndShallowestDepth(t *testing.T) {
	base := t.TempDir()
	root := directory(t, base, "search-base")
	outside := directory(t, base, "outside")
	directory(t, outside, "found-escape")
	a := directory(t, root, "a")
	b := directory(t, a, "b")
	c := directory(t, b, "c")
	target := directory(t, c, "target")
	var want []string
	parent := target
	for depth := 1; depth <= 10; depth++ {
		parent = directory(t, parent, fmt.Sprintf("found-%d", depth))
		if depth <= 9 {
			want = append(want, parent)
		}
	}
	for link, dest := range map[string]string{
		filepath.Join(root, "z-shortcut"):        target,
		filepath.Join(root, "visible-ALIAS"):     target,
		filepath.Join(c, "back-to-root"):         root,
		filepath.Join(root, "found-escape-link"): outside,
	} {
		if err := os.Symlink(dest, link); err != nil {
			t.Fatal(err)
		}
	}
	// The deployment authorizes both siblings; this search still must remain
	// within its narrower root and never enumerate the outside symlink target.
	s := DirectoryService{Roots: []string{base}, SearchDepth: 10}
	page, err := s.SearchRecursive(context.Background(), root, "found", "", 100)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), want) || page.Total != len(want) {
		t.Fatal("symlink traversal escaped, looped, duplicated, or lost shallow route", page, err)
	}
	page, err = s.SearchRecursive(context.Background(), root, "visible-alias", "", 100)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), []string{target}) {
		t.Fatal("search failed to match a symlink's visible basename", page, err)
	}
	if _, err = (DirectoryService{Roots: []string{root}}).SearchRecursive(context.Background(), outside, "", "", 100); !errors.Is(err, ErrDirectoryOutsideRoots) {
		t.Fatal("recursive search accepted root outside configured scope", err)
	}
}

func TestDirectorySearchRecursiveReadOnlyMetadataAndAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses normal directory access bits")
	}
	base := t.TempDir()
	root := directory(t, base, "readonly")
	child := directory(t, root, "visible-child")
	restricted := directory(t, root, "visible-restricted")
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0700) })
	if err := os.Chmod(restricted, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(restricted, 0700) })
	s := DirectoryService{Roots: []string{base}}
	page, err := s.SearchRecursive(context.Background(), root, "visible", "", 100)
	if err != nil || !page.ReadOnly || !reflect.DeepEqual(directoryPaths(page.Choices), []string{child}) || page.Choices[0].ReadOnly {
		t.Fatal("recursive search lost readonly metadata or traversed inaccessible directory", page, err)
	}
}
