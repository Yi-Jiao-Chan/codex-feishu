package creation

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"codex-feishu/internal/store"
)

// BrowsePath holds the editable query. The cursor freezes the last executed
// search separately, so deleting an attachment preserves an unsubmitted edit
// without silently changing either the result set or its search root.
type inlineDirectoryPosition struct {
	Query string `json:"query"`
	Root  string `json:"root,omitempty"`
	After string `json:"after,omitempty"`
}

func parseInlineDirectoryPosition(raw string) (inlineDirectoryPosition, error) {
	var p inlineDirectoryPosition
	if raw == "" {
		return p, nil
	}
	if json.Unmarshal([]byte(raw), &p) != nil || validateDirectoryFilter(p.Query) != nil || !validDirectoryCursor(p.After) || p.Root != "" && !validDirectoryCursor(p.Root) {
		return inlineDirectoryPosition{}, ErrDirectoryCursor
	}
	return p, nil
}

func applyWorkspaceForm(d *store.CreationDraft, form map[string]any) error {
	if raw, supplied := form["creation_use_worktree"]; supplied {
		checked, ok := raw.(bool)
		if !ok {
			return store.ErrSubmissionStale
		}
		d.WorkspaceMode = "local"
		if checked {
			d.WorkspaceMode = "worktree"
		} else {
			d.BaseBranch = ""
		}
	}
	if raw, supplied := form["creation_branch"]; supplied && d.WorkspaceMode == "worktree" {
		branch, ok := raw.(string)
		if !ok || utf8.RuneCountInString(branch) > 1000 {
			return ErrWorktreeBranch
		}
	}
	return nil
}

func attachmentIndex(raw any, count int) (int, bool) {
	// Persisted action payloads decode numbers as float64. Accept no string,
	// fractional, negative, or out-of-range index from a callback.
	value, ok := raw.(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= float64(count) || math.Trunc(value) != value {
		return 0, false
	}
	return int(value), true
}

func (m *Manager) inlineSearchRoot(ctx context.Context, selected string) (string, error) {
	dirs := m.directories(nil)
	if selected != "" {
		choice, err := dirs.Browse(ctx, selected)
		return choice.Path, err
	}
	for _, root := range dirs.Roots {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if choice, err := dirs.Browse(ctx, root); err == nil {
			return choice.Path, nil
		}
	}
	return "", ErrDirectoryUnavailable
}

func (m *Manager) positionInlineDirectory(ctx context.Context, d *store.CreationDraft, kind string, value map[string]any) error {
	query := d.BrowsePath
	if query == "" {
		d.BrowseCursor = ""
		return nil
	}
	previous, err := parseInlineDirectoryPosition(d.BrowseCursor)
	if err != nil {
		return err
	}
	position := inlineDirectoryPosition{Query: query}
	if kind == "creation_find_next" && query == previous.Query {
		position.Root, position.After = previous.Root, textValue(value, "after")
	}
	if !validDirectoryCursor(position.After) {
		return ErrDirectoryCursor
	}
	if !filepath.IsAbs(query) && position.Root == "" {
		position.Root, err = m.inlineSearchRoot(ctx, d.Directory)
	}
	// Persist the query even when its path cannot be read. The subsequent
	// rendering reports the failure while retaining the last selected cwd.
	raw, _ := json.Marshal(position)
	d.BrowseCursor = string(raw)
	return err
}

func (m *Manager) findInlineDirectory(ctx context.Context, d *store.CreationDraft, kind string, value map[string]any) error {
	if err := m.positionInlineDirectory(ctx, d, kind, value); err != nil {
		return err
	}
	if d.BrowsePath == "" {
		return nil
	}
	_, err := inlineDirectoryPage(ctx, m.directories(nil), *d)
	return err
}

func inlineDirectoryPage(ctx context.Context, dirs DirectoryService, d store.CreationDraft) (DirectoryPage, error) {
	position, err := parseInlineDirectoryPosition(d.BrowseCursor)
	if err != nil {
		return DirectoryPage{}, err
	}
	if position.Query == "" {
		choices, err := dirs.Quick(ctx, 20)
		if err != nil {
			return DirectoryPage{}, err
		}
		// Always include the current validated selection among recent choices.
		// Search results do not use this fallback: an unrelated old cwd must
		// not masquerade as a matching result.
		if d.Directory != "" {
			if current, err := dirs.Validate(ctx, d.Directory, ""); err == nil {
				all := []DirectoryChoice{current}
				for _, choice := range choices {
					if choice.Path != current.Path {
						all = append(all, choice)
					}
				}
				choices = all[:min(len(all), 20)]
			}
		}
		return DirectoryPage{Path: d.Directory, Choices: choices, Total: len(choices)}, ctx.Err()
	}
	if filepath.IsAbs(position.Query) {
		choice, err := dirs.Validate(ctx, position.Query, "")
		if err != nil {
			return DirectoryPage{}, err
		}
		page := DirectoryPage{Path: choice.Path, Total: 1}
		if choice.Path > position.After {
			page.Choices = []DirectoryChoice{choice}
		} else {
			page.Offset = 1
		}
		return page, ctx.Err()
	}
	if position.Root == "" || strings.TrimSpace(position.Query) == "" {
		return DirectoryPage{}, ErrDirectoryUnavailable
	}
	return dirs.SearchRecursive(ctx, position.Root, position.Query, position.After, 100)
}
