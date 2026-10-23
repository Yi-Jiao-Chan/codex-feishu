package creation

import (
	"codex-feishu/internal/cards"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Recents are local to the authenticated creator and deployment. A card never
// pulls another operator's directory history into its suggestions.
func (m *Manager) selectionDirectories(ctx context.Context, n Native, creator string) (DirectoryService, error) {
	dirs := m.directories(n)
	rows, err := m.Store.DB.QueryContext(ctx, `SELECT directory FROM creation_drafts WHERE host_id=? AND app_id=? AND creator_id=? AND state='created' ORDER BY created_at_ms DESC,draft_id DESC LIMIT 20`, m.Config.Host.ID, m.Config.Feishu.AppID, creator)
	if err != nil {
		return dirs, err
	}
	defer rows.Close()
	for rows.Next() {
		var directory string
		if err := rows.Scan(&directory); err != nil {
			return dirs, err
		}
		dirs.Recent = append(dirs.Recent, directory)
	}
	return dirs, rows.Err()
}

// Project assignment comes from the server catalog and must match the real
// root exactly. A transient catalog outage still permits an independent cwd.
func selectDirectory(ctx context.Context, dirs DirectoryService, path string) (DirectoryChoice, error) {
	choice, err := dirs.Validate(ctx, path, "")
	if err != nil {
		return choice, err
	}
	if dirs.Projects != nil {
		projects, err := dirs.Projects(ctx)
		if err == nil {
			choice = directoryProject(choice, projects)
		}
		if err := ctx.Err(); err != nil {
			return DirectoryChoice{}, err
		}
	}
	return choice, nil
}

func cardDirectoryChoice(c DirectoryChoice) cards.CreationDirectoryChoice {
	return cards.CreationDirectoryChoice{Path: c.Path, ProjectID: c.ProjectID, Label: c.Label, ReadOnly: c.ReadOnly}
}

func cardDirectoryPage(p DirectoryPage) *cards.CreationDirectoryPage {
	out := &cards.CreationDirectoryPage{Path: p.Path, Parent: p.Parent, Next: p.Next, ReadOnly: p.ReadOnly, Total: p.Total, Offset: p.Offset}
	for _, choice := range p.Choices {
		out.Choices = append(out.Choices, cardDirectoryChoice(choice))
	}
	return out
}

// No selected value is a
// proof of authorization. It is validated again before native dispatch.
func resetDirectoryView(d *store.CreationDraft, choice DirectoryChoice) {
	if d.Directory != choice.Path {
		d.BaseBranch = ""
	}
	d.Directory, d.ProjectID, d.View, d.BrowsePath, d.BrowseCursor = choice.Path, choice.ProjectID, "main", "", ""
}

var ErrSearchSelectionRequired = errors.New("creation search result must be selected before creating")

type directoryPosition struct {
	Filter string `json:"filter"`
	After  string `json:"after"`
}

func validateDirectoryFilter(filter string) error {
	if utf8.RuneCountInString(filter) > 1000 || strings.IndexFunc(filter, unicode.IsControl) >= 0 {
		return ErrDirectoryCursor
	}
	return nil
}

func directoryCursor(filter, after string) string {
	if filter == "" {
		return after
	}
	raw, _ := json.Marshal(directoryPosition{Filter: filter, After: after})
	return string(raw)
}

// Older cards stored only the canonical path cursor. JSON adds a filter to
// that same durable position without changing the database schema.
func parseDirectoryCursor(raw string) (filter, after string, err error) {
	if raw == "" || filepath.IsAbs(raw) {
		return "", raw, nil
	}
	var p directoryPosition
	if json.Unmarshal([]byte(raw), &p) != nil || validateDirectoryFilter(p.Filter) != nil || !validDirectoryCursor(p.After) {
		return "", "", ErrDirectoryCursor
	}
	return p.Filter, p.After, nil
}

// Keep the scope root and nearest ancestors as compact, validated navigation.
// Full paths stay in callback values; labels are presentation only.
func directoryBreadcrumbs(ctx context.Context, dirs DirectoryService, path string) []cards.CreationOption {
	if path == "" {
		return nil
	}
	var root string
	for _, candidate := range dirs.Roots {
		choice, err := dirs.Browse(ctx, candidate)
		if err == nil && rules.Inside(choice.Path, path) && (root == "" || len(choice.Path) < len(root)) {
			root = choice.Path
		}
	}
	paths := []string{path}
	for i := 0; i < 2; i++ {
		parent := filepath.Dir(paths[0])
		if parent == paths[0] {
			break
		}
		if choice, err := dirs.Browse(ctx, parent); err == nil {
			paths = append([]string{choice.Path}, paths...)
		} else {
			break
		}
	}
	if root != "" && root != paths[0] {
		paths = append([]string{root}, paths...)
	}
	result := make([]cards.CreationOption, 0, len(paths))
	for _, p := range paths {
		result = append(result, cards.CreationOption{Label: filepath.Base(p), Value: p})
	}
	return result
}

// A failed directory lookup must not erase the task edits submitted with it.
// Only safe draft fields are preserved; an invalid selected path never lands
// in the draft, and no remote creation is claimed here.
func (m *Manager) rejectSelectionAction(ctx context.Context, a store.CreationAction, cause error) error {
	d, err := m.Store.CreationDraft(ctx, a.DraftID)
	if err != nil {
		return err
	}
	if d.State != "draft" || d.Revision != a.Revision {
		return m.Store.RefreshCreationAction(ctx, a, actionError(cause))
	}
	var p actionPayload
	if json.Unmarshal([]byte(a.Payload), &p) != nil {
		return m.Store.RefreshCreationAction(ctx, a, actionError(cause))
	}
	if body, ok := p.Form["creation_body"].(string); ok && utf8.RuneCountInString(strings.TrimSpace(body)) <= MaxTaskRunes {
		d.Body = strings.TrimSpace(body)
	}
	if mode := textValue(p.Form, "creation_mode"); mode == "group" || mode == "topic" && m.Config.Creation.TopicsEnabled {
		d.FollowMode = mode
	}
	if target, ok := p.Form["creation_target"].(string); ok && (target == "" || contains(m.Config.Creation.WorkgroupChatIDs, target)) {
		d.TargetChatID = target
	}
	// The inline form keeps independently valid edits when a different field
	// fails validation. No invalid directory or malformed checkbox is saved.
	if d.View == "main" {
		if path := textValue(p.Form, "creation_directory"); path != "" && path != d.Directory {
			if choice, err := m.directories(nil).Validate(ctx, path, ""); err == nil {
				d.Directory, d.ProjectID, d.BaseBranch = choice.Path, "", ""
			}
		}
		if checked, ok := p.Form["creation_use_worktree"].(bool); ok {
			d.WorkspaceMode = "local"
			if checked {
				d.WorkspaceMode = "worktree"
			} else {
				d.BaseBranch = ""
			}
		}
		if query, ok := p.Form["creation_query"].(string); ok && validateDirectoryFilter(query) == nil {
			d.BrowsePath = strings.TrimSpace(query)
		}
		if a.Kind == "creation_find" || a.Kind == "creation_find_next" {
			_ = m.positionInlineDirectory(ctx, &d, a.Kind, p.Value)
		}
	}
	if branch, ok := p.Form["creation_branch"].(string); ok && d.WorkspaceMode == "worktree" && (branch == "HEAD" || strings.HasPrefix(branch, "refs/heads/") || strings.HasPrefix(branch, "refs/remotes/")) && utf8.RuneCountInString(branch) <= 1000 {
		d.BaseBranch = branch
	}
	if a.Kind == "creation_search" || a.Kind == "creation_search_next" {
		query := strings.TrimSpace(textValue(p.Form, "creation_query"))
		if utf8.RuneCountInString(query) <= 1000 {
			d.View, d.BrowsePath, d.BrowseCursor = "search", query, ""
		}
	}
	d.ErrorCode = actionError(cause)
	_, err = m.Store.UpdateCreationDraft(ctx, d, a.Revision, m.now().UnixMilli())
	return err
}
