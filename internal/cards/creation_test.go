package cards

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func creationNodes(t *testing.T, card string) []map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(card), &root); err != nil {
		t.Fatal(err)
	}
	if root["schema"] != "2.0" || root["header"] != nil {
		t.Fatal("creation card has wrong schema or colored header")
	}
	var out []map[string]any
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			out = append(out, v)
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(root)
	return out
}

func creationAction(node map[string]any) map[string]any {
	if node["tag"] != "button" && node["tag"] != "select_static" {
		return nil
	}
	behaviors, ok := node["behaviors"].([]any)
	if !ok || len(behaviors) != 1 {
		return nil
	}
	behavior, ok := behaviors[0].(map[string]any)
	if !ok || behavior["type"] != "callback" {
		return nil
	}
	value, _ := behavior["value"].(map[string]any)
	return value
}

func creationView() CreationView {
	return CreationView{State: "draft", Host: "host-1", Body: "预填任务\n下一行", Directory: "/项目/中文 空格", Token: "opaque", Revision: 7, ThreadID: "must-not-be-trusted"}
}

func TestCreationDraftFormAndActionContract(t *testing.T) {
	v := creationView()
	v.QuickChoices = []CreationDirectoryChoice{{Path: "/recent", Label: "recent"}}
	card := Creation(v)
	actions := map[string]bool{}
	body, form, primary := false, false, 0
	for _, n := range creationNodes(t, card) {
		if n["tag"] == "form" && n["name"] == "creation_form" {
			form = true
		}
		if n["tag"] == "input" {
			if n["name"] != "creation_body" || n["default_value"] != v.Body {
				t.Fatal("main card mixed directory search into task input")
			}
			body = true
		}
		if n["tag"] == "select_static" {
			t.Fatal("main card exposed a second directory value")
		}
		if a := creationAction(n); a != nil {
			action := a["action"].(string)
			actions[action] = true
			if a["token"] != v.Token || a["revision"] != float64(v.Revision) || len(a) != 3 {
				t.Fatal("invalid draft callback identity")
			}
			if n["form_action_type"] != "submit" {
				t.Fatal("main actions must preserve task and follow edits")
			}
			if n["type"] == "text" || n["size"] != "medium" || n["width"] != "default" {
				t.Fatal("creation actions must be visibly pressable")
			}
			if n["type"] == "primary_filled" {
				primary++
				if action != "creation_submit" {
					t.Fatal("wrong primary action")
				}
			}
		}
	}
	if !body || !form || primary != 1 || !reflect.DeepEqual(actions, map[string]bool{"creation_submit": true, "creation_panel": true, "creation_cancel": true, "creation_local": true, "creation_worktree": true}) {
		t.Fatal("main task summary contract", actions)
	}
	if !strings.Contains(card, "新建专属群") || !strings.Contains(card, v.Directory) {
		t.Fatal("selected directory or follow summary missing")
	}
}

func TestCreationSearchResultsAreAdjacentAndDoNotCreate(t *testing.T) {
	v := creationView()
	v.SearchQuery = "codex"
	v.TopicsEnabled = true
	v.Workgroups = []CreationOption{{Label: "工作群", Value: "oc_known"}}
	v.SearchPage = &CreationDirectoryPage{Total: 2, Choices: []CreationDirectoryChoice{{Path: "/one/codex", Label: "codex"}, {Path: "/two/codex", Label: "codex", ReadOnly: true}}}
	card := Creation(v)
	paths := map[string]string{}
	query := false
	for _, n := range creationNodes(t, card) {
		if n["tag"] == "input" {
			if n["name"] != "creation_query" || n["default_value"] != v.SearchQuery {
				t.Fatal("search view did not isolate its query")
			}
			query = true
		}
		if n["name"] == "creation_mode" || n["name"] == "creation_target" || n["name"] == "creation_body" {
			t.Fatal("search view duplicated the main draft form")
		}
		if a := creationAction(n); a != nil {
			action := a["action"].(string)
			if action == "creation_submit" {
				t.Fatal("search results can create in an old directory")
			}
			if path, ok := a["path"].(string); ok {
				paths[path] = action
			}
			if a["token"] != v.Token || a["revision"] != float64(v.Revision) {
				t.Fatal("missing search scope")
			}
		}
	}
	if !query || paths["/one/codex"] != "creation_pick" || paths["/two/codex"] != "creation_browse" {
		t.Fatal("same-name search results lost their exact action", paths)
	}
	for _, path := range []string{"/one/codex", "/two/codex"} {
		if !strings.Contains(card, path) {
			t.Fatal("same-name result has no path for disambiguation")
		}
	}
}

