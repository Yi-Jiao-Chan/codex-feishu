package cards

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const creationInputMaxRunes = 1000

// Leave a small margin for the durable pending status prepended by
// CreationPending while keeping the published card below the 28 KiB limit.
const creationViewBytes = 27 * 1024

// Reserve two components for pending feedback and request-marking overhead.
const creationComponentLimit = 198

// CreationPending keeps the last published layout stable while a claimed
// action runs. The caller journals this snapshot through the same card writer.
// It does not query the filesystem, infer completion, or authorize form data.
func CreationPending(base, status string, form map[string]any) (string, error) {
	var root map[string]any
	if json.Unmarshal([]byte(base), &root) != nil || root["schema"] != "2.0" {
		return "", fmt.Errorf("invalid creation card snapshot")
	}
	body, ok := root["body"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("invalid creation card body")
	}
	els, ok := body["elements"].([]any)
	if !ok {
		return "", fmt.Errorf("invalid creation card elements")
	}
	var disable func(any)
	disable = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			tag, _ := node["tag"].(string)
			if tag == "button" || tag == "input" || tag == "select_static" || tag == "checker" {
				node["disabled"] = true
			}
			name, _ := node["name"].(string)
			if checked, ok := form[name].(bool); ok && tag == "checker" {
				node["checked"] = checked
			}
			if text, ok := form[name].(string); ok && utf8.RuneCountInString(text) <= creationInputMaxRunes {
				if tag == "input" {
					node["default_value"] = text
				} else if tag == "select_static" {
					options, _ := node["options"].([]any)
					for _, option := range options {
						if choice, ok := option.(map[string]any); ok && choice["value"] == text {
							node["initial_option"] = text
							break
						}
					}
				}
			}
			for _, child := range node {
				disable(child)
			}
		case []any:
			for _, child := range node {
				disable(child)
			}
		}
	}
	disable(els)
	// A new unmarked status gives WithRequestMarker a unique slot without
	// changing IDs or removing the previous snapshot's evidence.
	body["elements"] = append([]any{creationText("**"+creationLiteral(status)+"**", "normal")}, els...)
	raw, err := json.Marshal(root)
	if err != nil {
		return "", err
	}
	if len(raw) > creationPickerBytes {
		return "", fmt.Errorf("creation pending snapshot exceeds card budget")
	}
	return string(raw), nil
}

type CreationOption struct{ Label, Value string }
type CreationDirectoryChoice struct {
	Path, ProjectID, Label string
	ReadOnly               bool
}
type CreationDirectoryPage struct {
	Path, Parent, Next string
	Choices            []CreationDirectoryChoice
	ReadOnly           bool
	Filter             string
	Breadcrumbs        []CreationOption
	Total, Offset      int
}
type CreationAttachmentView struct {
	Index                             int
	Kind, Name, ImageKey, Text, State string
	Truncated                         bool
}
type CreationView struct {
	State, Error, Host, Body, Directory, ProjectLabel, FollowMode, TargetChatID string
	Token, ChatLink, ThreadID                                                   string
	WorkspaceMode, BaseBranch, WorktreeUnavailable                              string
	Revision                                                                    int64
	Images, Files                                                               int
	TopicsEnabled                                                               bool
	Workgroups                                                                  []CreationOption
	Branches                                                                    []CreationOption
	QuickChoices                                                                []CreationDirectoryChoice
	InlineDirectories                                                           *CreationDirectoryPage
	Attachments                                                                 []CreationAttachmentView
	SearchQuery                                                                 string
	SearchPage                                                                  *CreationDirectoryPage
	DirectoryPage                                                               *CreationDirectoryPage
	Busy                                                                        string
}

