package cards

import (
	"fmt"
	"strings"
	"testing"
)

func TestCreationWorktreeBranchSelectionPreservesFormAndPrimaryAction(t *testing.T) {
	v := creationView()
	v.WorkspaceMode, v.BaseBranch = "worktree", "refs/heads/main"
	v.Branches = []CreationOption{{Label: "main", Value: v.BaseBranch}, {Label: "feature/任务", Value: "refs/heads/feature/任务"}}
	card := Creation(v)
	branch, checker, primary := false, false, 0
	for _, node := range creationNodes(t, card) {
		if node["name"] == "creation_branch" {
			branch = true
			if node["tag"] != "select_static" || node["initial_option"] != v.BaseBranch || len(node["options"].([]any)) != 2 {
				t.Fatal("selected base branch is not visible")
			}
			if node["behaviors"] != nil {
				t.Fatal("branch edits must be submitted with the form")
			}
		}
		if node["tag"] == "checker" {
			checker = true
			style, _ := node["checked_style"].(map[string]any)
			if node["name"] != "creation_use_worktree" || node["checked"] != true || node["behaviors"] != nil || node["overall_checkable"] != true || node["margin"] != "0px" || node["padding"] != "0px" || style["show_strikethrough"] != false || style["opacity"] != float64(1) {
				t.Fatal("worktree checker is not a local checked form control")
			}
		}
		if node["tag"] == "column_set" && containsNode(node, map[string]any{"tag": "checker", "name": "creation_use_worktree"}) && containsNode(node, map[string]any{"tag": "select_static", "name": "creation_branch"}) {
			columns, _ := node["columns"].([]any)
			if len(columns) != 2 || columns[0].(map[string]any)["weight"] != float64(1) || columns[1].(map[string]any)["weight"] != float64(3) {
				t.Fatal("worktree and branch controls lost their 1:3 row weights")
			}
		}
		if action := creationAction(node); action != nil {
			if node["type"] == "primary_filled" {
				primary++
				if action["action"] != "creation_submit" || node["disabled"] == true {
					t.Fatal("valid worktree choice cannot be submitted")
				}
			}
		}
	}
	if !branch || !checker || primary != 1 || strings.Contains(card, "工作方式") {
		t.Fatal("worktree creation summary is incomplete")
	}
	for _, node := range creationNodes(t, card) {
		if node["tag"] == "markdown" && strings.Contains(fmt.Sprint(node["content"]), "已提交内容") {
			t.Fatal("worktree guidance must remain in the hover tip")
		}
	}
	// A missing branch disables only the branch selector; the server validates
	// the submitted checker and branch values after the user changes them.
	v.Branches = nil
	primary = 0
	for _, node := range creationNodes(t, Creation(v)) {
		if node["name"] == "creation_branch" && node["disabled"] != true {
			t.Fatal("empty branch list did not disable its selector")
		}
		if action := creationAction(node); action != nil && action["action"] == "creation_submit" {
			primary++
			if node["disabled"] == true {
				t.Fatal("creation submit was blocked by stale branch state")
			}
		}
	}
	if primary != 1 {
		t.Fatal("creation submit disappeared without branch options")
	}
}

func TestCreationWorktreeBranchesFitCardBudget(t *testing.T) {
	v := creationView()
	v.WorkspaceMode, v.BaseBranch = "worktree", "refs/heads/main"
	v.Body = strings.Repeat("任务", 1900)
	v.Branches = []CreationOption{{Label: "main", Value: v.BaseBranch}}
	for i := 0; i < 99; i++ {
		ref := fmt.Sprintf("refs/heads/%03d/", i) + strings.Repeat("branch", 120)
		v.Branches = append(v.Branches, CreationOption{Label: ref, Value: ref})
	}
	raw := Creation(v)
	if len(raw) > 28*1024 || !strings.Contains(raw, v.Body) {
		t.Fatal("branch options exceeded the full JSON budget or truncated the task")
	}
	for _, node := range creationNodes(t, raw) {
		if node["name"] == "creation_branch" && (node["initial_option"] != v.BaseBranch || len(node["options"].([]any)) < 1) {
			t.Fatal("budgeting removed the selected branch")
		}
	}
	if _, err := CreationPending(raw, "正在更新工作方式…", map[string]any{"creation_branch": v.BaseBranch, "creation_use_worktree": true}); err != nil {
		t.Fatal("bounded worktree card cannot show pending feedback", err)
	}
}

func TestCreationUnavailableWorktreeKeepsLocalCreationAndDirectorySelection(t *testing.T) {
	v := creationView()
	v.WorktreeUnavailable = "此目录不是可用的 Git 仓库，请更改目录。"
	card := Creation(v)
	if !strings.Contains(card, v.WorktreeUnavailable) {
		t.Fatal("unavailable worktree has no visible explanation")
	}
	for _, node := range creationNodes(t, card) {
		if node["tag"] == "checker" && node["disabled"] == true {
			t.Fatal("unavailable worktree blocked local checker edits")
		}
		if node["tag"] == "select_static" && node["name"] == "creation_directory" && node["disabled"] == true {
			t.Fatal("worktree preflight blocked directory recovery")
		}
		if action := creationAction(node); action != nil && action["action"] == "creation_submit" && node["disabled"] == true {
			t.Fatal("worktree preflight blocked creation submit")
		}
	}
}