func TestCreationEmptySearchResultsAndShortDirectoryAvoidNoise(t *testing.T) {
	v := creationView()
	v.SearchPage = &CreationDirectoryPage{}
	card := Creation(v)
	back := false
	for _, n := range creationNodes(t, card) {
		if n["tag"] == "select_static" {
			t.Fatal("empty results exposed a selector")
		}
		if a := creationAction(n); a != nil {
			if a["action"] == "creation_submit" || a["action"] == "creation_pick" || a["action"] == "creation_search_pick" {
				t.Fatal("empty results exposed a completion action")
			}
			if a["action"] == "creation_back" {
				back = true
			}
		}
	}
	if !back || !strings.Contains(card, "没有匹配目录") {
		t.Fatal("empty results have no recovery")
	}
}

func TestCreationTopicOptionsAndAuthorizedWorkgroups(t *testing.T) {
	v := creationView()
	v.TopicsEnabled = true
	v.FollowMode = "topic"
	v.TargetChatID = "oc_known"
	v.Workgroups = []CreationOption{{Label: "工作群", Value: "oc_known"}, {Label: "重复", Value: "oc_known"}, {Label: "无效", Value: ""}}
	seen := map[string]bool{}
	for _, node := range creationNodes(t, Creation(v)) {
		if node["tag"] != "select_static" {
			continue
		}
		name, _ := node["name"].(string)
		seen[name] = true
		switch name {
		case "creation_mode":
			if node["initial_option"] != "topic" || len(node["options"].([]any)) != 2 {
				t.Fatal("topic mode missing")
			}
		case "creation_target":
			if node["initial_option"] != "oc_known" || len(node["options"].([]any)) != 1 {
				t.Fatal("workgroup list not restricted/deduplicated")
			}
		}
	}
	if !seen["creation_mode"] || !seen["creation_target"] {
		t.Fatal("follow controls missing")
	}
	v.FollowMode = "group"
	groupPanel := false
	for _, node := range creationNodes(t, Creation(v)) {
		if node["tag"] != "collapsible_panel" {
			continue
		}
		raw, _ := json.Marshal(node)
		if strings.Contains(string(raw), `"name":"creation_mode"`) {
			groupPanel = true
			if node["expanded"] != false || node["header"].(map[string]any)["title"].(map[string]any)["content"] != "跟进位置 · 新建专属群" {
				t.Fatal("group follow panel did not show its collapsed default")
			}
		}
	}
	if !groupPanel {
		t.Fatal("group follow panel missing")
	}
	v.TargetChatID = "oc_forged"
	for _, node := range creationNodes(t, Creation(v)) {
		if node["name"] == "creation_target" && node["initial_option"] != nil {
			t.Fatal("unlisted target selected")
		}
	}
}

func TestCreationStateSpecificActionsAndNoAppClaim(t *testing.T) {
	for state, want := range map[string]string{"creating": "", "ready": "", "working": "", "unknown": "creation_check", "rejected": "creation_retry", "created": "", "canceled": "", "expired": "", "unexpected": ""} {
		t.Run(state, func(t *testing.T) {
			v := creationView()
			v.State = state
			card := Creation(v)
			actions := []string{}
			for _, node := range creationNodes(t, card) {
				if a := creationAction(node); a != nil {
					actions = append(actions, a["action"].(string))
				}
			}
			if want == "" && len(actions) != 0 || want != "" && (len(actions) != 1 || actions[0] != want) {
				t.Fatal("unsafe action for creation state", actions)
			}
			if strings.Contains(card, "App 中可见") || strings.Contains(card, "App 已") || strings.Contains(card, "已显示在 App") {
				t.Fatal("protocol state made App visibility claim")
			}
			if state == "unknown" && !strings.Contains(card, "请勿重复创建") {
				t.Fatal("unknown missing explanation")
			}
		})
	}
}

