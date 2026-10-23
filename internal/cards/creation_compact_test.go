package cards

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCreationInlineDirectoryAndSearchControlsStayInTheMainForm(t *testing.T) {
	v := creationView()
	v.QuickChoices = []CreationDirectoryChoice{{Path: "/quick"}}
	v.InlineDirectories = &CreationDirectoryPage{
		Choices: []CreationDirectoryChoice{{Path: "/inline", Label: "inline"}},
		Next:    "cursor-2",
	}
	card := Creation(v)
	var directory, query, next map[string]any
	for _, node := range creationNodes(t, card) {
		if node["tag"] == "select_static" && node["name"] == "creation_directory" {
			directory = node
		}
		if node["tag"] == "input" && node["name"] == "creation_query" {
			query = node
		}
		if action := creationAction(node); action != nil && action["action"] == "creation_find_next" {
			next = node
			if action["after"] != "cursor-2" || node["size"] != "small" || node["form_action_type"] != "submit" {
				t.Fatal("next page action lost its cursor or form submission")
			}
		}
	}
	if directory == nil || query == nil || next == nil {
		t.Fatal("inline directory controls missing from the main form")
	}
	if directory["initial_option"] != nil || directory["behaviors"] != nil || directory["disabled"] == true {
		t.Fatal("inline directory choices unexpectedly accepted the stale current directory")
	}
	options := directory["options"].([]any)
	if len(options) != 1 || options[0].(map[string]any)["value"] != "/inline" {
		t.Fatal("inline choices were mixed with quick or current choices")
	}
	if query["default_value"] != "" || query["name"] != "creation_query" {
		t.Fatal("main directory query did not use its submitted input")
	}
	for _, node := range creationNodes(t, card) {
		if node["tag"] != "column_set" {
			continue
		}
		columns, _ := node["columns"].([]any)
		if len(columns) != 2 {
			continue
		}
		if containsNode(columns[0], query) {
			if columns[0].(map[string]any)["weight"] != float64(3) || columns[1].(map[string]any)["weight"] != float64(1) {
				t.Fatal("directory query row does not use the required 3:1 weights")
			}
			if find := nestedButton(columns[1], "creation_find"); find == nil || find["width"] != "fill" {
				t.Fatal("directory find action is not a full-width form submit")
			}
		}
	}

	v.InlineDirectories = nil
	v.QuickChoices = nil
	card = Creation(v)
	for _, node := range creationNodes(t, card) {
		if node["tag"] == "select_static" && node["name"] == "creation_directory" {
			options := node["options"].([]any)
			if len(options) != 1 || options[0].(map[string]any)["value"] != v.Directory || node["initial_option"] != v.Directory {
				t.Fatal("quick-choice fallback did not preserve the current directory")
			}
		}
	}
	v.InlineDirectories = &CreationDirectoryPage{}
	card = Creation(v)
	for _, node := range creationNodes(t, card) {
		if node["tag"] == "select_static" && node["name"] == "creation_directory" {
			placeholder, _ := node["placeholder"].(map[string]any)
			if node["disabled"] != true || placeholder["content"] != "请选择工作目录" || len(node["options"].([]any)) != 0 {
				t.Fatal("empty inline directory results did not keep a disabled selector")
			}
		}
	}
}

func TestCreationAttachmentRowsUseSafePreviewsAndSubmittedDeletes(t *testing.T) {
	v := creationView()
	v.Attachments = []CreationAttachmentView{
		{Index: 3, Kind: "image", Name: "fixture.png", ImageKey: "img_fixture_safe"},
		{Index: 9, Kind: "file", Name: "notes.md", Text: "<script>**unsafe**\nplain", Truncated: true},
	}
	card := Creation(v)
	var image bool
	var filePanel bool
	var truncated bool
	deletes := map[float64]bool{}
	for _, node := range creationNodes(t, card) {
		if node["tag"] == "img" {
			image = true
			if node["img_key"] != "img_fixture_safe" || node["preview"] != true || node["size"] != "40px 40px" {
				t.Fatal("image attachment preview contract changed")
			}
		}
		if node["tag"] == "collapsible_panel" {
			header, _ := node["header"].(map[string]any)
			title, _ := header["title"].(map[string]any)
			if title["content"] == "notes.md" {
				filePanel = true
			}
		}
		if node["tag"] == "button" && strings.HasPrefix(fmt.Sprint(node["name"]), "creation_remove_attachment_") {
			action := creationAction(node)
			if action == nil || action["action"] != "creation_remove_attachment" || node["form_action_type"] != "submit" || action["token"] != v.Token || action["revision"] != float64(v.Revision) {
				t.Fatal("attachment delete did not preserve the draft scope")
			}
			index, ok := action["index"].(float64)
			if !ok || deletes[index] {
				t.Fatal("attachment delete index is missing or duplicated")
			}
			deletes[index] = true
		}
		if content, ok := node["content"].(string); ok && strings.Contains(content, "预览已截断") {
			truncated = true
		}
	}
	if !image || !filePanel || !truncated || len(deletes) != 2 || !deletes[3] || !deletes[9] {
		t.Fatal("attachment previews or delete controls missing")
	}
	if strings.Contains(card, "<script>") || strings.Contains(card, "**unsafe**") {
		t.Fatal("file preview was not safely escaped")
	}
	if strings.Contains(card, "已附带") {
		t.Fatal("legacy attachment count leaked into the creation form")
	}
}

