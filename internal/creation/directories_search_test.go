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

	"codex-feishu/internal/codexclient"
)

func directoryPaths(choices []DirectoryChoice) []string {
	paths := []string{}
	for _, choice := range choices {
		paths = append(paths, choice.Path)
	}
	return paths
}

func TestDirectoryQuickRecentCommonProjectsAndExactIdentity(t *testing.T) {
	root := t.TempDir()
	recent := directory(t, root, "recent")
	common := directory(t, root, "common 中文")
	project := directory(t, root, "project")
	nested := directory(t, project, "child")
	alias := filepath.Join(root, "recent-alias")
	if err := os.Symlink(recent, alias); err != nil {
		t.Fatal(err)
	}
	s := DirectoryService{
		Roots: []string{root}, Recent: []string{recent, alias, nested},
		Common: []string{common, recent, t.TempDir(), filepath.Join(root, "missing")},
		Projects: func(context.Context) ([]codexclient.Project, error) {
			return []codexclient.Project{{ID: "p", Name: "Native Project", Roots: []codexclient.ProjectRoot{{Path: project}}}}, nil
		},
	}
	choices, err := s.Quick(context.Background(), 20)
	if err != nil || !reflect.DeepEqual(directoryPaths(choices), []string{recent, nested, common, project}) {
		t.Fatal("shortcut priority/scope/physical dedup incorrect", choices, err)
	}
	for i, choice := range choices {
		if i == 3 {
			if choice.ProjectID != "p" || choice.Label != "Native Project" {
				t.Fatal("exact native project missing", choice)
			}
		} else if choice.ProjectID != "" {
			t.Fatal("nested or unrelated directory acquired project identity", choice)
		}
	}
	choices, err = s.Quick(context.Background(), 2)
	if err != nil || !reflect.DeepEqual(directoryPaths(choices), []string{recent, nested}) {
		t.Fatal("shortcut limit reordered priority", choices, err)
	}
	for i := 0; i < 25; i++ {
		s.Recent = append(s.Recent, directory(t, root, fmt.Sprintf("extra-%02d", i)))
	}
	if choices, err = s.Quick(context.Background(), 100); err != nil || len(choices) != 20 {
		t.Fatal("shortcut maximum not enforced", len(choices), err)
	}
}

func TestDirectorySearchKnownPathsNamesAndLexicalPagination(t *testing.T) {
	root := t.TempDir()
	a := directory(t, root, "alpha")
	b := directory(t, root, "beta 中文")
	c := directory(t, root, "common")
	unknown := directory(t, root, "not-in-catalog")
	directory(t, unknown, "alpha-nested")
	s := DirectoryService{Roots: []string{root}, Recent: []string{c, a}, Common: []string{a}, Projects: func(context.Context) ([]codexclient.Project, error) {
		return []codexclient.Project{{ID: "p", Name: "CUSTOM LABEL", Roots: []codexclient.ProjectRoot{{Path: b}}}}, nil
	}}
	page, err := s.Search(context.Background(), "custom label", "", 20)
	if err != nil || len(page.Choices) != 1 || page.Choices[0].Path != b || page.Choices[0].ProjectID != "p" {
		t.Fatal("project label search incorrect", page, err)
	}
	for _, query := range []string{"ALPHA", "beta 中文", "COMMON"} {
		page, err = s.Search(context.Background(), query, "", 20)
		if err != nil || len(page.Choices) != 1 {
			t.Fatal("known path search is not case insensitive", query, page, err)
		}
	}
	page, err = s.Search(context.Background(), "not-in-catalog", "", 20)
	if err != nil || len(page.Choices) != 0 {
		t.Fatal("text search scanned unknown filesystem entries", page, err)
	}
	var paths []string
	for after := ""; ; {
		page, err = s.Search(context.Background(), "", after, 1)
		if err != nil || page.Path != "" || len(page.Choices) != 1 {
			t.Fatal("invalid known-path search page", page, err)
		}
		paths = append(paths, page.Choices[0].Path)
		if page.Next == "" {
			break
		}
		if page.Next != page.Choices[0].Path || page.Next <= after {
			t.Fatal("cursor was not canonical lexical last path", page)
		}
		after = page.Next
	}
	if !reflect.DeepEqual(paths, []string{a, b, c}) {
		t.Fatal("empty search did not page deduplicated known paths lexically", paths)
	}
	if _, err := s.Search(context.Background(), "", "relative", 1); !errors.Is(err, ErrDirectoryCursor) {
		t.Fatal("invalid cursor accepted", err)
	}
}

