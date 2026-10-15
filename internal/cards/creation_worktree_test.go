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
	branch, primary, modes := false, 0, 0
	for _, node := range creationNodes(t, card) {
		if node["name"] == "creation_branch" {
			branch = true
			if node["tag"] != "select_static" || node["initial_option"] != v.BaseBranch || len(node["options"].([]any)) != 2 {
				t.Fatal("selected base branch is not visible")
			}
		}
		if action := creationAction(node); action != nil {
			if action["action"] == "creation_worktree" || action["action"] == "creation_local" {
				modes++
				if node["form_action_type"] != "submit" {
					t.Fatal("mode switch lost unsaved task and branch edits")
				}
			}
			if node["type"] == "primary_filled" {
				primary++
				if action["action"] != "creation_submit" || node["disabled"] == true {
					t.Fatal("valid worktree choice cannot be submitted")
				}
			}
		}
	}
	if !branch || primary != 1 || modes != 2 || !strings.Contains(card, "已提交内容") {
		t.Fatal("worktree creation summary is incomplete")
	}
	// A missing or stale repository must not fall back to creating in place.
	v.Branches = nil
	for _, node := range creationNodes(t, Creation(v)) {
		if action := creationAction(node); action != nil && action["action"] == "creation_submit" && node["disabled"] != true {
			t.Fatal("worktree creation remained enabled without a branch")
		}
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
	if _, err := CreationPending(raw, "正在更新工作方式…", map[string]any{"creation_branch": v.BaseBranch}); err != nil {
		t.Fatal("bounded worktree card cannot show pending feedback", err)
	}
}