func creationWorkspace(v CreationView) []element {
	checker := element{
		"tag": "checker", "name": "creation_use_worktree",
		"checked": v.WorkspaceMode == "worktree", "overall_checkable": true,
		"text":          element{"tag": "plain_text", "content": "工作树"},
		"checked_style": element{"show_strikethrough": false, "opacity": 1},
		"margin":        "0px", "padding": "0px",
		"hover_tips": element{"tag": "plain_text", "content": "工作树使用所选分支的已提交内容"},
	}
	options := make([]element, 0, len(v.Branches))
	for _, branch := range v.Branches {
		label := branch.Label
		if label == "" {
			label = branch.Value
		}
		options = append(options, element{
			"text":  element{"tag": "plain_text", "content": "基准分支 · " + interactionLine(label, 80)},
			"value": branch.Value,
		})
	}
	selectBranch := element{
		"tag": "select_static", "name": "creation_branch", "width": "fill", "type": "default",
		"options": options, "placeholder": element{"tag": "plain_text", "content": "请选择基准分支"},
	}
	if v.BaseBranch != "" {
		for _, branch := range v.Branches {
			if branch.Value == v.BaseBranch {
				selectBranch["initial_option"] = v.BaseBranch
				break
			}
		}
	}
	if len(options) == 0 {
		selectBranch["disabled"] = true
	}
	row := element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "margin": "0px", "columns": []element{
		{"tag": "column", "width": "weighted", "weight": 1, "vertical_align": "center", "padding": "0px", "elements": []element{checker}},
		{"tag": "column", "width": "weighted", "weight": 3, "vertical_align": "center", "padding": "0px", "elements": []element{selectBranch}},
	}}
	els := []element{row}
	if v.WorktreeUnavailable != "" {
		els = append(els, creationText(creationLiteral(v.WorktreeUnavailable), "normal"))
	}
	return els
}

func creationButton(v CreationView, label, action string, values element, submit bool) element {
	value := element{"action": action, "token": v.Token, "revision": v.Revision}
	for key, val := range values {
		value[key] = val
	}
	b := Button(label, action, v.Token)
	b["type"], b["size"], b["width"] = "default", "medium", "default"
	b["behaviors"] = []element{{"type": "callback", "value": value}}
	if submit {
		b["name"], b["form_action_type"] = action, "submit"
	}
	return b
}

func creationRow(buttons ...element) element {
	columns := make([]element, 0, len(buttons))
	for _, button := range buttons {
		columns = append(columns, element{"tag": "column", "width": "auto", "elements": []element{button}})
	}
	return element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "margin": "0px", "columns": columns}
}

func creationPrimary(button element) element {
	button["type"] = "primary_filled"
	return button
}

func creationText(text, size string) element {
	if size == "notation" {
		text = "<font color='grey'>" + text + "</font>"
	}
	return element{"tag": "markdown", "content": text, "text_size": size, "margin": "0px"}
}

func creationForm(name string, children []element) element {
	return element{"tag": "form", "name": name, "padding": "0px", "margin": "0px", "vertical_spacing": "8px", "elements": children}
}

// This is only a visual summary. The full path remains in details and every
// callback retains the original canonical value.
func creationCompactPath(path string) string {
	if utf8.RuneCountInString(path) <= 32 {
		return path
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 1; i < len(parts); i++ {
		short := "…/" + strings.Join(parts[i:], "/")
		if utf8.RuneCountInString(short) <= 32 {
			return short
		}
	}
	return "…/" + interactionLine(filepath.Base(path), 28)
}

func creationDirectorySummary(v CreationView, path, label string, action element) element {
	name, parent := "尚未选择目录", ""
	if path != "" {
		name, parent = interactionLine(filepath.Base(path), 28), creationCompactPath(filepath.Dir(path))
	}
	text := []element{creationText(label, "notation"), creationText("**"+creationLiteral(name)+"**", "normal")}
	if parent != "" {
		details := []element{creationText(creationLiteral(path), "notation")}
		text = append(text, taskPanel(parent, details))
	}
	columns := []element{{"tag": "column", "width": "weighted", "weight": 1, "vertical_spacing": "2px", "elements": text}}
	if action != nil {
		columns = append(columns, element{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []element{action}})
	}
	return element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "margin": "4px 0px", "columns": columns}
}

func creationQueryForm(v CreationView, action, placeholder, query string) element {
	if utf8.RuneCountInString(query) > creationInputMaxRunes {
		query = ""
	}
	input := element{"tag": "input", "name": "creation_query", "input_type": "text", "width": "fill", "default_value": query, "placeholder": element{"tag": "plain_text", "content": placeholder}, "max_length": creationInputMaxRunes}
	// The short two-character action leaves room for the weighted input even
	// on a narrow card. Action rows use content widths rather than full width.
	row := element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "columns": []element{
		{"tag": "column", "width": "weighted", "weight": 1, "elements": []element{input}},
		{"tag": "column", "width": "auto", "elements": []element{creationButton(v, "查找", action, nil, true)}},
	}}
	return creationForm("creation_query_form", []element{row})
}

