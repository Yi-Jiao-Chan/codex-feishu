package creation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"codex-feishu/internal/store"
)

func inlineCardNode(t *testing.T, card, tag, name string) map[string]any {
	t.Helper()
	var root any
	if err := json.Unmarshal([]byte(card), &root); err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	var visit func(any)
	visit = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			if node["tag"] == tag && node["name"] == name {
				if found != nil {
					t.Fatalf("duplicate %s %s", tag, name)
				}
				found = node
			}
			for _, child := range node {
				visit(child)
			}
		case []any:
			for _, child := range node {
				visit(child)
			}
		}
	}
	visit(root)
	if found == nil {
		t.Fatalf("missing %s %s", tag, name)
	}
	return found
}

func inlineDirectoryValues(t *testing.T, h *managerHarness, d store.CreationDraft) []string {
	t.Helper()
	node := inlineCardNode(t, h.g.cards[d.CardID].Content, "select_static", "creation_directory")
	var paths []string
	for _, option := range node["options"].([]any) {
		paths = append(paths, option.(map[string]any)["value"].(string))
	}
	return paths
}

func TestInlineCreationDefaultsToWorktreeAndCanUncheck(t *testing.T) {
	for _, worktree := range []bool{true, false} {
		t.Run(fmt.Sprintf("worktree=%t", worktree), func(t *testing.T) {
			h := newManagerHarness(t)
			h.m.Config.Storage.DatabaseFile = h.db
			commit := initWorktreeTestRepo(t, h.root)
			d := h.receive("inline-workspace", "implement fixture")
			checker := inlineCardNode(t, h.g.cards[d.CardID].Content, "checker", "creation_use_worktree")
			if d.WorkspaceMode != "worktree" || checker["checked"] != true {
				t.Fatal("new draft did not default to worktree")
			}
			// No separate mode action is needed. The form carries the checkbox,
			// and a missing branch resolves the displayed current branch.
			form := map[string]any{"creation_directory": h.root, "creation_use_worktree": worktree}
			if !worktree {
				form["creation_branch"] = "refs/heads/main"
			}
			if err := h.action(d, "creation_submit", form); err != nil {
				t.Fatal(err)
			}
			if err := h.m.tickUI(context.Background()); err != nil {
				t.Fatal(err)
			}
			i := h.intent(d)
			if worktree {
				if i.WorkspaceMode != "worktree" || i.BaseBranch != "refs/heads/main" || i.BaseCommit != commit || i.WorktreePath == "" {
					t.Fatal("checked form did not freeze the default branch and checkout")
				}
			} else if i.WorkspaceMode != "local" || i.BaseBranch != "" || i.WorktreePath != "" {
				t.Fatal("unchecked form retained worktree state")
			}
			h.g.expectedDirectory = i.ExecutionDirectory()
			h.tick()
			if len(h.n.starts) != 1 || h.n.starts[0].CWD != i.ExecutionDirectory() || h.intent(d).State != "confirmed" {
				t.Fatal("inline workspace choice did not reach the intended cwd")
			}
		})
	}
}