func TestCreationEmptyAttachmentsOmitAttachmentRegion(t *testing.T) {
	v := creationView()
	v.Images, v.Files = 4, 5
	card := Creation(v)
	if strings.Contains(card, "creation_attachment_") || strings.Contains(card, "已附带") {
		t.Fatal("empty attachment list rendered a phantom attachment region")
	}
}

func TestCreationAttachmentAndChoiceBudgetPreservesSelections(t *testing.T) {
	v := creationView()
	v.Directory = "/project/" + strings.Repeat("d", 1400)
	v.InlineDirectories = &CreationDirectoryPage{Next: "directory-after"}
	v.InlineDirectories.Choices = append(v.InlineDirectories.Choices, CreationDirectoryChoice{Path: v.Directory, Label: "selected"})
	for i := 0; i < 80; i++ {
		v.InlineDirectories.Choices = append(v.InlineDirectories.Choices, CreationDirectoryChoice{Path: fmt.Sprintf("/project/%03d-%s", i, strings.Repeat("choice", 45))})
	}
	v.BaseBranch = "refs/heads/selected"
	v.WorkspaceMode = "worktree"
	v.Branches = []CreationOption{{Label: "selected", Value: v.BaseBranch}}
	for i := 0; i < 80; i++ {
		ref := fmt.Sprintf("refs/heads/%03d-%s", i, strings.Repeat("branch", 45))
		v.Branches = append(v.Branches, CreationOption{Label: ref, Value: ref})
	}
	for i := 0; i < 20; i++ {
		v.Attachments = append(v.Attachments, CreationAttachmentView{Index: i, Kind: "file", Name: fmt.Sprintf("file-%02d.txt", i), Text: strings.Repeat("preview <unsafe> ", 400)})
	}
	card := Creation(v)
	if len(card) > creationPickerBytes {
		t.Fatalf("creation card exceeded byte budget: %d", len(card))
	}
	var directories, branches, deletes int
	for _, node := range creationNodes(t, card) {
		if node["tag"] == "select_static" && node["name"] == "creation_directory" {
			for _, raw := range node["options"].([]any) {
				if raw.(map[string]any)["value"] == v.Directory {
					directories++
				}
			}
		}
		if node["tag"] == "select_static" && node["name"] == "creation_branch" {
			for _, raw := range node["options"].([]any) {
				if raw.(map[string]any)["value"] == v.BaseBranch {
					branches++
				}
			}
		}
		if node["tag"] == "button" && strings.HasPrefix(fmt.Sprint(node["name"]), "creation_remove_attachment_") {
			deletes++
		}
	}
	if directories != 1 || branches != 1 || deletes != 20 {
		t.Fatalf("budget trimming lost selected values or attachment rows: len=%d directories=%d branches=%d deletes=%d", len(card), directories, branches, deletes)
	}
}

func TestCreationSearchBudgetDoesNotSkipOmittedDirectories(t *testing.T) {
	v := creationView()
	v.InlineDirectories = &CreationDirectoryPage{Filter: "match"}
	for i := 0; i < 80; i++ {
		path := fmt.Sprintf("/project/match-%03d-%s", i, strings.Repeat("segment", 100))
		v.InlineDirectories.Choices = append(v.InlineDirectories.Choices, CreationDirectoryChoice{Path: path})
	}
	v.Directory = v.InlineDirectories.Choices[79].Path
	card := Creation(v)
	kept, next := map[string]bool{}, ""
	for _, node := range creationNodes(t, card) {
		if node["tag"] == "select_static" && node["name"] == "creation_directory" {
			if node["initial_option"] != v.Directory {
				t.Fatal("capacity trimming lost the selected tail option")
			}
			for _, option := range node["options"].([]any) {
				kept[option.(map[string]any)["value"].(string)] = true
			}
		}
		if action := creationAction(node); action != nil && action["action"] == "creation_find_next" {
			next, _ = action["after"].(string)
		}
	}
	if len(card) > creationViewBytes || len(kept) >= 80 || !kept[v.Directory] || !kept[v.InlineDirectories.Choices[0].Path] || next == "" {
		t.Fatal("fixture did not produce a bounded, navigable partial page")
	}
	for _, choice := range v.InlineDirectories.Choices {
		if !kept[choice.Path] && next >= choice.Path {
			t.Fatal("next page cursor skipped an omitted directory")
		}
	}
	if len(v.InlineDirectories.Choices) != 80 || v.InlineDirectories.Next != "" {
		t.Fatal("rendering mutated the caller's search page")
	}
}