func creationSelect(v CreationView, name, placeholder, action string, options []element) element {
	value := element{"action": action, "token": v.Token, "revision": v.Revision}
	return element{"tag": "select_static", "name": name, "type": "default", "width": "fill", "placeholder": element{"tag": "plain_text", "content": placeholder}, "options": options, "behaviors": []element{{"type": "callback", "value": value}}}
}

func creationLiteral(value string) string {
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "!", "\\!", "#", "\\#").Replace(Safe(value))
}

func creationChoiceLabel(choice CreationDirectoryChoice, readOnly bool) string {
	name := strings.TrimSpace(choice.Label)
	if name == "" || name == choice.Path {
		name = choice.Path
	} else {
		name += " · " + choice.Path
	}
	if readOnly {
		name = "只读 · " + name
	}
	return name
}

func creationSelectOptions(choices []CreationDirectoryChoice, writableOnly bool) []element {
	options := []element{}
	seen := map[string]bool{}
	for _, choice := range choices {
		if choice.Path == "" || seen[choice.Path] || writableOnly && choice.ReadOnly {
			continue
		}
		seen[choice.Path] = true
		options = append(options, element{
			"text":  element{"tag": "plain_text", "content": creationChoiceLabel(choice, choice.ReadOnly)},
			"value": choice.Path,
		})
	}
	return options
}

func creationMainDirectoryChoices(v CreationView) []CreationDirectoryChoice {
	if v.InlineDirectories != nil {
		return append([]CreationDirectoryChoice(nil), v.InlineDirectories.Choices...)
	}
	choices := append([]CreationDirectoryChoice(nil), v.QuickChoices...)
	if v.Directory != "" {
		seen := false
		for _, choice := range choices {
			if choice.Path == v.Directory {
				seen = true
				break
			}
		}
		if !seen {
			choices = append(choices, CreationDirectoryChoice{Path: v.Directory, Label: v.ProjectLabel})
		}
	}
	return choices
}

func creationMainDirectoryElements(v CreationView) []element {
	choices := creationMainDirectoryChoices(v)
	for index := range choices {
		choices[index].Label = "" // A single complete path identifies each choice.
	}
	selectDirectory := element{
		"tag": "select_static", "name": "creation_directory", "type": "default", "width": "fill",
		"placeholder": element{"tag": "plain_text", "content": "请选择工作目录"},
		"options":     creationSelectOptions(choices, false),
	}
	if len(selectDirectory["options"].([]element)) == 0 {
		selectDirectory["disabled"] = true
		if v.InlineDirectories != nil && v.InlineDirectories.Filter != "" {
			selectDirectory["placeholder"] = element{"tag": "plain_text", "content": "未找到匹配目录"}
		}
	} else if v.Directory != "" {
		for _, choice := range choices {
			if choice.Path == v.Directory {
				selectDirectory["initial_option"] = v.Directory
				break
			}
		}
	}
	els := []element{creationText("工作目录", "normal"), selectDirectory}
	query := v.SearchQuery
	if utf8.RuneCountInString(query) > creationInputMaxRunes {
		query = ""
	}
	queryInput := element{
		"tag": "input", "name": "creation_query", "input_type": "text", "width": "fill",
		"default_value": query, "max_length": creationInputMaxRunes,
		"placeholder": element{"tag": "plain_text", "content": "目录名或完整路径"},
	}
	find := creationButton(v, "查找", "creation_find", nil, true)
	find["width"] = "fill"
	queryRow := element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "margin": "0px", "columns": []element{
		{"tag": "column", "width": "weighted", "weight": 3, "elements": []element{queryInput}},
		{"tag": "column", "width": "weighted", "weight": 1, "elements": []element{find}},
	}}
	els = append(els, queryRow)
	if v.InlineDirectories != nil && v.InlineDirectories.Next != "" {
		next := creationButton(v, "下一批", "creation_find_next", element{"after": v.InlineDirectories.Next}, true)
		next["size"] = "small"
		els = append(els, creationRow(next))
	}
	return els
}

func creationAttachmentName(item CreationAttachmentView) string {
	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = fmt.Sprintf("附件 %d", item.Index+1)
		if item.Kind == "image" {
			name = fmt.Sprintf("图片 %d", item.Index+1)
		}
	}
	return interactionLine(name, 160)
}