func TestCreationDirectoryActionValuesAndTopLevelForms(t *testing.T) {
	v := creationView()
	v.DirectoryPage = &CreationDirectoryPage{Path: "/allowed/目录 空格", Parent: "/allowed", Filter: "项目", Next: "/allowed/目录 空格/next", Total: 120, Offset: 100, Choices: []CreationDirectoryChoice{{Path: "/allowed/目录 空格/项目", Label: "项目"}}}
	card := Creation(v)
	var root map[string]any
	if err := json.Unmarshal([]byte(card), &root); err != nil {
		t.Fatal(err)
	}
	body := root["body"].(map[string]any)["elements"].([]any)
	formIndex, selectIndex := -1, -1
	primary := 0
	actions := map[string]bool{}
	for i, raw := range body {
		n := raw.(map[string]any)
		if n["tag"] == "form" {
			formIndex = i
		}
		if n["tag"] == "select_static" {
			selectIndex = i
			a := creationAction(n)
			if a["action"] != "creation_browse" || n["type"] != "default" {
				t.Fatal("child selection is not an immediate navigation callback")
			}
			options := n["options"].([]any)
			if options[0].(map[string]any)["value"] != v.DirectoryPage.Choices[0].Path {
				t.Fatal("child navigation lost canonical value")
			}
		}
	}
	for _, n := range creationNodes(t, card) {
		if n["tag"] == "input" && (n["name"] != "creation_query" || n["default_value"] != "项目") {
			t.Fatal("browser must use a single name/path query")
		}
		if a := creationAction(n); a != nil {
			if a["token"] != v.Token || a["revision"] != float64(v.Revision) {
				t.Fatal("callback lost draft scope")
			}
			name := a["action"].(string)
			actions[name] = true
			if name == "creation_lookup" && n["form_action_type"] != "submit" {
				t.Fatal("query is not submitted")
			}
			if n["type"] == "primary_filled" {
				primary++
				if name != "creation_pick" || a["path"] != v.DirectoryPage.Path {
					t.Fatal("primary did not select current directory")
				}
			}
		}
	}
	if formIndex < 0 || selectIndex <= formIndex || primary != 1 {
		t.Fatal("query/result order or primary hierarchy incorrect")
	}
	for _, name := range []string{"creation_lookup", "creation_browse", "creation_pick", "creation_back", "creation_projects"} {
		if !actions[name] {
			t.Fatal("missing browser action", name)
		}
	}
	for _, name := range []string{"creation_submit", "creation_path", "creation_goto", "creation_filter"} {
		if actions[name] {
			t.Fatal("redundant or unsafe action", name)
		}
	}
	if !strings.Contains(card, "101–101") || !strings.Contains(card, "共 120 项") {
		t.Fatal("batch range/count not explicit")
	}
}

func TestCreationDirectoryReadOnlyItemsOnlyBrowse(t *testing.T) {
	v := creationView()
	v.DirectoryPage = &CreationDirectoryPage{Path: "/readonly", Parent: "/parent", ReadOnly: true, Choices: []CreationDirectoryChoice{{Path: "/readonly/sub", ReadOnly: true}, {Path: "/readonly/writable"}}}
	card := Creation(v)
	selectFound, disabledPick := false, false
	for _, n := range creationNodes(t, card) {
		if n["tag"] == "select_static" {
			selectFound = true
			if len(n["options"].([]any)) != 2 || creationAction(n)["action"] != "creation_browse" {
				t.Fatal("read-only children must remain browsable")
			}
		}
		if a := creationAction(n); a != nil && a["action"] == "creation_pick" {
			disabledPick = n["disabled"] == true
		}
	}
	if !selectFound || !disabledPick || !strings.Contains(card, "可浏览但不可用于任务") {
		t.Fatal("read-only state is not explained and disabled")
	}
}

func TestCreationDirectoryEmptyFilterKeepsManualPath(t *testing.T) {
	v := creationView()
	v.DirectoryPage = &CreationDirectoryPage{Path: "/allowed", Filter: "missing", Choices: []CreationDirectoryChoice{{Path: ""}}}
	card := Creation(v)
	query := false
	for _, n := range creationNodes(t, card) {
		if n["tag"] == "select_static" {
			t.Fatal("empty filtered list exposed selector")
		}
		if n["tag"] == "input" && n["name"] == "creation_query" {
			query = true
		}
	}
	if !query || !strings.Contains(card, "没有匹配目录") || !strings.Contains(card, "完整路径") {
		t.Fatal("empty list lost path/query recovery")
	}
}