func TestInlineSearchReplacesRecentChoicesAndRequiresExplicitSelection(t *testing.T) {
	h := newManagerHarness(t)
	fourth := filepath.Join(h.root, "a", "b", "c", "needle")
	fifth := filepath.Join(h.root, "a", "b", "c", "d", "needle")
	for _, path := range []string{fourth, fifth} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	d := h.receive("inline-search", "original task")
	if got := inlineDirectoryValues(t, h, d); !reflect.DeepEqual(got, []string{h.root}) {
		t.Fatal("recent choices did not start with the configured default", got)
	}
	reads := h.n.reads
	if err := h.action(d, "creation_find", map[string]any{"creation_query": "needle", "creation_body": "edited task", "creation_use_worktree": false}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.View != "main" || d.Directory != h.root || d.Body != "edited task" || d.WorkspaceMode != "local" || h.n.reads != reads {
		t.Fatal("inline search changed the selection, lost edits, or queried the project catalog")
	}
	if got := inlineDirectoryValues(t, h, d); !reflect.DeepEqual(got, []string{fourth}) {
		t.Fatal("search did not replace the selector with depth-bounded results", got)
	}
	selector := inlineCardNode(t, h.g.cards[d.CardID].Content, "select_static", "creation_directory")
	if selector["initial_option"] != nil {
		t.Fatal("unmatched old cwd remained selected after search")
	}
	if err := h.action(d, "creation_submit", map[string]any{"creation_use_worktree": false}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.State != "draft" || d.ErrorCode != "directory-selection" || h.g.groups != 0 || len(h.n.starts) != 0 {
		t.Fatal("missing selection created a task in the old cwd")
	}
	// Absolute input is a single exact candidate, including directories beyond
	// the name search depth. It still needs an explicit selection.
	if err := h.action(d, "creation_find", map[string]any{"creation_query": fifth}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if got := inlineDirectoryValues(t, h, d); !reflect.DeepEqual(got, []string{fifth}) || d.Directory != h.root {
		t.Fatal("absolute search did not offer one uncommitted candidate", got)
	}
	h.g.expectedDirectory = fifth
	if err := h.action(d, "creation_submit", map[string]any{"creation_directory": fifth, "creation_use_worktree": false}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if len(h.n.starts) != 1 || h.n.starts[0].CWD != fifth {
		t.Fatal("final submission did not use the selected search result")
	}
}

func TestInlineSearchFailurePreservesEditsAndDoesNotCreate(t *testing.T) {
	for _, query := range []string{"no-matching-name", "/outside-the-authorized-fixture"} {
		t.Run(query, func(t *testing.T) {
			h := newManagerHarness(t)
			d := h.receive("inline-failed-search", "original")
			if err := h.action(d, "creation_find", map[string]any{"creation_query": query, "creation_body": "keep these edits", "creation_directory": h.root, "creation_use_worktree": false}); err != nil {
				t.Fatal(err)
			}
			h.tick()
			d = h.draft(d.SourceMessageID)
			if d.Body != "keep these edits" || d.Directory != h.root || d.WorkspaceMode != "local" || d.BrowsePath != query || len(inlineDirectoryValues(t, h, d)) != 0 {
				t.Fatal("empty or failed search erased edits or fell back to recent choices")
			}
			if err := h.action(d, "creation_submit", nil); err != nil {
				t.Fatal(err)
			}
			h.tick()
			if h.g.groups != 0 || len(h.n.starts) != 0 || h.draft(d.SourceMessageID).State != "draft" {
				t.Fatal("empty search accidentally created a task")
			}
		})
	}
}

func TestInlineSearchPagingKeepsOriginalRoot(t *testing.T) {
	h := newManagerHarness(t)
	for i := 0; i < 105; i++ {
		if err := os.Mkdir(filepath.Join(h.root, fmt.Sprintf("match-%03d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	d := h.receive("inline-pagination", "task")
	d.BrowsePath = "match"
	ctx := context.Background()
	if err := h.m.findInlineDirectory(ctx, &d, "creation_find", nil); err != nil {
		t.Fatal(err)
	}
	page, err := inlineDirectoryPage(ctx, h.m.directories(nil), d)
	if err != nil || len(page.Choices) != 100 || page.Next == "" {
		t.Fatal("fixture did not produce a paged result", err)
	}
	d.Directory = page.Choices[0].Path // A choice submitted alongside Next.
	if err := h.m.findInlineDirectory(ctx, &d, "creation_find_next", map[string]any{"after": page.Next}); err != nil {
		t.Fatal(err)
	}
	page, err = inlineDirectoryPage(ctx, h.m.directories(nil), d)
	position, parseErr := parseInlineDirectoryPosition(d.BrowseCursor)
	if err != nil || parseErr != nil || position.Root != h.root || len(page.Choices) != 5 || page.Choices[0].Path != filepath.Join(h.root, "match-100") {
		t.Fatal("next page restarted the search inside the new selection", err, parseErr)
	}
}

func TestInlineDeletePreservesUnsubmittedFormAndRejectsReplay(t *testing.T) {
	h := newManagerHarness(t)
	h.m.Config.Attachments.Enabled = true
	h.m.Config.Creation.TopicsEnabled = true
	h.m.Config.Creation.WorkgroupChatIDs = []string{"oc_work"}
	h.g.workgroups = map[string]string{"oc_work": "fixture workgroup"}
	d := receiveCreationAttachments(t, h)
	if err := h.action(d, "creation_find", map[string]any{"creation_query": "missing"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	// Feature disablement must not prevent removing already attached items.
	h.m.Config.Attachments.Enabled = false
	form := map[string]any{"creation_body": "unsaved task", "creation_mode": "topic", "creation_target": "oc_work", "creation_directory": h.root, "creation_use_worktree": false, "creation_branch": "refs/heads/old", "creation_query": "unsearched edit"}
	action := func(d store.CreationDraft, index float64) error {
		return h.m.Action(context.Background(), d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_remove_attachment", map[string]any{"index": index}, form)
	}
	if err := action(d, 0); err != nil {
		t.Fatal(err)
	}
	h.tick()
	updated := h.draft(d.SourceMessageID)
	position, err := parseInlineDirectoryPosition(updated.BrowseCursor)
	if err != nil || len(updated.Attachments) != 1 || updated.Attachments[0].Kind != "file" || updated.Body != "unsaved task" || updated.FollowMode != "topic" || updated.TargetChatID != "oc_work" || updated.WorkspaceMode != "local" || updated.BaseBranch != "" || updated.BrowsePath != "unsearched edit" || position.Query != "missing" {
		t.Fatal("attachment deletion lost the form or executed the unsubmitted query", err)
	}
	if err := action(d, 0); err != nil && !errors.Is(err, store.ErrSubmissionStale) {
		t.Fatal(err)
	}
	h.tick()
	if len(h.draft(d.SourceMessageID).Attachments) != 1 || h.g.groups != 0 || len(h.n.starts) != 0 {
		t.Fatal("old revision removed another attachment or created resources")
	}
	h.restart()
	updated = h.draft(d.SourceMessageID)
	if err := action(updated, 0); err != nil {
		t.Fatal(err)
	}
	h.tick()
	updated = h.draft(d.SourceMessageID)
	if len(updated.Attachments) != 0 {
		t.Fatal("remaining attachment could not be removed after restart")
	}
	if err := h.action(updated, "creation_submit", map[string]any{"creation_directory": h.root, "creation_mode": "group", "creation_use_worktree": false}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if h.draft(d.SourceMessageID).State != "created" || len(h.n.adds) != 1 || h.n.adds[0].body != "unsaved task" {
		t.Fatal("removing all disabled attachments did not permit normal creation")
	}
}

func TestInlineMalformedCheckboxAndAttachmentIndexCannotExecute(t *testing.T) {
	for _, kind := range []string{"checkbox", "fractional-index", "negative-index", "string-index"} {
		t.Run(kind, func(t *testing.T) {
			h := newManagerHarness(t)
			h.m.Config.Attachments.Enabled = true
			d := receiveCreationAttachments(t, h)
			action, form, value := "creation_remove_attachment", map[string]any{}, map[string]any{"index": float64(0)}
			switch kind {
			case "checkbox":
				action, form["creation_use_worktree"] = "creation_submit", "false"
			case "fractional-index":
				value["index"] = 0.5
			case "negative-index":
				value["index"] = -1
			case "string-index":
				value["index"] = "0"
			}
			if err := h.m.Action(context.Background(), d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, action, value, form); err != nil {
				t.Fatal(err)
			}
			h.tick()
			got := h.draft(d.SourceMessageID)
			if got.State != "draft" || got.WorkspaceMode != "worktree" || len(got.Attachments) != 2 || h.g.groups != 0 || len(h.n.starts) != 0 {
				t.Fatal("malformed control value mutated attachments or executed creation")
			}
		})
	}
}
