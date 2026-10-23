package creation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
)

func TestDirectorySearchRecursiveDepthNamesAndNoCatalog(t *testing.T) {
	root := directory(t, t.TempDir(), "MATCH-root")
	want := []string{root}
	parent := root
	for depth := 1; depth <= 5; depth++ {
		parent = directory(t, parent, fmt.Sprintf("match-level-%d", depth))
		if depth <= 4 {
			want = append(want, parent)
		}
	}
	directory(t, root, "unrelated")
	s := DirectoryService{Roots: []string{root}, Projects: func(context.Context) ([]codexclient.Project, error) {
		t.Fatal("recursive filesystem search called the native catalog")
		return nil, nil
	}}
	page, err := s.SearchRecursive(context.Background(), root, "  MaTcH  ", "", 100)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), want) || page.Path != root || page.Total != 5 || page.Next != "" {
		t.Fatal("recursive search lost root/fourth level or included fifth level", page, err)
	}
	page, err = s.SearchRecursive(context.Background(), root, "MATCH-root", "", 100)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), []string{root}) {
		t.Fatal("query matched parent path instead of visible basename", page, err)
	}
}

func TestDirectorySearchRecursiveGlobalEntryBudget(t *testing.T) {
	root := t.TempDir()
	a, b := directory(t, root, "bucket-a"), directory(t, root, "bucket-b")
	for _, path := range []string{a, b} {
		for i := 0; i < (maxDirectoryCandidates-2)/2; i++ {
			if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("file-%04d", i)), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := DirectoryService{Roots: []string{root}}
	page, err := s.SearchRecursive(context.Background(), root, "bucket", "", 100)
	if err != nil || page.Total != 2 {
		t.Fatal("exact global entry budget was rejected", page, err)
	}
	if err := os.WriteFile(filepath.Join(b, "one-extra-file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	page, err = s.SearchRecursive(context.Background(), root, "bucket", "", 100)
	if !errors.Is(err, ErrDirectoryLimit) || len(page.Choices) != 0 || page.Total != 0 || page.Next != "" {
		t.Fatal("global overflow returned partial results or counted only directories", page, err)
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
	for depth := 1; depth <= 4; depth++ {
		parent = directory(t, parent, fmt.Sprintf("found-%d", depth))
		if depth <= 3 {
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
	s := DirectoryService{Roots: []string{base}}
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