func TestDirectorySearchAbsoluteExactPrefixAndSymlinkScope(t *testing.T) {
	root := t.TempDir()
	project := directory(t, root, "项目 空格")
	directory(t, project, "nested")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	escape := filepath.Join(root, "pre-escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plain-file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	s := DirectoryService{Roots: []string{root}, Projects: func(context.Context) ([]codexclient.Project, error) {
		return []codexclient.Project{{ID: "p", Name: "项目", Roots: []codexclient.ProjectRoot{{Path: alias}}}}, nil
	}}
	page, err := s.Search(context.Background(), alias, "", 20)
	if err != nil || page.Path != project || page.Parent != root || len(page.Choices) != 1 || page.Choices[0].Path != project || page.Choices[0].ProjectID != "p" {
		t.Fatal("exact path did not resolve one canonical directory", page, err)
	}
	page, err = s.Search(context.Background(), alias, project, 20)
	if err != nil || len(page.Choices) != 0 || page.Next != "" {
		t.Fatal("exact search ignored lexical cursor", page, err)
	}
	for _, path := range []string{outside, escape, filepath.Join(escape, "absent-prefix")} {
		if _, err := s.Search(context.Background(), path, "", 20); !errors.Is(err, ErrDirectoryOutsideRoots) {
			t.Fatal("absolute path search escaped scope", err)
		}
	}
	if _, err := s.Search(context.Background(), filepath.Join(root, "plain-file"), "", 20); !errors.Is(err, ErrDirectoryUnavailable) {
		t.Fatal("existing file treated as directory prefix", err)
	}
	// Matches must be collected before paging, even with many earlier entries.
	for i := 0; i < 25; i++ {
		directory(t, root, fmt.Sprintf("earlier-%02d", i))
	}
	want := []string{}
	for i := 0; i < 23; i++ {
		want = append(want, directory(t, root, fmt.Sprintf("Pre-%02d", i)))
	}
	directory(t, want[0], "pre-nested")
	if err := os.Symlink(want[0], filepath.Join(root, "pre-alias")); err != nil {
		t.Fatal(err)
	}
	var got []string
	for after := ""; ; {
		page, err = s.Search(context.Background(), filepath.Join(root, "pre-"), after, 20)
		if err != nil || page.Path != root || page.Parent != "" || len(page.Choices) > 20 {
			t.Fatal("invalid prefix page", page, err)
		}
		got = append(got, directoryPaths(page.Choices)...)
		if page.Next == "" {
			break
		}
		if page.Next <= after {
			t.Fatal("prefix cursor failed to advance")
		}
		after = page.Next
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("prefix listing recursed, escaped, duplicated or missed entries", got)
	}
}

func TestDirectoryCatalogOfflineDoesNotBlockFilesystemOrKnownPaths(t *testing.T) {
	root := t.TempDir()
	child := directory(t, root, "child")
	offline := errors.New("catalog offline")
	s := DirectoryService{Roots: []string{root}, Common: []string{child}, Projects: func(context.Context) ([]codexclient.Project, error) { return nil, offline }}
	if choices, err := s.Quick(context.Background(), 20); err != nil || len(choices) != 1 || choices[0].ProjectID != "" {
		t.Fatal("offline catalog blocked quick paths", choices, err)
	}
	for _, query := range []string{"", "child", child, filepath.Join(root, "chi")} {
		if page, err := s.Search(context.Background(), query, "", 20); err != nil || len(page.Choices) != 1 || page.Choices[0].ProjectID != "" {
			t.Fatal("offline catalog blocked search", query, page, err)
		}
	}
	if page, err := s.List(context.Background(), root, "", 20); err != nil || len(page.Choices) != 1 {
		t.Fatal("offline catalog blocked browsing", page, err)
	}
	if _, err := s.Validate(context.Background(), child, "p"); !errors.Is(err, offline) {
		t.Fatal("explicit project validation failed open", err)
	}
}