func TestCreationCapacityFallbackCannotSubmitHiddenWorkspace(t *testing.T) {
	v := creationView()
	v.Directory = "/" + strings.Repeat("oversized", 4000)
	v.InlineDirectories = &CreationDirectoryPage{Choices: []CreationDirectoryChoice{{Path: v.Directory}}}
	v.Attachments = []CreationAttachmentView{{Index: 0, Kind: "file", Name: "fixture.txt", Text: "fixture"}}
	card := Creation(v)
	if len(card) > creationViewBytes {
		t.Fatal("recovery form still exceeds card budget")
	}
	var find, remove, body bool
	for _, node := range creationNodes(t, card) {
		if action := creationAction(node); action != nil {
			switch action["action"] {
			case "creation_submit":
				t.Fatal("recovery form can create with an invisible directory/branch")
			case "creation_find":
				find = true
			case "creation_remove_attachment":
				remove = true
			}
		}
		if node["tag"] == "input" && node["name"] == "creation_body" && node["default_value"] == v.Body {
			body = true
		}
	}
	if !find || !remove || !body {
		t.Fatal("capacity fallback lost the actions needed to recover the full form")
	}
}

func TestCreationComponentBudgetPreservesAttachmentRecovery(t *testing.T) {
	for _, images := range []bool{false, true} {
		t.Run(fmt.Sprintf("images=%t", images), func(t *testing.T) {
			v := creationView()
			v.InlineDirectories = &CreationDirectoryPage{Filter: "match"}
			for i := 0; i < 100; i++ {
				v.InlineDirectories.Choices = append(v.InlineDirectories.Choices, CreationDirectoryChoice{Path: fmt.Sprintf("/d/%03d", i)})
				v.Branches = append(v.Branches, CreationOption{Label: fmt.Sprint(i), Value: fmt.Sprintf("refs/heads/b%03d", i)})
			}
			v.Directory, v.BaseBranch = "/d/099", "refs/heads/b099"
			count := 10
			if images {
				count = 20
			}
			for i := 0; i < count; i++ {
				part := CreationAttachmentView{Index: i, Kind: "file", Name: fmt.Sprintf("%d.txt", i), Text: "fixture"}
				if images {
					part.Kind, part.ImageKey, part.Text = "image", "img_fixture", ""
				}
				v.Attachments = append(v.Attachments, part)
			}
			original := creationWithAttachmentLimit(v, -1)
			var root any
			if err := json.Unmarshal([]byte(original), &root); err != nil || taskComponentCount(root) <= 200 {
				t.Fatal("fixture did not exceed the platform component limit", err)
			}
			card := Creation(v)
			if err := json.Unmarshal([]byte(card), &root); err != nil || taskComponentCount(root) > creationComponentLimit || len(card) > creationViewBytes {
				t.Fatal("creation card exceeded a platform budget", err)
			}
			deletes := 0
			for _, node := range creationNodes(t, card) {
				if action := creationAction(node); action != nil && action["action"] == "creation_remove_attachment" {
					deletes++
				}
			}
			if deletes != count {
				t.Fatal("component budgeting made attachments impossible to remove")
			}
			pending, err := CreationPending(card, "正在保存…", nil)
			if err != nil || json.Unmarshal([]byte(pending), &root) != nil || taskComponentCount(root) > 200 {
				t.Fatal("creation card left no room for pending feedback", err)
			}
		})
	}
}

func containsNode(value any, target map[string]any) bool {
	if node, ok := value.(map[string]any); ok {
		if node["tag"] == target["tag"] && node["name"] == target["name"] {
			return true
		}
		for _, child := range node {
			if containsNode(child, target) {
				return true
			}
		}
	}
	if nodes, ok := value.([]any); ok {
		for _, child := range nodes {
			if containsNode(child, target) {
				return true
			}
		}
	}
	return false
}

func nestedButton(value any, wanted string) map[string]any {
	if node, ok := value.(map[string]any); ok {
		if node["tag"] == "button" && node["name"] == wanted {
			return node
		}
		for _, child := range node {
			if button := nestedButton(child, wanted); button != nil {
				return button
			}
		}
	}
	if nodes, ok := value.([]any); ok {
		for _, child := range nodes {
			if button := nestedButton(child, wanted); button != nil {
				return button
			}
		}
	}
	return nil
}

func TestCreationPendingRepopulatesWorktreeCheckbox(t *testing.T) {
	v := creationView()
	v.WorkspaceMode = "worktree"
	v.Branches = []CreationOption{{Label: "main", Value: "main"}}
	base := Creation(v)
	pending, err := CreationPending(base, "处理中", map[string]any{"creation_use_worktree": false, "creation_branch": "main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range creationNodes(t, pending) {
		if node["tag"] == "checker" && (node["checked"] != false || node["disabled"] != true) {
			t.Fatal("pending checker did not preserve the submitted false value")
		}
	}
}

func TestCreationPendingCardRemainsWithinBudget(t *testing.T) {
	v := creationView()
	v.Attachments = []CreationAttachmentView{{Index: 1, Name: "x.txt", Text: strings.Repeat("x", 2000)}}
	card, err := CreationPending(Creation(v), "处理中", nil)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(card), &root); err != nil || len(card) > creationPickerBytes {
		t.Fatalf("pending card is invalid or over budget: %v len=%d", err, len(card))
	}
}