func TestCreationLinkSafetyAndThreadDetails(t *testing.T) {
	v := creationView()
	v.State = "created"
	for _, link := range []string{"javascript:alert(1)", "http://applink.feishu.cn/client/chat/open?openChatId=x", "https://applink.feishu.cn.evil.test/client/chat/open?openChatId=x", "https://user@applink.feishu.cn/client/chat/open?openChatId=x", "https://applink.feishu.cn/other?openChatId=x"} {
		v.ChatLink = link
		if strings.Contains(Creation(v), "打开任务位置") {
			t.Fatal("unsafe chat link rendered")
		}
	}
	v.ChatLink = "https://applink.feishu.cn/client/chat/open?openChatId=oc_known"
	card := Creation(v)
	if !strings.Contains(card, "打开任务位置") {
		t.Fatal("verified chat link missing")
	}
	threadInPanel := false
	for _, node := range creationNodes(t, card) {
		if node["tag"] == "collapsible_panel" {
			raw, _ := json.Marshal(node)
			if strings.Contains(string(raw), v.ThreadID) {
				threadInPanel = true
				if node["expanded"] != false {
					t.Fatal("thread details expanded")
				}
			}
		}
	}
	if !threadInPanel {
		t.Fatal("thread identity evidence missing")
	}
}

func TestCreationWithoutTokenAndRootEscaping(t *testing.T) {
	v := creationView()
	v.Token = ""
	for _, node := range creationNodes(t, Creation(v)) {
		if creationAction(node) != nil {
			t.Fatal("unusable token has controls")
		}
	}
	card := CreationRoot("任务 <at id=\"all\"></at>\n正文", "/allowed/<at>", "飞书来源 <at>")
	var root map[string]any
	if err := json.Unmarshal([]byte(card), &root); err != nil {
		t.Fatal(err)
	}
	for _, node := range creationNodes(t, card) {
		if text, ok := node["content"].(string); ok && strings.Contains(text, "<at") {
			t.Fatal("root content injected native mention")
		}
		if creationAction(node) != nil {
			t.Fatal("platform anchor carries creating controls")
		}
	}
	if strings.Contains(card, "已创建") || strings.Contains(card, "已接收") || strings.Contains(card, "App 已") {
		t.Fatal("platform anchor claims native creation")
	}
}

func TestCreationBodyLengthBoundaryAndLongBodyPanel(t *testing.T) {
	v := creationView()
	v.Body = strings.Repeat("字", 1000)
	var boundaryInput map[string]any
	for _, node := range creationNodes(t, Creation(v)) {
		if node["tag"] == "input" && node["name"] == "creation_body" {
			boundaryInput = node
		}
	}
	if boundaryInput == nil || boundaryInput["default_value"] != v.Body || boundaryInput["max_length"] != float64(1000) {
		t.Fatalf("1000-rune body was not kept as an editable bounded input: %#v", boundaryInput)
	}

	v.Body = strings.Repeat("x", 1001)
	card := Creation(v)
	actions := map[string]bool{}
	longBodyPanel := false
	bodyInput := false
	hint := false
	for _, node := range creationNodes(t, card) {
		if node["tag"] == "input" && node["name"] == "creation_body" {
			bodyInput = true
		}
		if node["tag"] == "collapsible_panel" {
			raw, _ := json.Marshal(node)
			if strings.Contains(string(raw), v.Body) {
				longBodyPanel = true
			}
		}
		if content, ok := node["content"].(string); ok && strings.Contains(content, "将使用原任务正文；如需修改请重新发起 /new") {
			hint = true
		}
		if action := creationAction(node); action != nil {
			actions[action["action"].(string)] = true
		}
	}
	if bodyInput || !longBodyPanel || !hint {
		t.Fatalf("long body was truncated or exposed as an input: input=%v panel=%v hint=%v", bodyInput, longBodyPanel, hint)
	}
	if !reflect.DeepEqual(actions, map[string]bool{"creation_submit": true, "creation_panel": true, "creation_cancel": true, "creation_local": true, "creation_worktree": true}) {
		t.Fatalf("long body removed creation controls: %v", actions)
	}
	if !strings.Contains(card, v.Body) {
		t.Fatal("long body disappeared from the rendered creation card")
	}
}

func TestCreationLongDirectoryPathKeepsExactCallbackAndVisiblePath(t *testing.T) {
	v := creationView()
	path := "/" + strings.Repeat("p", 1000)
	v.DirectoryPage = &CreationDirectoryPage{Path: path, Parent: "/"}
	card := Creation(v)
	exact, visible := false, false
	for _, n := range creationNodes(t, card) {
		if n["tag"] == "input" && n["default_value"] != "" {
			t.Fatal("long path was copied into a bounded input")
		}
		if n["tag"] == "markdown" {
			content, _ := n["content"].(string)
			visible = visible || strings.Contains(content, path)
		}
		if a := creationAction(n); a != nil && a["action"] == "creation_pick" && a["path"] == path {
			exact = true
		}
	}
	if !exact || !visible {
		t.Fatal("long path was shortened for display or execution")
	}
}