func creationAttachmentDelete(v CreationView, item CreationAttachmentView) element {
	button := creationButton(v, "删除", "creation_remove_attachment", element{"index": item.Index}, true)
	button["name"] = fmt.Sprintf("creation_remove_attachment_%d", item.Index)
	button["type"] = "text"
	button["size"] = "tiny"
	return button
}

func creationAttachmentPreview(text string, limit int) (string, bool) {
	if text == "" {
		return "", false
	}
	if limit < 0 {
		return text, false
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text, false
	}
	if limit <= 1 {
		return string(runes[:limit]), true
	}
	return string(runes[:limit-1]) + "…", true
}

func creationAttachmentElements(v CreationView, previewLimit int) []element {
	if len(v.Attachments) == 0 {
		return nil
	}
	els := make([]element, 0, len(v.Attachments))
	for _, item := range v.Attachments {
		name := creationAttachmentName(item)
		deleteButton := creationAttachmentDelete(v, item)
		var content element
		if key := strings.TrimSpace(item.ImageKey); key != "" && !humanDataURL(key) {
			content = element{"tag": "img", "img_key": key, "preview": true, "size": "40px 40px", "scale_type": "crop_center", "corner_radius": "4px", "alt": element{"tag": "plain_text", "content": Safe(name)}, "margin": "0px"}
		} else if previewLimit != 0 && (item.Text != "" || item.Kind == "file" && item.State == "ready") {
			preview, shortened := creationAttachmentPreview(item.Text, previewLimit)
			children := []element{}
			if preview != "" {
				children = append(children, creationText(creationLiteral(preview), "normal"))
			} else if item.Text == "" && !item.Truncated {
				children = append(children, creationText("（空文件）", "normal"))
			}
			if shortened || item.Truncated {
				children = append(children, creationText("预览已截断", "notation"))
			}
			content = taskPanel(name, children)
		} else {
			content = creationText(creationLiteral(name), "normal")
		}
		var columns []element
		if content["tag"] == "img" {
			columns = []element{
				{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []element{content}},
				{"tag": "column", "width": "weighted", "weight": 1, "vertical_align": "center", "elements": []element{creationText(creationLiteral(name), "normal")}},
				{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []element{deleteButton}},
			}
		} else {
			columns = []element{
				{"tag": "column", "width": "weighted", "weight": 1, "elements": []element{content}},
				{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []element{deleteButton}},
			}
		}
		els = append(els, element{"tag": "column_set", "columns": columns})
	}
	return els
}

func creationFollowPanel(v CreationView) element {
	if !v.TopicsEnabled || len(v.Workgroups) == 0 {
		return creationText("跟进位置：新建专属群", "normal")
	}
	mode := v.FollowMode
	if mode != "topic" {
		mode = "group"
	}
	modeSelect := element{"tag": "select_static", "name": "creation_mode", "width": "fill", "initial_option": mode, "options": []element{
		{"text": element{"tag": "plain_text", "content": "新建专属群"}, "value": "group"},
		{"text": element{"tag": "plain_text", "content": "工作群中的话题"}, "value": "topic"},
	}}
	options := []element{}
	selected := ""
	seen := map[string]bool{}
	for _, group := range v.Workgroups {
		if group.Value == "" || seen[group.Value] {
			continue
		}
		seen[group.Value] = true
		label := group.Label
		if label == "" {
			label = group.Value
		}
		options = append(options, element{"text": element{"tag": "plain_text", "content": label}, "value": group.Value})
		if group.Value == v.TargetChatID {
			selected = group.Value
		}
	}
	selectGroup := element{"tag": "select_static", "name": "creation_target", "width": "fill", "placeholder": element{"tag": "plain_text", "content": "话题模式请选择授权工作群"}, "options": options}
	if selected != "" {
		selectGroup["initial_option"] = selected
	}
	title := "跟进位置 · 新建专属群"
	if mode == "topic" {
		title = "跟进位置 · 工作群话题"
	}
	panel := taskPanel(title, []element{modeSelect, selectGroup})
	panel["expanded"] = mode == "topic"
	return panel
}

