package creation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestDirectorySearchRecursiveRootsOverlapExtendsDepthAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	want := []string{directory(t, root, "found-shared")}
	selected := directory(t, directory(t, directory(t, root, "a"), "b"), "c")
	parent := selected
	for depth := 1; depth <= 11; depth++ {
		parent = directory(t, parent, fmt.Sprintf("found-%d", depth))
		if depth <= 10 {
			want = append(want, parent)
		}
	}
	sort.Strings(want)
	s := DirectoryService{Roots: []string{root}, SearchDepth: 10}
	for _, roots := range [][]string{{root, selected}, {selected, root}} {
		page, err := s.SearchRecursiveRoots(context.Background(), roots, "found", "", 100)
		if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), want) || page.Total != len(want) || page.Path != roots[0] {
			t.Fatal("overlapping roots lost extended depth, duplicated matches, or changed metadata", page, err)
		}
	}
}

func TestDirectorySearchRecursiveRootsDisjointScopesAndGlobalPages(t *testing.T) {
	base := t.TempDir()
	a, z := directory(t, base, "a-root"), directory(t, base, "z-root")
	var want []string
	for _, root := range []string{z, a} {
		for _, name := range []string{"found-c", "found-a", "found-b"} {
			want = append(want, directory(t, root, name))
		}
	}
	sort.Strings(want)
	// Both roots are authorized, but an alias cannot use another route's scope
	// as permission to escape its own root or add that root as a matching cwd.
	if err := os.Symlink(a, filepath.Join(z, "found-alias")); err != nil {
		t.Fatal(err)
	}
	outside := directory(t, base, "outside")
	directory(t, outside, "found-escape")
	if err := os.Symlink(outside, filepath.Join(a, "found-escape-alias")); err != nil {
		t.Fatal(err)
	}
	s := DirectoryService{Roots: []string{a, z}}
	after := ""
	for offset := 0; offset < len(want); offset += 2 {
		page, err := s.SearchRecursiveRoots(context.Background(), []string{z, a}, "found", after, 2)
		if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), want[offset:offset+2]) || page.Total != len(want) || page.Offset != offset || page.Path != z {
			t.Fatal("disjoint routes did not merge into globally sorted pages", page, err)
		}
		if offset+2 < len(want) && page.Next != want[offset+1] || offset+2 == len(want) && page.Next != "" {
			t.Fatal("global page cursor was lost", page)
		}
		after = page.Next
	}
	page, err := s.SearchRecursiveRoots(context.Background(), []string{z, a}, "found", want[len(want)-1], 2)
	if err != nil || len(page.Choices) != 0 || page.Total != len(want) || page.Offset != len(want) || page.Next != "" {
		t.Fatal("global after cursor lost the complete total", page, err)
	}
}

func TestDirectorySearchRecursiveRootsCanonicalAliasesScanOnce(t *testing.T) {
	base := t.TempDir()
	root := directory(t, base, "root")
	for i := 0; i < maxDirectoryCandidates/2+1; i++ {
		directory(t, root, fmt.Sprintf("found-%04d", i))
	}
	alias := filepath.Join(base, "root-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	s := DirectoryService{Roots: []string{base}}
	page, err := s.SearchRecursiveRoots(context.Background(), []string{root, alias, root}, "found", "", 100)
	if err != nil || page.Total != maxDirectoryCandidates/2+1 || len(page.Choices) != 100 {
		t.Fatal("canonical aliases scanned more than once or duplicated matches", page, err)
	}
	page, err = s.SearchRecursiveRoots(context.Background(), []string{root, alias}, "root-alias", "", 100)
	if err != nil || !reflect.DeepEqual(directoryPaths(page.Choices), []string{root}) {
		t.Fatal("deduplicated root alias lost its visible matching name", page, err)
	}
}

func TestDirectorySearchRecursiveRootsExceedLegacyCandidateLimit(t *testing.T) {
	base := t.TempDir()
	a, b := directory(t, base, "a"), directory(t, base, "b")
	s := DirectoryService{Roots: []string{base}}
	for _, root := range []string{a, b} {
		for i := 0; i <= maxDirectoryCandidates; i++ {
			directory(t, root, fmt.Sprintf("found-%04d", i))
		}
	}
	// Large count/pagination correctness is separate from the public API's
	// 500ms budget, which may expire correctly on a busy test host.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, roots := range [][]string{{a, b}, {b, a}} {
		page, err := s.searchRecursiveRoots(ctx, roots, "found", "", 100)
		if err != nil || page.Total != 2*(maxDirectoryCandidates+1) || len(page.Choices) != 100 || page.Offset != 0 || page.Next != filepath.Join(a, "found-0099") {
			t.Fatal("multi-root name search retained a per-route or combined candidate limit", page, err)
		}
	}
	page, err := s.searchRecursiveRoots(ctx, []string{a, b}, "found", filepath.Join(a, fmt.Sprintf("found-%04d", maxDirectoryCandidates)), 100)
	if err != nil || page.Total != 2*(maxDirectoryCandidates+1) || page.Offset != maxDirectoryCandidates+1 || len(page.Choices) != 100 || page.Choices[0].Path != filepath.Join(b, "found-0000") || page.Next != filepath.Join(b, "found-0099") {
		t.Fatal("globally paged union lost matches beyond the legacy candidate limit", page, err)
	}
}

func TestDirectorySearchRecursiveRootsFailClosedOnInvalidRootsAndCancellation(t *testing.T) {
	base := t.TempDir()
	root, outside := directory(t, base, "root"), directory(t, base, "outside")
	directory(t, root, "found")
	s := DirectoryService{Roots: []string{root}}
	for _, test := range []struct {
		roots []string
		err   error
	}{
		{nil, ErrDirectoryUnavailable},
		{[]string{root, filepath.Join(root, "missing")}, ErrDirectoryUnavailable},
		{[]string{root, outside}, ErrDirectoryOutsideRoots},
	} {
		page, err := s.SearchRecursiveRoots(context.Background(), test.roots, "found", "", 100)
		if !errors.Is(err, test.err) || len(page.Choices) != 0 || page.Total != 0 || page.Next != "" {
			t.Fatal("invalid route returned another route's partial results", page, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err := s.SearchRecursiveRoots(ctx, []string{root, root}, "found", "", 100)
	if !errors.Is(err, context.Canceled) || len(page.Choices) != 0 || page.Total != 0 {
		t.Fatal("multi-root search ignored caller cancellation", page, err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	page, err = s.SearchRecursiveRoots(ctx, []string{root}, "found", "", 100)
	if !errors.Is(err, context.DeadlineExceeded) || len(page.Choices) != 0 || page.Total != 0 {
		t.Fatal("multi-root search replaced a shorter caller deadline", page, err)
	}
}
