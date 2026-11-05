package cards

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/displaypath"
)

const pathDisplayHome = "/synthetic/home/operator"

func pathDisplayNode(t *testing.T, nodes []map[string]any, tag, name string) map[string]any {
	t.Helper()
	for _, node := range nodes {
		if node["tag"] == tag && node["name"] == name {
			return node
		}
	}
	t.Fatalf("missing %s %s", tag, name)
	return nil
}

func pathDisplayOptions(t *testing.T, node map[string]any) map[string]string {
	t.Helper()
	options, ok := node["options"].([]any)
	if !ok {
		t.Fatalf("missing options: %#v", node)
	}
	got := make(map[string]string, len(options))
	for _, raw := range options {
		option := raw.(map[string]any)
		got[option["value"].(string)] = option["text"].(map[string]any)["content"].(string)
	}
	return got
}

func requirePathDisplayContent(t *testing.T, nodes []map[string]any, want string) {
	t.Helper()
	for _, node := range nodes {
		if content, ok := node["content"].(string); ok && strings.Contains(content, want) {
			return
		}
	}
	t.Errorf("missing visible content %q", want)
}

func requirePathDisplayExactContent(t *testing.T, nodes []map[string]any, want string) {
	t.Helper()
	for _, node := range nodes {
		if content, ok := node["content"].(string); ok && content == want {
			return
		}
	}
	t.Errorf("missing exact visible content %q", want)
}

func requirePathDisplayTitle(t *testing.T, nodes []map[string]any, want string) {
	t.Helper()
	for _, node := range nodes {
		if node["tag"] == "collapsible_panel" {
			header, _ := node["header"].(map[string]any)
			title, _ := header["title"].(map[string]any)
			if title["content"] == want {
				return
			}
		}
	}
	t.Errorf("missing panel title %q", want)
}

func requirePathDisplayAction(t *testing.T, nodes []map[string]any, name string, fields map[string]any) {
	t.Helper()
	for _, node := range nodes {
		action := creationAction(node)
		if action["action"] != name {
			continue
		}
		matches := true
		for key, want := range fields {
			if action[key] != want {
				matches = false
			}
		}
		if matches {
			return
		}
	}
	t.Errorf("missing action %q with fields %v", name, fields)
}

func TestCreationMainPathDisplayKeepsFormValues(t *testing.T) {
	v := creationView()
	v.Paths = displaypath.ForHome(pathDisplayHome)
	v.Directory = pathDisplayHome
	v.Body = "任务正文 " + pathDisplayHome + "/原样"
	v.SearchQuery = pathDisplayHome + "/查询 原样"
	v.QuickChoices = []CreationDirectoryChoice{
		{Path: pathDisplayHome},
		{Path: pathDisplayHome + "/项目 空间"},
		{Path: pathDisplayHome + "-other/项目"},
	}
	nodes := creationNodes(t, Creation(v))
	selectDirectory := pathDisplayNode(t, nodes, "select_static", "creation_directory")
	if selectDirectory["initial_option"] != v.Directory {
		t.Fatalf("initial directory = %v, want raw %q", selectDirectory["initial_option"], v.Directory)
	}
	want := map[string]string{
		pathDisplayHome:               "$HOME",
		pathDisplayHome + "/项目 空间":    "项目 空间 · $HOME",
		pathDisplayHome + "-other/项目": "项目 · " + pathDisplayHome + "-other",
	}
	if got := pathDisplayOptions(t, selectDirectory); !reflect.DeepEqual(got, want) {
		t.Errorf("directory options = %v, want %v", got, want)
	}
	for name, value := range map[string]string{"creation_body": v.Body, "creation_query": v.SearchQuery} {
		if got := pathDisplayNode(t, nodes, "input", name)["default_value"]; got != value {
			t.Errorf("%s = %v, want raw %q", name, got, value)
		}
	}
	v.InlineDirectories = &CreationDirectoryPage{Choices: v.QuickChoices, Next: pathDisplayHome + "/下一批"}
	requirePathDisplayAction(t, creationNodes(t, Creation(v)), "creation_find_next", map[string]any{"after": v.InlineDirectories.Next})
}

