package creation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"codex-feishu/internal/codexclient"
)

func directory(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDirectoryPhysicalScopeAndProjectIdentity(t *testing.T) {
	base := t.TempDir()
	root := directory(t, base, "repo")
	child := directory(t, root, "中文 空格")
	other := directory(t, base, "repo-other")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(child, alias); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(other, escape); err != nil {
		t.Fatal(err)
	}
	s := DirectoryService{Roots: []string{root}, Projects: func(context.Context) ([]codexclient.Project, error) {
		return []codexclient.Project{{ID: "p", Name: "真实项目", Roots: []codexclient.ProjectRoot{{Path: alias}}}}, nil
	}}
	choice, err := s.Validate(context.Background(), alias, "p")
	if err != nil || choice.Path != child || choice.ProjectID != "p" || choice.Label != "真实项目" {
		t.Fatal("canonical project root not retained", choice, err)
	}
	for _, path := range []string{other, escape} {
		if _, err := s.Validate(context.Background(), path, ""); !errors.Is(err, ErrDirectoryOutsideRoots) {
			t.Fatal("scope escape accepted", err)
		}
	}
	for _, path := range []string{"relative", filepath.Join(root, "missing"), root + "\n"} {
		if _, err := s.Validate(context.Background(), path, ""); !errors.Is(err, ErrDirectoryUnavailable) {
			t.Fatal("unavailable path accepted", err)
		}
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Validate(context.Background(), file, ""); !errors.Is(err, ErrDirectoryUnavailable) {
		t.Fatal("file accepted", err)
	}
	for _, p := range []struct{ path, id string }{{root, "p"}, {child, "forged"}} {
		if _, err := s.Validate(context.Background(), p.path, p.id); !errors.Is(err, ErrDirectoryProject) {
			t.Fatal("unverified project accepted", err)
		}
	}
	independent, err := s.Validate(context.Background(), child, "")
	if err != nil || independent.ProjectID != "" {
		t.Fatal("independent directory gained project", err)
	}
}

func TestDirectoryReadWriteSearchAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses normal directory access bits")
	}
	root := t.TempDir()
	child := directory(t, root, "restricted")
	s := DirectoryService{Roots: []string{root}}
	for _, mode := range []os.FileMode{0500, 0300, 0600} {
		if err := os.Chmod(child, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Validate(context.Background(), child, ""); !errors.Is(err, ErrDirectoryUnavailable) {
			t.Fatal("missing R/W/X accepted", mode, err)
		}
	}
	if err := os.Chmod(child, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Validate(context.Background(), child, ""); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryDefaultsOnlyRecentAndCommon(t *testing.T) {
	root := t.TempDir()
	common := directory(t, root, "common")
	recent := directory(t, root, "recent")
	s := DirectoryService{Roots: []string{root}, Common: []string{filepath.Join(root, "missing"), common}}
	choice, err := s.Default(context.Background(), recent)
	if err != nil || choice.Path != recent {
		t.Fatal("valid recent not used", err)
	}
	choice, err = s.Default(context.Background(), "/does-not-exist")
	if err != nil || choice.Path != common {
		t.Fatal("valid common not used", err)
	}
	s.Common = nil
	choice, err = s.Default(context.Background(), "")
	if err != nil || choice.Path != "" {
		t.Fatal("default guessed a root or cwd", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.Default(ctx, recent); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
}

func TestDirectoryRootListDeduplicatesAndPages(t *testing.T) {
	root := t.TempDir()
	project := directory(t, root, "project")
	common := directory(t, root, "常用 路径")
	outside := t.TempDir()
	alias := filepath.Join(root, "project-alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	s := DirectoryService{Roots: []string{root}, Common: []string{common, alias, outside}, Projects: func(context.Context) ([]codexclient.Project, error) {
		return []codexclient.Project{{ID: "p", Name: "项目", Roots: []codexclient.ProjectRoot{{Path: project}, {Path: alias}, {Path: outside}}}}, nil
	}}
	var all []DirectoryChoice
	after := ""
	for {
		page, err := s.List(context.Background(), "", after, 1)
		if err != nil {
			t.Fatal(err)
		}
		if page.Path != "" || page.Parent != "" || len(page.Choices) > 1 {
			t.Fatal("bad root page")
		}
		all = append(all, page.Choices...)
		if page.Next == "" {
			break
		}
		if page.Next == after {
			t.Fatal("cursor did not advance")
		}
		after = page.Next
	}
	if len(all) != 3 || all[0].Path != root || all[1].Path != project || all[1].ProjectID != "p" || all[2].Path != common {
		t.Fatal("root candidates not scoped and deduplicated", all)
	}
}

func TestDirectoryBrowseOnlyDirectChildrenAndSymlinks(t *testing.T) {
	root := t.TempDir()
	a := directory(t, root, "a")
	nested := directory(t, a, "never-scan-this")
	b := directory(t, root, "b 空格")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	s := DirectoryService{Roots: []string{root}}
	page, err := s.List(context.Background(), root, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, choice := range page.Choices {
		got = append(got, choice.Path)
		if choice.Path == nested {
			t.Fatal("recursive listing")
		}
	}
	if !reflect.DeepEqual(got, []string{a, b}) || page.Path != root || page.Parent != "" {
		t.Fatal("incorrect scoped direct children", page)
	}
	child, err := s.List(context.Background(), a, "", 10)
	if err != nil || child.Parent != root || len(child.Choices) != 1 || child.Choices[0].Path != nested {
		t.Fatal("parent navigation incorrect", child, err)
	}
	if _, err := s.List(context.Background(), outside, "", 10); !errors.Is(err, ErrDirectoryOutsideRoots) {
		t.Fatal("outside directory browsed", err)
	}
	if _, err := s.List(context.Background(), root, "bad-cursor", 10); !errors.Is(err, ErrDirectoryCursor) {
		t.Fatal("invalid cursor accepted", err)
	}
}

func TestDirectoryListingBoundAndCancellation(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < maxDirectoryCandidates+1; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%04d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := DirectoryService{Roots: []string{root}}
	if _, err := s.List(context.Background(), root, "", 1); !errors.Is(err, ErrDirectoryLimit) {
		t.Fatal("unbounded candidate listing", err)
	}
	if _, err := s.Search(context.Background(), filepath.Join(root, "missing-prefix"), "", 1); !errors.Is(err, ErrDirectoryLimit) {
		t.Fatal("unbounded prefix search listing", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.List(ctx, root, "", 10); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
}

func TestDirectoryReadOnlyParentsBrowseToWritableDescendants(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses ordinary directory access bits")
	}
	root := t.TempDir()
	writable := directory(t, root, "writable 中文")
	readOnly := directory(t, root, "read-only")
	grandchild := directory(t, readOnly, "writable-child")
	alias := filepath.Join(root, "inside-alias")
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(writable, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), escape); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0700); os.Chmod(readOnly, 0700) })
	if err := os.Chmod(readOnly, 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	s := DirectoryService{Roots: []string{root}, Common: []string{readOnly, alias}, Projects: func(context.Context) ([]codexclient.Project, error) {
		return []codexclient.Project{{ID: "p", Name: "read-only project", Roots: []codexclient.ProjectRoot{{Path: readOnly}}}}, nil
	}}
	choice, err := s.Browse(context.Background(), root)
	if err != nil || !choice.ReadOnly || choice.Path != root {
		t.Fatal("read-only scope root was not browsable", err)
	}
	if _, err := s.Validate(context.Background(), root, ""); !errors.Is(err, ErrDirectoryUnavailable) {
		t.Fatal("read-only root was selectable", err)
	}
	if _, err := s.Validate(context.Background(), readOnly, "p"); !errors.Is(err, ErrDirectoryUnavailable) {
		t.Fatal("project assignment made a read-only root selectable", err)
	}
	roots, err := s.List(context.Background(), "", "", 20)
	if err != nil || len(roots.Choices) != 3 {
		t.Fatal("read-only roots/common candidates hidden", err)
	}
	for _, entry := range roots.Choices {
		if entry.ReadOnly != (entry.Path != writable) {
			t.Fatal("incorrect candidate write capability", entry)
		}
	}
	page, err := s.List(context.Background(), root, "", 20)
	if err != nil || !page.ReadOnly || len(page.Choices) != 2 {
		t.Fatal("read-only parent could not list direct children", err)
	}
	for _, entry := range page.Choices {
		if entry.Path == grandchild || entry.Path == escape {
			t.Fatal("listing recursed or escaped through a symlink")
		}
	}
	next, err := s.List(context.Background(), readOnly, "", 20)
	if err != nil || !next.ReadOnly || next.Parent != root || len(next.Choices) != 1 || next.Choices[0].Path != grandchild || next.Choices[0].ReadOnly {
		t.Fatal("read-only intermediate directory hid writable descendant", err)
	}
	selected, err := s.Validate(context.Background(), grandchild, "")
	if err != nil || selected.ReadOnly {
		t.Fatal("writable descendant was not selectable", err)
	}
	if selected, err = s.Default(context.Background(), root); err != nil || selected.Path != writable || selected.ReadOnly {
		t.Fatal("default did not skip read-only recent/common paths", err)
	}
	if selected, err = (DirectoryService{Roots: []string{root}}).Default(context.Background(), root); err != nil || selected.Path != "" {
		t.Fatal("read-only recent path became an execution default", err)
	}
	if selected, err = s.Browse(context.Background(), alias); err != nil || selected.Path != writable {
		t.Fatal("in-scope alias was not normalized", err)
	}
	if _, err = s.Browse(context.Background(), escape); !errors.Is(err, ErrDirectoryOutsideRoots) {
		t.Fatal("read-only browsing widened symlink scope", err)
	}
}

func TestDirectoryBrowseRejectsMissingReadOrSearch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses ordinary directory access bits")
	}
	root := t.TempDir()
	child := directory(t, root, "restricted")
	t.Cleanup(func() { os.Chmod(child, 0700) })
	s := DirectoryService{Roots: []string{root}}
	for _, mode := range []os.FileMode{0300, 0600} {
		if err := os.Chmod(child, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Browse(context.Background(), child); !errors.Is(err, ErrDirectoryUnavailable) {
			t.Fatal("unreadable/unsearchable directory browsed", mode, err)
		}
		if _, err := s.List(context.Background(), child, "", 10); !errors.Is(err, ErrDirectoryUnavailable) {
			t.Fatal("unreadable/unsearchable directory listed", mode, err)
		}
		if _, err := s.Search(context.Background(), child, "", 10); !errors.Is(err, ErrDirectoryUnavailable) {
			t.Fatal("unreadable/unsearchable exact search target accepted", mode, err)
		}
		if _, err := s.Search(context.Background(), filepath.Join(child, "prefix"), "", 10); !errors.Is(err, ErrDirectoryUnavailable) {
			t.Fatal("unreadable/unsearchable prefix parent accepted", mode, err)
		}
		page, err := s.List(context.Background(), root, "", 10)
		if err != nil || len(page.Choices) != 0 {
			t.Fatal("unbrowsable child was offered as a candidate", err)
		}
	}
}

func TestDirectoryManagerBrowsesReadOnlyRootThenSelectsWritableChild(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses ordinary directory access bits")
	}
	h := newManagerHarness(t)
	child := directory(t, h.root, "selected")
	t.Cleanup(func() { os.Chmod(h.root, 0700) })
	if err := os.Chmod(h.root, 0500); err != nil {
		t.Fatal(err)
	}
	d := h.receive("read-only-root-source", "task")
	if d.Directory != "" {
		t.Fatal("read-only common directory became execution default")
	}
	if err := h.m.Action(context.Background(), d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_browse", map[string]any{"path": h.root}, nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.View != "directories" || d.BrowsePath != h.root || d.ErrorCode != "" {
		t.Fatal("browse callback still required a writable parent")
	}
	if err := h.m.Action(context.Background(), d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_pick", map[string]any{"path": h.root}, nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.Directory != "" || d.ErrorCode != "directory" || len(h.n.starts) != 0 {
		t.Fatal("read-only parent was selectable through callback")
	}
	if err := h.m.Action(context.Background(), d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_pick", map[string]any{"path": child}, nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	h.submit(d)
	if h.draft(d.SourceMessageID).State != "created" || len(h.n.starts) != 1 || h.n.starts[0].CWD != child {
		t.Fatal("writable descendant could not be selected and submitted")
	}
}

func TestDirectoryManagerRejectsSelectedDirectoryBecomingReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses ordinary directory access bits")
	}
	h := newManagerHarness(t)
	d := h.receive("selected-before-permission-change", "task")
	if d.Directory != h.root {
		t.Fatal("fixture did not select the writable default")
	}
	t.Cleanup(func() { os.Chmod(h.root, 0700) })
	if err := os.Chmod(h.root, 0500); err != nil {
		t.Fatal(err)
	}
	h.submit(d)
	d = h.draft(d.SourceMessageID)
	if d.State != "draft" || d.ErrorCode != "directory" || h.g.groups != 0 || len(h.n.starts) != 0 || h.count(`SELECT COUNT(*) FROM creation_workflows`) != 0 {
		t.Fatal("submit reused a directory that lost write access")
	}
}