// Creation renders a pure view. Every callback is revalidated against the
// durable draft; displaying a path or a selected option grants no authority.
func Creation(v CreationView) string {
	v = cloneCreationView(v)
	for _, previewLimit := range []int{-1, 4096, 2048, 1024, 512, 256, 128, 64, 0} {
		if raw := creationWithAttachmentLimit(v, previewLimit); creationFits(v, raw) {
			return raw
		}
	}
	// Preview text is the first thing trimmed. Keep the attachment rows and
	// their delete actions while reducing the option lists only afterwards.
	for trimCreationDirectories(&v) || trimCreationBranches(&v) {
		if raw := creationWithAttachmentLimit(v, 0); creationFits(v, raw) {
			return raw
		}
	}
	if raw := creationWithAttachmentLimit(v, 0); creationFits(v, raw) {
		return raw
	}
	return creationCapacityFallback(v)
}

func creationFits(v CreationView, raw string) bool {
	state := v.State
	if state == "" {
		state = "draft"
	}
	limit := creationViewBytes
	if state == "draft" && (v.DirectoryPage != nil || v.SearchPage != nil) {
		limit = creationPickerBytes
	}
	if len(raw) > limit {
		return false
	}
	var root any
	return json.Unmarshal([]byte(raw), &root) == nil && taskComponentCount(root) <= creationComponentLimit
}

func cloneCreationView(v CreationView) CreationView {
	v.Branches = append([]CreationOption(nil), v.Branches...)
	v.QuickChoices = append([]CreationDirectoryChoice(nil), v.QuickChoices...)
	v.Attachments = append([]CreationAttachmentView(nil), v.Attachments...)
	if v.InlineDirectories != nil {
		page := *v.InlineDirectories
		page.Choices = append([]CreationDirectoryChoice(nil), page.Choices...)
		page.Breadcrumbs = append([]CreationOption(nil), page.Breadcrumbs...)
		v.InlineDirectories = &page
	}
	return v
}

func trimCreationDirectoryChoices(choices *[]CreationDirectoryChoice, selected string) bool {
	if choices == nil || len(*choices) <= 1 {
		return false
	}
	for i := len(*choices) - 1; i >= 0; i-- {
		if selected == "" || (*choices)[i].Path != selected {
			*choices = append((*choices)[:i], (*choices)[i+1:]...)
			return true
		}
	}
	return false
}

func trimCreationDirectories(v *CreationView) bool {
	if v.InlineDirectories != nil {
		page := v.InlineDirectories
		if page.Filter == "" {
			return trimCreationDirectoryChoices(&page.Choices, v.Directory)
		}
		// A search page must retain a contiguous prefix. A selected option at
		// its tail may remain visible, but the next cursor stays before every
		// omitted result so capacity trimming cannot skip unseen directories.
		for i := len(page.Choices) - 1; i > 0; i-- {
			if page.Choices[i].Path == v.Directory {
				continue
			}
			page.Next = page.Choices[i-1].Path
			page.Choices = append(page.Choices[:i], page.Choices[i+1:]...)
			return true
		}
		return false
	}
	return trimCreationDirectoryChoices(&v.QuickChoices, v.Directory)
}

func trimCreationBranches(v *CreationView) bool {
	if len(v.Branches) <= 1 {
		return false
	}
	for i := len(v.Branches) - 1; i >= 0; i-- {
		if v.BaseBranch == "" || v.Branches[i].Value != v.BaseBranch {
			v.Branches = append(v.Branches[:i], v.Branches[i+1:]...)
			return true
		}
	}
	// Duplicate selected values are harmless; retain one selected option.
	v.Branches = v.Branches[:len(v.Branches)-1]
	return true
}

func creationCapacityFallback(v CreationView) string {
	els := []element{creationText("卡片内容过长，原任务已保留。请删除部分附件或查找更具体的目录。", "normal")}
	if (v.State == "draft" || v.State == "") && v.Token != "" && v.Revision > 0 {
		form := []element{}
		if utf8.RuneCountInString(v.Body) <= creationInputMaxRunes {
			form = append(form, element{"tag": "input", "name": "creation_body", "input_type": "multiline_text", "width": "fill", "default_value": v.Body, "max_length": creationInputMaxRunes})
		}
		minimal := v
		minimal.Attachments = append([]CreationAttachmentView(nil), v.Attachments...)
		for i := range minimal.Attachments {
			item := &minimal.Attachments[i]
			item.Name = interactionLine(creationAttachmentName(*item), 48)
			item.ImageKey, item.Text, item.State = "", "", ""
		}
		form = append(form, creationAttachmentElements(minimal, 0)...)
		form = append(form, element{"tag": "input", "name": "creation_query", "width": "fill", "placeholder": element{"tag": "plain_text", "content": "目录名或完整路径"}, "max_length": creationInputMaxRunes})
		// Hidden directory/branch values must never be executable from this
		// recovery view. Return to the full form through search before creating.
		form = append(form, creationRow(creationButton(v, "查找", "creation_find", nil, true), creationButton(v, "取消", "creation_cancel", nil, true)))
		els = append(els, creationForm("creation_form", form))
	}
	raw := renderWidth(els, "default")
	if creationFits(v, raw) {
		return raw
	}
	return renderWidth([]element{creationText("卡片内容过长，请缩短后重试。", "normal")}, "default")
}