func TestCreationBrowsePathDisplayAndCallbacks(t *testing.T) {
	v := creationView()
	v.Paths = displaypath.ForHome(pathDisplayHome)
	parent := pathDisplayHome + "/0123456789AB"
	path := parent + "/项目 空间"
	next := path + "/继续"
	v.DirectoryPage = &CreationDirectoryPage{
		Path: path, Parent: parent, Next: next, Filter: pathDisplayHome + "/查询 原样",
		Choices: []CreationDirectoryChoice{{Path: path + "/子项目"}},
	}
	nodes := creationNodes(t, Creation(v))
	// The raw parent exceeds the compact heading limit, while the shortened one fits.
	requirePathDisplayTitle(t, nodes, "$HOME/0123456789AB")
	requirePathDisplayContent(t, nodes, "$HOME/0123456789AB/项目 空间")
	if got := pathDisplayNode(t, nodes, "input", "creation_query")["default_value"]; got != v.DirectoryPage.Filter {
		t.Errorf("browse query = %v, want raw %q", got, v.DirectoryPage.Filter)
	}
	if got := pathDisplayOptions(t, pathDisplayNode(t, nodes, "select_static", "creation_entry")); !reflect.DeepEqual(got, map[string]string{path + "/子项目": "子项目"}) {
		t.Errorf("browse options = %v", got)
	}
	requirePathDisplayAction(t, nodes, "creation_browse", map[string]any{"path": parent})
	requirePathDisplayAction(t, nodes, "creation_browse", map[string]any{"path": path, "after": next})
	requirePathDisplayAction(t, nodes, "creation_pick", map[string]any{"path": path})
}

func TestCreationRootChoicesDisplayPaths(t *testing.T) {
	v := creationView()
	v.Paths = displaypath.ForHome(pathDisplayHome)
	next := pathDisplayHome + "/继续"
	v.DirectoryPage = &CreationDirectoryPage{Next: next, Choices: []CreationDirectoryChoice{
		{Path: pathDisplayHome},
		{Path: pathDisplayHome + "/项目 空间"},
		{Path: pathDisplayHome + "-other/项目"},
	}}
	nodes := creationNodes(t, Creation(v))
	want := map[string]string{
		pathDisplayHome:               "$HOME",
		pathDisplayHome + "/项目 空间":    "$HOME/项目 空间",
		pathDisplayHome + "-other/项目": pathDisplayHome + "-other/项目",
	}
	if got := pathDisplayOptions(t, pathDisplayNode(t, nodes, "select_static", "creation_entry")); !reflect.DeepEqual(got, want) {
		t.Errorf("root options = %v, want %v", got, want)
	}
	requirePathDisplayAction(t, nodes, "creation_browse", map[string]any{"path": "", "after": next})
}

func TestCreationBrowseHomeSummary(t *testing.T) {
	v := creationView()
	v.Paths = displaypath.ForHome(pathDisplayHome)
	v.DirectoryPage = &CreationDirectoryPage{Path: pathDisplayHome}
	nodes := creationNodes(t, Creation(v))
	requirePathDisplayExactContent(t, nodes, "**$HOME**")
	requirePathDisplayAction(t, nodes, "creation_pick", map[string]any{"path": pathDisplayHome})
}

func TestCreationSearchPathDisplayAndCallbacks(t *testing.T) {
	for _, count := range []int{3, 7} {
		t.Run(fmt.Sprintf("%d results", count), func(t *testing.T) {
			v := creationView()
			v.Paths = displaypath.ForHome(pathDisplayHome)
			v.SearchQuery = pathDisplayHome + "/查询 原样"
			choices := []CreationDirectoryChoice{
				{Path: pathDisplayHome},
				{Path: pathDisplayHome + "/项目 空间", ReadOnly: count == 3},
				{Path: pathDisplayHome + "-other/项目"},
			}
			for len(choices) < count {
				choices = append(choices, CreationDirectoryChoice{Path: fmt.Sprintf("%s/项目%d", pathDisplayHome, len(choices))})
			}
			next := pathDisplayHome + "/下一批"
			v.SearchPage = &CreationDirectoryPage{Choices: choices, Next: next}
			nodes := creationNodes(t, Creation(v))
			if got := pathDisplayNode(t, nodes, "input", "creation_query")["default_value"]; got != v.SearchQuery {
				t.Errorf("search query = %v, want raw %q", got, v.SearchQuery)
			}
			requirePathDisplayContent(t, nodes, v.SearchQuery)
			requirePathDisplayAction(t, nodes, "creation_search_next", map[string]any{"after": next})
			if count == 3 {
				for _, display := range []string{"$HOME", "$HOME/项目 空间", pathDisplayHome + "-other/项目"} {
					requirePathDisplayExactContent(t, nodes, "<font color='grey'>"+display+"</font>")
				}
				requirePathDisplayAction(t, nodes, "creation_pick", map[string]any{"path": pathDisplayHome})
				requirePathDisplayAction(t, nodes, "creation_browse", map[string]any{"path": pathDisplayHome + "/项目 空间"})
				requirePathDisplayAction(t, nodes, "creation_pick", map[string]any{"path": pathDisplayHome + "-other/项目"})
				return
			}
			options := pathDisplayOptions(t, pathDisplayNode(t, nodes, "select_static", "creation_search_choice"))
			if len(options) != count || options[pathDisplayHome] != "$HOME" || options[pathDisplayHome+"/项目 空间"] != "项目 空间 · $HOME" || options[pathDisplayHome+"-other/项目"] != "项目 · "+pathDisplayHome+"-other" {
				t.Errorf("search options lost displayed labels or raw values: %v", options)
			}
		})
	}
}