func TestDirectoryListMatchingFiltersWholeLayerBeforePagination(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 25; i++ {
		directory(t, root, fmt.Sprintf("irrelevant-%02d", i))
	}
	want := []string{}
	for i := 0; i < 23; i++ {
		want = append(want, directory(t, root, fmt.Sprintf("prefix-Needle-%02d", i)))
	}
	directory(t, want[0], "nested-needle")
	aliasTarget := directory(t, root, "unrelated-name")
	if err := os.Symlink(aliasTarget, filepath.Join(root, "alias-NEEDLE")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape-needle")); err != nil {
		t.Fatal(err)
	}
	want = append(want, aliasTarget)
	sort.Strings(want)
	s := DirectoryService{Roots: []string{root}}
	got := []string{}
	for after := ""; ; {
		page, err := s.ListMatching(context.Background(), root, "nEeDlE", after, 20)
		if err != nil || page.Path != root || len(page.Choices) > 20 || page.Total != len(want) || page.Offset != len(got) {
			t.Fatal("invalid filtered directory page", page, err)
		}
		got = append(got, directoryPaths(page.Choices)...)
		if page.Next == "" {
			break
		}
		if page.Next <= after {
			t.Fatal("filtered cursor failed to advance")
		}
		after = page.Next
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("filter missed matches after page one, recursed or escaped", got)
	}
	plain, err := s.List(context.Background(), root, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	unfiltered, err := s.ListMatching(context.Background(), root, "", "", 20)
	if err != nil || !reflect.DeepEqual(plain, unfiltered) {
		t.Fatal("empty filter changed List contract", plain, unfiltered, err)
	}
}

func TestDirectoryQuickAndSearchBoundsAndCancellation(t *testing.T) {
	s := DirectoryService{Common: make([]string, maxDirectoryCandidates+1)}
	if _, err := s.Quick(context.Background(), 1); !errors.Is(err, ErrDirectoryLimit) {
		t.Fatal("unbounded quick candidates", err)
	}
	if _, err := s.Search(context.Background(), "anything", "", 1); !errors.Is(err, ErrDirectoryLimit) {
		t.Fatal("unbounded known search candidates", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, query := range []string{"", "/missing-prefix"} {
		if _, err := s.Search(ctx, query, "", 1); !errors.Is(err, context.Canceled) {
			t.Fatal("search cancellation ignored", err)
		}
	}
	if _, err := s.Quick(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("quick cancellation ignored", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	s = DirectoryService{Projects: func(context.Context) ([]codexclient.Project, error) { cancel(); return nil, errors.New("unavailable") }}
	if _, err := s.Search(ctx, "", "", 1); !errors.Is(err, context.Canceled) {
		t.Fatal("catalog cancellation swallowed as optional enrichment", err)
	}
}

func TestDirectoryQuickSkipsReadOnlyWhileSearchAllowsBrowsing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses ordinary directory access bits")
	}
	root := t.TempDir()
	readOnly := directory(t, root, "read-only")
	writable := directory(t, readOnly, "child")
	t.Cleanup(func() { os.Chmod(readOnly, 0700) })
	if err := os.Chmod(readOnly, 0500); err != nil {
		t.Fatal(err)
	}
	s := DirectoryService{Roots: []string{root}, Recent: []string{readOnly, writable}}
	if choices, err := s.Quick(context.Background(), 20); err != nil || !reflect.DeepEqual(directoryPaths(choices), []string{writable}) {
		t.Fatal("read-only directory was selectable shortcut", choices, err)
	}
	for _, query := range []string{"read-only", readOnly, filepath.Join(root, "read-")} {
		page, err := s.Search(context.Background(), query, "", 20)
		if err != nil || len(page.Choices) == 0 || page.Choices[0].Path != readOnly || !page.Choices[0].ReadOnly {
			t.Fatal("search hid browsable read-only directory", query, page, err)
		}
	}
	page, err := s.Search(context.Background(), filepath.Join(readOnly, "chi"), "", 20)
	if err != nil || !page.ReadOnly || len(page.Choices) != 1 || page.Choices[0].Path != writable || page.Choices[0].ReadOnly {
		t.Fatal("read-only parent blocked writable prefix match", page, err)
	}
	if _, err := s.Validate(context.Background(), readOnly, ""); !errors.Is(err, ErrDirectoryUnavailable) {
		t.Fatal("search bypassed final write check", err)
	}
}