func creationWithAttachmentLimit(v CreationView, previewLimit int) string {
	if v.Busy != "" {
		els := []element{creationText("**"+creationLiteral(v.Busy)+"**", "normal")}
		path := v.Directory
		if v.DirectoryPage != nil {
			path = v.DirectoryPage.Path
		}
		if path != "" {
			els = append(els, creationText(creationLiteral(interactionLine(path, 200)), "notation"))
		}
		els = append(els, creationText("任务和原目录已保留，处理完成后此卡会自动更新。", "notation"))
		return render(els)
	}
	state := v.State
	if state == "" {
		state = "draft"
	}
	if state == "draft" && (v.DirectoryPage != nil || v.SearchPage != nil) {
		return creationPicker(v)
	}
	status := map[string]string{
		"draft": "新建 Codex 任务", "creating": "正在创建 · 请勿重复提交",
		"ready": "创建请求已保存 · 等待处理", "working": "正在创建 · 请勿重复提交",
		"collecting": "正在收取附件 · 收齐后提交首条任务",
		"rejected":   "创建未完成 · 已确认该步骤未被接受",
		"unknown":    "正在核验创建结果 · 请勿重复创建",
		"created":    "任务已创建 · 首条输入已接收", "confirmed": "任务已创建 · 首条输入已接收",
		"canceled": "创建已取消", "expired": "草稿已过期 · 请重新发起",
		"repair": "任务位置不可用 · 需要修复",
	}[state]
	if status == "" {
		status = "创建状态待核验"
	}
	els := []element{creationText("**"+status+"**", "heading")}
	if v.Error != "" {
		els = append(els, creationText(creationLiteral(v.Error), "normal"))
	}
	if state == "draft" {
		if v.Token == "" || v.Revision <= 0 {
			return renderWidth(append(els, publicBodyElements(v.Body, "normal")...), "default")
		}
		form := []element{creationText("任务内容", "normal")}
		if utf8.RuneCountInString(v.Body) <= creationInputMaxRunes {
			form = append(form, element{"tag": "input", "name": "creation_body", "input_type": "multiline_text", "width": "fill", "default_value": v.Body, "placeholder": element{"tag": "plain_text", "content": "希望 Codex 完成什么任务？"}, "max_length": creationInputMaxRunes, "rows": 2, "auto_resize": true, "max_rows": 6})
		} else {
			form = append(form,
				taskPanel("原任务正文", publicBodyElements(v.Body, "normal")),
				creationText("将使用原任务正文；如需修改请重新发起 /new", "notation"),
			)
		}
		form = append(form, creationAttachmentElements(v, previewLimit)...)
		form = append(form, creationMainDirectoryElements(v)...)
		form = append(form, creationWorkspace(v)...)
		form = append(form, creationFollowPanel(v))
		submit := creationPrimary(creationButton(v, "创建任务", "creation_submit", nil, true))
		form = append(form, creationRow(submit, creationButton(v, "取消", "creation_cancel", nil, true)))
		els = append(els, creationForm("creation_form", form))
	} else {
		if v.WorkspaceMode == "worktree" {
			branch := strings.TrimPrefix(strings.TrimPrefix(v.BaseBranch, "refs/heads/"), "refs/remotes/")
			els = append(els, creationText("新建工作树 · "+creationLiteral(branch), "notation"))
		}
		if v.Body != "" {
			els = append(els, publicBodyElements(v.Body, "normal")...)
		}
		if v.Token != "" && v.Revision > 0 {
			if state == "rejected" {
				els = append(els, creationPrimary(creationButton(v, "继续创建", "creation_retry", nil, false)))
			}
			if state == "unknown" {
				els = append(els, creationButton(v, "核验结果", "creation_check", nil, false))
			}
		}
		if state == "created" || state == "confirmed" {
			if link := creationChatLink(v.ChatLink); link != "" {
				els = append(els, creationText("[打开任务位置]("+link+")", "normal"))
			}
		}
		details := "目录：" + v.Directory
		if v.ThreadID != "" {
			details += "\nCodex 线程：" + v.ThreadID
		}
		els = append(els, taskPanel("任务详情", []element{creationText(creationLiteral(details), "notation")}))
	}
	return renderWidth(els, "default")
}