func TestCreationAndAttachmentInputsStayWithinPlatformLimit(t *testing.T) {
	cards := []string{
		Creation(creationView()),
		Creation(CreationView{State: "draft", Token: "opaque", Revision: 1, DirectoryPage: &CreationDirectoryPage{Path: "/allowed"}}),
		AttachmentDraft(AttachmentDraftView{State: "open", Token: "opaque", Revision: 1}),
	}
	for _, card := range cards {
		for _, node := range creationNodes(t, card) {
			if node["tag"] != "input" {
				continue
			}
			max, ok := node["max_length"].(float64)
			if !ok || max > 1000 {
				t.Fatalf("input exceeds or omits the Feishu limit: %#v", node)
			}
		}
	}
}

func TestCreationPickerManyChoicesAndByteBudget(t *testing.T) {
	for _, long := range []bool{false, true} {
		v := creationView()
		p := &CreationDirectoryPage{Path: "/allowed", Total: 100}
		for i := 0; i < 100; i++ {
			name := fmt.Sprintf("dir-%03d", i)
			if long {
				name += strings.Repeat("中文", 160)
			}
			p.Choices = append(p.Choices, CreationDirectoryChoice{Path: "/allowed/" + name})
		}
		v.DirectoryPage = p
		card := Creation(v)
		if len(card) > creationPickerBytes {
			t.Fatal("picker exceeded serialized byte budget", len(card))
		}
		options := 0
		next := ""
		for _, n := range creationNodes(t, card) {
			if n["tag"] == "select_static" {
				options = len(n["options"].([]any))
			}
			if a := creationAction(n); a != nil {
				if after, ok := a["after"].(string); ok {
					next = after
				}
			}
		}
		if !long && (options != 100 || next != "") {
			t.Fatal("ordinary directory list was unnecessarily paged", options, next)
		}
		if long && (options < 1 || options >= 100 || next != p.Choices[options-1].Path) {
			t.Fatal("budget truncation lost an exact continuation", options)
		}
		if len(p.Choices) != 100 || p.Next != "" {
			t.Fatal("renderer mutated the durable page")
		}
	}
}

func TestCreationBusyPreservesContextWithoutActions(t *testing.T) {
	v := creationView()
	v.Busy = "正在打开目录…"
	v.DirectoryPage = &CreationDirectoryPage{Path: "/allowed"}
	card := Creation(v)
	for _, n := range creationNodes(t, card) {
		if n["tag"] == "input" || n["tag"] == "select_static" || creationAction(n) != nil {
			t.Fatal("busy card allows conflicting operations")
		}
	}
	if !strings.Contains(card, v.Busy) || !strings.Contains(card, "/allowed") || !strings.Contains(card, "任务和原目录已保留") {
		t.Fatal("busy feedback lost action/context")
	}
}

func TestCreationPickerEscapedPathCannotExceedRequestBudget(t *testing.T) {
	v := creationView()
	v.DirectoryPage = &CreationDirectoryPage{Path: "/" + strings.Repeat("<", 4000), Parent: "/"}
	card := Creation(v)
	if len(card) > creationPickerBytes || !strings.Contains(card, "超出卡片容量") {
		t.Fatal("one escaped path exceeded the serialized card limit")
	}
	for _, node := range creationNodes(t, card) {
		if action := creationAction(node); action != nil && action["action"] != "creation_back" {
			t.Fatal("capacity fallback executes a truncated path")
		}
	}
}

func TestCreationPendingKeepsLayoutAndSubmittedFields(t *testing.T) {
	v := creationView()
	base := Creation(v)
	card, err := CreationPending(base, "正在打开目录…", map[string]any{"creation_body": "刚编辑的任务"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(card, v.Directory) || !strings.Contains(card, "刚编辑的任务") || !strings.Contains(card, "正在打开目录") {
		t.Fatal("pending snapshot lost layout, input or context")
	}
	controls := 0
	for _, node := range creationNodes(t, card) {
		if node["tag"] == "input" || node["tag"] == "button" || node["tag"] == "select_static" {
			controls++
			if node["disabled"] != true {
				t.Fatal("pending card has a live conflicting control")
			}
		}
	}
	if controls < 4 || strings.Contains(base, "刚编辑的任务") {
		t.Fatal("pending conversion removed controls or mutated the source")
	}
	if _, _, err := WithRequestMarker(card, "newBusyMarker"); err != nil {
		t.Fatal("pending snapshot cannot carry its own durable marker", err)
	}
}