func TestCreationDirectoryLabelsKeepProjectNamesFirst(t *testing.T) {
	paths := displaypath.ForHome(pathDisplayHome)
	for _, tt := range []struct{ path, want string }{
		{pathDisplayHome, "$HOME"},
		{pathDisplayHome + "/goProject/larkmail/smtp-in-postfix-service", "smtp-in-postfix-service · $HOME/goProject/larkmail"},
		{pathDisplayHome + "/ai-tools/codex-feishu", "codex-feishu · $HOME/ai-tools"},
		{pathDisplayHome + "/goProject/ai-tools/codex-feishu", "codex-feishu · $HOME/goProject/ai-tools"},
		{"/", "/"},
	} {
		if got := creationChoiceLabel(CreationDirectoryChoice{Path: tt.path}, false, paths); got != tt.want {
			t.Errorf("label for %q = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestCreationFullPathDetailsMatchCapacityTrimmedChoices(t *testing.T) {
	v := creationView()
	v.Paths = displaypath.ForHome(pathDisplayHome)
	v.InlineDirectories = &CreationDirectoryPage{Filter: "project"}
	for i := 0; i < 80; i++ {
		path := fmt.Sprintf("%s/project-%03d-%s", pathDisplayHome, i, strings.Repeat("long", 100))
		v.InlineDirectories.Choices = append(v.InlineDirectories.Choices, CreationDirectoryChoice{Path: path})
	}
	v.Directory = v.InlineDirectories.Choices[79].Path
	nodes := creationNodes(t, Creation(v))
	options := pathDisplayOptions(t, pathDisplayNode(t, nodes, "select_static", "creation_directory"))
	if len(options) >= 80 || len(options) == 0 {
		t.Fatal("fixture did not trim the directory page")
	}
	var panel map[string]any
	for _, node := range nodes {
		if node["element_id"] == "creation_directory_paths" {
			panel = node
		}
	}
	if panel == nil || panel["expanded"] != false || panel["behaviors"] != nil {
		t.Fatal("complete paths must expand locally without a submission")
	}
	details, _ := panel["elements"].([]any)
	if len(details) != len(options) {
		t.Fatal("path details differ from the actual offered directories")
	}
	for _, detail := range details {
		node := detail.(map[string]any)
		content, _ := node["content"].(string)
		if node["tag"] != "markdown" || node["max_lines"] != nil || node["behaviors"] != nil {
			t.Fatal("full paths are clipped or have a callback")
		}
		matched := false
		for path := range options {
			if strings.HasSuffix(content, "\n"+creationLiteral(v.Paths.Path(path))) {
				matched = true
				delete(options, path)
				break
			}
		}
		if !matched {
			t.Fatal("a complete displayed path is shortened, duplicated, or absent from the selector")
		}
	}
	if len(options) != 0 || len(v.InlineDirectories.Choices) != 80 {
		t.Fatal("path details lost an offered choice or mutated the input page")
	}
}

func TestCreationBusyAndCompletedPathDisplay(t *testing.T) {
	longSuffix := "/" + strings.Repeat("长", 185)
	for _, state := range []string{"busy", "created", "confirmed"} {
		t.Run(state, func(t *testing.T) {
			v := creationView()
			v.Paths = displaypath.ForHome(pathDisplayHome)
			v.Directory = pathDisplayHome + longSuffix
			v.Body = "原任务 " + pathDisplayHome + "/正文"
			if state == "busy" {
				v.Busy = "正在打开目录"
				v.DirectoryPage = &CreationDirectoryPage{Path: v.Directory}
			} else {
				v.State = state
			}
			nodes := creationNodes(t, Creation(v))
			// The raw path exceeds the busy limit, but the shortened path fits.
			requirePathDisplayContent(t, nodes, "$HOME"+longSuffix)
			if state != "busy" {
				requirePathDisplayContent(t, nodes, v.Body)
			}
			if v.Directory != pathDisplayHome+longSuffix {
				t.Fatal("renderer changed the original directory")
			}
		})
	}
}

func TestHumanAttachmentPathDisplayPreservesInput(t *testing.T) {
	path := pathDisplayHome + "/附件 空格/file.txt"
	body := "原正文 " + pathDisplayHome + "/正文"
	attachments := []AttachmentView{{Kind: "file", Name: "file.txt", Path: path, Paths: displaypath.ForHome(pathDisplayHome)}}
	elements := humanElements(t, Human("Codex 端", body, attachments))
	if got := elements[0].(map[string]any)["content"]; got != body {
		t.Errorf("body = %v, want raw %q", got, body)
	}
	panel := elements[len(elements)-2].(map[string]any)
	details := panel["elements"].([]any)
	if got, want := details[0].(map[string]any)["content"], "文件 · file.txt\n$HOME/附件 空格/file.txt"; got != want {
		t.Errorf("attachment details = %v, want %q", got, want)
	}
	if attachments[0].Path != path {
		t.Errorf("attachment path changed to %q, want %q", attachments[0].Path, path)
	}
}