// Leave room for the durable request marker and PATCH envelope below the
// platform's 30 KB limit. Trim only the view copy, never a canonical path.
const creationPickerBytes = 28 * 1024

func creationPicker(v CreationView) string {
	page := v.DirectoryPage
	if page == nil {
		page = v.SearchPage
	}
	copy := *page
	copy.Choices = append([]CreationDirectoryChoice(nil), page.Choices...)
	if v.DirectoryPage != nil {
		v.DirectoryPage = &copy
	} else {
		v.SearchPage = &copy
	}
	for {
		var card string
		if v.DirectoryPage != nil {
			card = creationDirectory(v)
		} else {
			card = creationSearch(v)
		}
		if len(card) <= creationPickerBytes {
			return card
		}
		if len(copy.Choices) > 1 {
			copy.Choices = copy.Choices[:len(copy.Choices)-1]
			copy.Next = copy.Choices[len(copy.Choices)-1].Path
			continue
		}
		// A single escaped path can consume the whole request budget. Never
		// truncate it into a different executable value or send an invalid card.
		return render([]element{
			creationText("目录信息超出卡片容量，原任务和选择已保留。请返回后查找更具体的目录。", "normal"),
			creationButton(v, "返回创建卡", "creation_back", nil, false),
		})
	}
}

func creationPageCount(p *CreationDirectoryPage) string {
	n := len(p.Choices)
	if p.Total > 0 {
		if n == 0 {
			return fmt.Sprintf("共 %d 个匹配目录，本批无结果", p.Total)
		}
		if p.Next == "" && p.Offset == 0 && n == p.Total {
			return fmt.Sprintf("本层 %d 项已全部载入", n)
		}
		return fmt.Sprintf("%d–%d 项 · 共 %d 项", p.Offset+1, p.Offset+n, p.Total)
	}
	if p.Next != "" {
		return fmt.Sprintf("本批载入 %d 项；可继续加载或查找", n)
	}
	return fmt.Sprintf("%d 个目录", n)
}

func creationPickerHeader(v CreationView, title string) []element {
	els := []element{creationText("**"+title+"**", "heading")}
	if v.Error != "" {
		els = append(els, creationText(creationLiteral(v.Error), "normal"))
	}
	return els
}

func creationDirectory(v CreationView) string {
	p := v.DirectoryPage
	els := creationPickerHeader(v, "浏览工作目录")
	if v.Token == "" || v.Revision <= 0 {
		return render(els)
	}
	els = append(els, creationQueryForm(v, "creation_lookup", "子目录名 / 完整路径", p.Filter))
	var up element
	if p.Parent != "" && p.Parent != p.Path {
		up = creationButton(v, "上一级", "creation_browse", element{"path": p.Parent}, false)
	}
	els = append(els, creationDirectorySummary(v, p.Path, "当前位置", up))
	if p.ReadOnly {
		els = append(els, creationText("此目录可浏览但不可用于任务，请进入可写子目录。", "notation"))
	}
	// Outside a form, selecting a child fires action.option immediately.
	// It navigates only; the separate primary action selects the current cwd.
	options := []element{}
	seen := map[string]bool{}
	for _, c := range p.Choices {
		if c.Path == "" || seen[c.Path] {
			continue
		}
		seen[c.Path] = true
		label := filepath.Base(c.Path)
		if p.Path == "" {
			label = c.Path
		}
		if c.ReadOnly {
			label += " · 只读"
		}
		options = append(options, element{"text": element{"tag": "plain_text", "content": label}, "value": c.Path})
	}
	if len(options) == 0 {
		message := "此目录没有可浏览的子目录。"
		if p.Filter != "" {
			message = "当前层没有匹配目录；可清空查找或输入完整路径。"
		}
		els = append(els, creationText(message, "notation"))
	} else {
		els = append(els, creationSelect(v, "creation_entry", "打开子目录…", "creation_browse", options), creationText(creationPageCount(p), "notation"))
	}
	navigation := []element{creationButton(v, "最近与项目", "creation_projects", nil, false)}
	if p.Next != "" {
		navigation = append(navigation, creationButton(v, "下一批", "creation_browse", element{"path": p.Path, "after": p.Next}, false))
	}
	pick := creationPrimary(creationButton(v, "使用当前目录", "creation_pick", element{"path": p.Path}, false))
	if p.Path == "" || p.ReadOnly {
		pick["disabled"] = true
	}
	els = append(els,
		creationRow(navigation...),
		creationRow(pick, creationButton(v, "取消", "creation_back", nil, false)),
	)
	return render(els)
}

