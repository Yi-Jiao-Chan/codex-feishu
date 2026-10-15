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
			if tag == "button" || tag == "input" || tag == "select_static" {
				node["disabled"] = true
			}
			name, _ := node["name"].(string)
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
	if len(raw) > 29*1024 {
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
type CreationView struct {
	State, Error, Host, Body, Directory, ProjectLabel, FollowMode, TargetChatID string
	Token, ChatLink, ThreadID                                                   string
	Revision                                                                    int64
	TopicsEnabled                                                               bool
	Workgroups                                                                  []CreationOption
	QuickChoices                                                                []CreationDirectoryChoice
	SearchQuery                                                                 string
	SearchPage                                                                  *CreationDirectoryPage
	DirectoryPage                                                               *CreationDirectoryPage
	Busy                                                                        string
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
		if v.Host != "" {
			details = append(details, creationText("主机："+creationLiteral(v.Host), "notation"))
		}
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

func creationFollowPanel(v CreationView) element {
	if !v.TopicsEnabled || len(v.Workgroups) == 0 {
		return creationText("跟进位置：新建专属群", "notation")
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
	if v.Busy != "" {
		els := []element{creationText("**"+creationLiteral(v.Busy)+"**", "normal")}
		if v.Host != "" {
			els = append(els, creationText("主机："+creationLiteral(v.Host), "notation"))
		}
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
		"rejected": "创建未完成 · 已确认该步骤未被接受",
		"unknown":  "正在核验创建结果 · 请勿重复创建",
		"created":  "任务已创建 · 首条输入已接收", "confirmed": "任务已创建 · 首条输入已接收",
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
			return render(append(els, publicBodyElements(v.Body, "normal")...))
		}
		form := []element{creationText("任务内容", "notation")}
		if utf8.RuneCountInString(v.Body) <= creationInputMaxRunes {
			form = append(form, element{"tag": "input", "name": "creation_body", "input_type": "multiline_text", "width": "fill", "default_value": v.Body, "placeholder": element{"tag": "plain_text", "content": "希望 Codex 完成什么任务？"}, "max_length": creationInputMaxRunes, "rows": 2, "auto_resize": true, "max_rows": 6})
		} else {
			form = append(form,
				taskPanel("原任务正文", publicBodyElements(v.Body, "normal")),
				creationText("将使用原任务正文；如需修改请重新发起 /new", "notation"),
			)
		}
		form = append(form, creationDirectorySummary(v, v.Directory, "工作目录", creationButton(v, "更改目录", "creation_panel", nil, true)))
		form = append(form, creationFollowPanel(v))
		submit := creationPrimary(creationButton(v, "创建任务", "creation_submit", nil, true))
		if v.Directory == "" {
			submit["disabled"] = true
			form = append(form, creationText("请先选择工作目录，再创建任务。", "notation"))
		}
		form = append(form, creationRow(submit, creationButton(v, "取消", "creation_cancel", nil, true)))
		els = append(els, creationForm("creation_form", form))
	} else {
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
		details := "主机：" + v.Host + "\n目录：" + v.Directory
		if v.ThreadID != "" {
			details += "\nCodex 线程：" + v.ThreadID
		}
		els = append(els, taskPanel("任务详情", []element{creationText(creationLiteral(details), "notation")}))
	}
	return render(els)
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