func creationSearch(v CreationView) string {
	p := v.SearchPage
	els := creationPickerHeader(v, "最近目录与 Codex 项目")
	if v.Token == "" || v.Revision <= 0 {
		return render(els)
	}
	els = append(els, creationQueryForm(v, "creation_search", "项目名或完整路径", v.SearchQuery))
	if v.SearchQuery != "" {
		els = append(els, creationText("查找结果 · "+creationLiteral(v.SearchQuery), "notation"))
	}
	choices := []CreationDirectoryChoice{}
	seen := map[string]bool{}
	for _, c := range p.Choices {
		if c.Path != "" && !seen[c.Path] {
			choices = append(choices, c)
			seen[c.Path] = true
		}
	}
	if len(choices) == 0 {
		els = append(els, creationText("没有匹配目录，可输入完整路径或返回浏览目录。", "notation"))
	} else if len(choices) <= 6 {
		for i, c := range choices {
			label := c.Label
			if label == "" || label == c.Path {
				label = filepath.Base(c.Path)
			}
			button := creationButton(v, "使用", "creation_pick", element{"path": c.Path}, false)
			if c.ReadOnly {
				button = creationButton(v, "进入", "creation_browse", element{"path": c.Path}, false)
			}
			button["name"] = fmt.Sprintf("creation_result_%d", i)
			row := element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "columns": []element{
				{"tag": "column", "width": "weighted", "weight": 1, "elements": []element{creationText("**"+creationLiteral(label)+"**", "normal"), creationText(creationLiteral(c.Path), "notation")}},
				{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []element{button}},
			}}
			els = append(els, row)
		}
	} else {
		options := creationSelectOptions(choices, true)
		if len(options) > 0 {
			selectResult := element{"tag": "select_static", "name": "creation_search_choice", "type": "default", "width": "fill", "placeholder": element{"tag": "plain_text", "content": "选择要使用的目录"}, "options": options}
			els = append(els, creationForm("creation_result_form", []element{selectResult, creationRow(creationPrimary(creationButton(v, "使用所选目录", "creation_search_pick", nil, true)))}))
		}
		for _, c := range choices {
			if c.ReadOnly {
				els = append(els, creationText("只读目录请通过浏览进入其可写子目录。", "notation"))
				break
			}
		}
	}
	if len(choices) > 0 {
		els = append(els, creationText(creationPageCount(p), "notation"))
	}
	if p.Next != "" {
		els = append(els, creationRow(creationButton(v, "加载下一批结果", "creation_search_next", element{"after": p.Next}, false)))
	}
	els = append(els, creationRow(creationButton(v, "返回浏览目录", "creation_panel", nil, false), creationButton(v, "取消选择", "creation_back", nil, false)))
	return render(els)
}

func creationChatLink(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Host != "applink.feishu.cn" && u.Host != "applink.larksuite.com") || u.Path != "/client/chat/open" || u.Query().Get("openChatId") == "" {
		return ""
	}
	u.RawQuery = u.Query().Encode()
	return u.String()
}

// CreationRoot is the platform anchor, which may precede native creation. It
// deliberately makes no claim about App visibility or input acceptance.
func CreationRoot(body, directory, source string) string {
	els := publicBodyElements(body, "normal")
	if directory != "" {
		els = append(els, taskPanel("任务目录", []element{{"tag": "markdown", "content": creationLiteral(directory), "text_size": "notation"}}))
	}
	if source != "" {
		els = append(els, footer(source))
	}
	return renderWidth(els, "")
}
