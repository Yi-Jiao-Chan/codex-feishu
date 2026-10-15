package cards

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// InteractionView is the server-verified projection of one native approval or
// question request. It deliberately contains no request ID, thread ID or
// other routing data: the opaque token and revision are the only callback
// identity exposed to the card.
type InteractionView struct {
	Kind, State, Reason, CWD, Command, Token, Summary string
	AnswerStatus                                      string
	Revision                                          int64
	MaxCardBytes                                      int
	Decisions                                         []InteractionDecision
	Details                                           []InteractionDetail
	Questions                                         []InteractionQuestion
	Permissions                                       []InteractionOption
	Enabled                                           bool
}

type InteractionDecision struct {
	Value, Label, Description string
}

type InteractionDetail struct {
	Title, Content string
}

type InteractionOption struct {
	Value, Label, Description string
}

type InteractionQuestion struct {
	Field, Header, Question string
	Options                 []InteractionOption
	Answers                 []string
	Other                   bool
}

const (
	interactionPermissionsName = "interaction_permissions"
	interactionAnswersName     = "interaction_answers"
	interactionMaxTextRunes    = 4096
	interactionMaxDetails      = 32
	interactionMaxQuestions    = 24
	interactionMaxOptions      = 32
	interactionMaxDecisions    = 8
	interactionMaxAnswers      = 64
)

// Interaction renders a pure Card JSON 2.0 approval or question card. The
// renderer never contacts Feishu and never infers a native response. In
// particular, a non-pending state retains its explanatory details but loses
// every action and form control.
func Interaction(v InteractionView) string {
	budget := v.MaxCardBytes
	if budget <= 0 || budget > DefaultMaxCardBytes {
		budget = DefaultMaxCardBytes
	}
	for _, limits := range []interactionLimits{
		{textRunes: interactionMaxTextRunes, details: interactionMaxDetails, questions: interactionMaxQuestions, options: interactionMaxOptions, answers: interactionMaxAnswers},
		{textRunes: 2048, details: 24, questions: 16, options: 24, answers: 32},
		{textRunes: 1024, details: 16, questions: 12, options: 16, answers: 24},
		{textRunes: 512, details: 8, questions: 8, options: 12, answers: 16},
		{textRunes: 256, details: 4, questions: 4, options: 8, answers: 8},
		{textRunes: 128, details: 2, questions: 2, options: 4, answers: 4},
		{textRunes: 64, details: 1, questions: 1, options: 2, answers: 2},
	} {
		card := renderInteraction(v, limits)
		if len(card) <= budget && interactionComponentCount(card) <= 200 {
			return card
		}
	}

	// Extremely large option values or opaque metadata can still consume the
	// complete budget after normal body truncation. Keep a useful, valid,
	// read-only preview in that pathological case.
	return renderInteraction(v, interactionLimits{textRunes: 32, details: 1, questions: 1, options: 1, answers: 1, minimal: true})
}

func interactionComponentCount(card string) int {
	var doc any
	if json.Unmarshal([]byte(card), &doc) != nil {
		return 201
	}
	var count func(any) int
	count = func(v any) int {
		n := 0
		switch value := v.(type) {
		case map[string]any:
			if _, tagged := value["tag"]; tagged {
				n++
			}
			for _, child := range value {
				n += count(child)
			}
		case []any:
			for _, child := range value {
				n += count(child)
			}
		}
		return n
	}
	return count(doc)
}

type interactionLimits struct {
	textRunes, details, questions, options, answers int
	minimal                                         bool
}

func renderInteraction(v InteractionView, limits interactionLimits) string {
	approval := !interactionIsQuestion(v.Kind)
	question := !approval
	canAct := v.Enabled && v.State == "pending" && v.Token != "" && v.Revision > 0
	limited := interactionDisplayLimited(v, limits, approval)
	elements := make([]element, 0, 8)
	if approval {
		title := interactionApprovalTitle(v)
		if title != "" {
			elements = append(elements, element{"tag": "markdown", "content": "**" + Safe(title) + "**", "text_size": "normal", "margin": "0px"})
		}
	}
	elements = append(elements, element{"tag": "markdown", "content": Safe(interactionStatus(v)), "text_size": "notation", "margin": "0px"})

	elements = append(elements, interactionBody(v, limits.textRunes, approval)...)
	if question && v.State != "pending" {
		elements = append(elements, interactionQuestionReceipt(v, limits)...)
		elements = append(elements, interactionQuestionDetails(v, limits)...)
	} else if question && (!canAct || limited) {
		elements = append(elements, interactionQuestionSummary(v, limits)...)
	}
	if v.Kind == "permissions" && (!canAct || limited) {
		elements = append(elements, interactionPermissionSummary(v, limits)...)
	}
	elements = append(elements, interactionDetails(v.Details, limits.textRunes, limits.details)...)

	if canAct && !limited {
		if question {
			elements = append(elements, interactionQuestionForm(v, limits)...)
		} else if v.Kind == "permissions" {
			elements = append(elements, interactionPermissionControls(v, limits)...)
		} else {
			elements = append(elements, interactionDecisionControls(v, limits.textRunes)...)
		}
	} else if v.State == "pending" && !limited {
		// A disabled button alone gives no explanation on clients that do not
		// render disabled controls. Keep the reason in the normal body.
		prompt := "此请求暂不能在飞书提交，请回 Codex App 处理。"
		if v.Kind == "async_question" {
			prompt = "请在 Codex App 中回答。"
		}
		elements = append(elements, element{"tag": "markdown", "content": prompt, "text_size": "normal", "margin": "0px"})
	}

	if v.State != "pending" && strings.TrimSpace(v.Summary) != "" {
		receipt := strings.ReplaceAll(v.Summary, "审批成功", "请求已处理")
		elements = append(elements, element{"tag": "markdown", "content": interactionText(receipt, limits.textRunes), "text_size": "notation", "margin": "0px"})
	}
	if limited {
		elements = append(elements, element{"tag": "markdown", "content": "内容超出卡片展示范围，请回 Codex App 处理", "text_size": "normal", "margin": "0px"})
	}
	if omitted := interactionOmittedSummary(v, limits); omitted != "" && !limits.minimal {
		elements = append(elements, element{"tag": "markdown", "content": omitted, "text_size": "notation", "margin": "0px"})
	}
	elements = append(elements, footer(interactionSource(v.Kind)))

	width := "compact"
	return renderInteractionCard(elements, width)
}

func interactionIsQuestion(kind string) bool {
	return kind == "question" || kind == "async_question"
}

func interactionSource(kind string) string {
	if interactionIsQuestion(kind) {
		return "用户问答 · Codex"
	}
	return "审批请求 · Codex"
}

func interactionApprovalTitle(v InteractionView) string {
	pending := v.State == "pending"
	switch v.Kind {
	case "command":
		if pending {
			return "允许执行这条命令？"
		}
		return "命令审批请求"
	case "file":
		if pending {
			return "允许修改这些文件？"
		}
		return "文件修改请求"
	case "network":
		if pending {
			return "允许访问这个网络地址？"
		}
		return "网络访问请求"
	case "permissions":
		if pending {
			return "允许这些临时权限？"
		}
		return "临时权限请求"
	default:
		if pending {
			return "需要你的确认"
		}
		return "审批请求"
	}
}

func interactionStatus(v InteractionView) string {
	question := interactionIsQuestion(v.Kind)
	if v.State == "pending" {
		if question {
			label := "等待你回答"
			if len(v.Questions) > 0 {
				label += fmt.Sprintf(" · %d 项", len(v.Questions))
			}
			return label
		}
		return "等待你确认"
	}
	switch v.State {
	case "claimed":
		return "已收到 · 正在提交"
	case "submitted":
		return "已提交 · 等待状态确认"
	case "unknown":
		return "正在核验 · 请勿重复提交"
	case "resolved":
		if question {
			if answered := interactionAnsweredQuestionCount(v); v.AnswerStatus == "recorded" && answered > 0 {
				return fmt.Sprintf("已回答 · %d 项", answered)
			}
			if len(v.Questions) > 0 && interactionAnsweredQuestionCount(v) == 0 {
				return "请求已处理 · 暂未取得回答"
			}
		}
		return "请求已处理"
	case "disconnected":
		return "连接已断开 · 请回 Codex App 核验"
	case "unsupported":
		return "此请求暂不支持在飞书处理 · 请回 Codex App"
	case "queued":
		if question {
			return "回答已排队 · 等待进入会话"
		}
		return "已排队 · 等待进入会话"
	case "received":
		return "回答已保存 · 等待入队"
	case "consumed":
		if question {
			return "回答已进入会话"
		}
		return "已进入会话"
	case "failed":
		return "提交失败 · 请回 Codex App 处理"
	default:
		return "状态待核验 · 请回 Codex App 查看"
	}
}

func interactionBody(v InteractionView, maxRunes int, approval bool) []element {
	var body []element
	if strings.TrimSpace(v.Reason) != "" {
		body = append(body, interactionNormalElements(v.Reason, maxRunes)...)
	}
	if approval && strings.TrimSpace(v.Command) != "" {
		body = append(body, interactionCommandElements(v.Command, maxRunes)...)
	}
	if strings.TrimSpace(v.CWD) != "" {
		body = append(body, interactionCWD(v.CWD, maxRunes)...)
	}
	if !approval {
		// Questions are rendered inside their top-level form. Keep no copied
		// question text here so the prompt is displayed exactly once.
		return body
	}
	return body
}

func interactionCommandElements(command string, maxRunes int) []element {
	if interactionLong(command) {
		return []element{interactionCommandPanel(command, maxRunes)}
	}
	display, truncated := interactionClip(command, maxRunes)
	elements := []element{interactionCodeElement(display)}
	if truncated {
		elements = append(elements, interactionTruncationNote("命令"))
	}
	return elements
}

func interactionCommandPanel(command string, maxRunes int) element {
	display, truncated := interactionClip(command, maxRunes)
	children := []element{interactionCodeElement(display)}
	if truncated {
		children = append(children, interactionTruncationNote("命令"))
	}
	return interactionPanelElements("完整命令", children)
}

func interactionCodeElement(command string) element {
	fence := interactionCodeFence(command)
	content := fence + "bash\n" + command
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += fence
	return element{"tag": "markdown", "content": SafeMarkdown(content), "text_size": "normal", "margin": "0px"}
}

func interactionCodeFence(command string) string {
	width := 3
	for _, line := range strings.Split(command, "\n") {
		for i := 0; i < len(line); {
			if line[i] != '`' {
				i++
				continue
			}
			start := i
			for i < len(line) && line[i] == '`' {
				i++
			}
			if run := i - start; run >= width {
				width = run + 1
			}
		}
	}
	return strings.Repeat("`", width)
}

func interactionCWD(cwd string, maxRunes int) []element {
	display, truncated := interactionClip(cwd, maxRunes)
	if interactionLong(cwd) {
		children := []element{{"tag": "markdown", "content": Safe(display), "text_size": "notation", "margin": "0px"}}
		if truncated {
			children = append(children, interactionTruncationNote("工作目录"))
		}
		return []element{interactionPanelElements("工作目录", children)}
	}
	elements := []element{{"tag": "markdown", "content": "工作目录：" + Safe(display), "text_size": "notation", "margin": "0px"}}
	if truncated {
		elements = append(elements, interactionTruncationNote("工作目录"))
	}
	return elements
}

func interactionTruncationNote(label string) element {
	return element{"tag": "markdown", "content": fmt.Sprintf("%s内容已截短，后续内容已省略；请回 Codex App 查看。", label), "text_size": "notation", "margin": "0px"}
}

// interactionNormalElements intentionally goes through the shared public
// Markdown block renderer so paragraph boundaries and the native body limit
// stay identical to ordinary messages.
func interactionNormalElements(value string, maxRunes int) []element {
	clipped, truncated := interactionClip(value, maxRunes)
	if truncated {
		clipped += "\n\n…（内容已截短，后续内容已省略；请回 Codex App 查看）"
	}
	return publicBodyElements(clipped, "normal")
}

// interactionDisplayLimited reports whether the current rendering limits have
// omitted or shortened any user-visible request data. Actions are withheld
// for the whole card in that case: a partial approval scope or partial answer
// form must never be submitted as if it represented the original request.
func interactionDisplayLimited(v InteractionView, limits interactionLimits, approval bool) bool {
	if limits.minimal {
		return true
	}
	if interactionNeedsClip(v.Reason, limits.textRunes) || interactionNeedsClip("工作目录："+v.CWD, limits.textRunes) {
		return true
	}
	if approval && strings.TrimSpace(v.Command) != "" && interactionNeedsClip(v.Command, limits.textRunes) {
		return true
	}

	if limits.details <= 0 && len(v.Details) > 0 {
		return true
	}
	if len(v.Details) > limits.details {
		return true
	}
	for _, detail := range v.Details {
		if interactionNeedsClip(detail.Title, limits.textRunes) || interactionNeedsClip(detail.Content, limits.textRunes) {
			return true
		}
	}

	if interactionIsQuestion(v.Kind) {
		if limits.questions <= 0 && len(v.Questions) > 0 {
			return true
		}
		if len(v.Questions) > limits.questions {
			return true
		}
		for _, question := range v.Questions {
			if utf8.RuneCountInString(strings.Join(strings.Fields(question.Header), " ")) > 80 || interactionNeedsClip(question.Header, limits.textRunes) || interactionNeedsClip(question.Question, limits.textRunes) {
				return true
			}
			if limits.options <= 0 && len(question.Options) > 0 {
				return true
			}
			if len(question.Options) > limits.options {
				return true
			}
			for _, option := range question.Options {
				if interactionNeedsClip(option.Value, limits.textRunes) || interactionNeedsClip(option.Label, limits.textRunes) || interactionNeedsClip(option.Description, limits.textRunes) {
					return true
				}
			}
		}
		if v.State != "pending" && interactionAnswerEvidence(v) {
			answerCount := interactionAnswerValueCount(v)
			if (limits.answers <= 0 && answerCount > 0) || answerCount > limits.answers {
				return true
			}
			for _, question := range v.Questions {
				for _, answer := range question.Answers {
					if strings.TrimSpace(answer) != "" && interactionNeedsClip(answer, limits.textRunes) {
						return true
					}
				}
			}
		}
		return false
	}

	if v.Kind == "permissions" {
		if limits.options <= 0 && len(v.Permissions) > 0 {
			return true
		}
		if len(v.Permissions) > limits.options {
			return true
		}
		for _, permission := range v.Permissions {
			if interactionNeedsClip(permission.Value, limits.textRunes) || interactionNeedsClip(permission.Label, limits.textRunes) || interactionNeedsClip(permission.Description, limits.textRunes) {
				return true
			}
		}
	}
	if len(v.Decisions) > interactionMaxDecisions {
		return true
	}
	for _, decision := range v.Decisions {
		if utf8.RuneCountInString(strings.Join(strings.Fields(decision.Label), " ")) > 80 || interactionNeedsClip(decision.Value, limits.textRunes) || interactionNeedsClip(decision.Description, limits.textRunes) {
			return true
		}
	}
	return false
}

func interactionNeedsClip(value string, maxRunes int) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	return maxRunes <= 0 || utf8.RuneCountInString(value) > maxRunes
}

func interactionQuestionSummary(v InteractionView, limits interactionLimits) []element {
	if len(v.Questions) == 0 || limits.questions <= 0 {
		return nil
	}
	questions := v.Questions
	if len(questions) > limits.questions {
		questions = questions[:limits.questions]
	}
	result := make([]element, 0, len(questions)*4)
	for i, question := range questions {
		if i > 0 {
			result = append(result, element{"tag": "hr", "margin": "0px"})
		}
		if header := interactionQuestionHeader(i, len(v.Questions), question.Header); header != "" {
			result = append(result, element{"tag": "markdown", "content": Safe(header), "text_size": "notation", "margin": "0px"})
		}
		result = append(result, interactionQuestionPrompt(question.Question, limits.textRunes)...)
		options := question.Options
		if len(options) > limits.options && limits.options > 0 {
			options = options[:limits.options]
		}
		for _, option := range options {
			result = append(result, interactionQuestionOptionDescription(option, limits.textRunes)...)
		}
	}
	return result
}

// interactionQuestionReceipt keeps closed question cards useful at a glance:
// the question and a verified answer are visible, while the native options
// remain in the collapsed detail panel below. Empty AnswerStatus deliberately
// suppresses Answers because it is an explicit lack of answer evidence.
func interactionQuestionReceipt(v InteractionView, limits interactionLimits) []element {
	if len(v.Questions) == 0 || limits.questions <= 0 {
		return nil
	}
	questions := v.Questions
	if len(questions) > limits.questions {
		questions = questions[:limits.questions]
	}
	answerEvidence := interactionAnswerEvidence(v)
	remainingAnswers := limits.answers
	labelAdded := false
	result := make([]element, 0, len(questions)*2+1)
	for _, question := range questions {
		result = append(result, interactionQuestionPrompt(question.Question, limits.textRunes)...)
		if !answerEvidence {
			continue
		}
		for _, answer := range question.Answers {
			if strings.TrimSpace(answer) == "" {
				continue
			}
			if remainingAnswers <= 0 {
				break
			}
			if v.AnswerStatus == "submitted" && !labelAdded {
				result = append(result, element{"tag": "markdown", "content": "已提交的回答", "text_size": "notation", "margin": "0px"})
				labelAdded = true
			}
			result = append(result, interactionAnswerElements(answer, limits.textRunes)...)
			remainingAnswers--
		}
	}
	return result
}

// interactionQuestionDetails is deliberately separate from the receipt. The
// collapsed native panel retains the exact question metadata and option
// descriptions without making the closed card as tall as the pending form.
func interactionQuestionDetails(v InteractionView, limits interactionLimits) []element {
	if len(v.Questions) == 0 || limits.questions <= 0 {
		return nil
	}
	questions := v.Questions
	if len(questions) > limits.questions {
		questions = questions[:limits.questions]
	}
	children := make([]element, 0, len(questions)*4)
	for i, question := range questions {
		if i > 0 && len(children) > 0 {
			children = append(children, element{"tag": "hr", "margin": "0px"})
		}
		if header := interactionQuestionHeader(i, len(v.Questions), question.Header); header != "" {
			children = append(children, element{"tag": "markdown", "content": Safe(header), "text_size": "notation", "margin": "0px"})
		}
		children = append(children, interactionQuestionPrompt(question.Question, limits.textRunes)...)
		options := question.Options
		if len(options) > limits.options && limits.options > 0 {
			options = options[:limits.options]
		}
		for _, option := range options {
			children = append(children, interactionQuestionDetailOption(option, limits.textRunes)...)
		}
	}
	if len(children) == 0 {
		return nil
	}
	return []element{interactionPanelElements("问题详情", children)}
}

func interactionQuestionDetailOption(option InteractionOption, maxRunes int) []element {
	label := option.Label
	if label == "" {
		label = option.Value
	}
	displayLabel, labelTruncated := interactionClip(label, maxRunes)
	content := Safe(displayLabel)
	if strings.TrimSpace(option.Description) != "" {
		description, truncated := interactionMarkdown(option.Description, maxRunes)
		content += "：" + description
		result := []element{{"tag": "markdown", "content": content, "text_size": "notation", "margin": "0px"}}
		if labelTruncated {
			result = append(result, interactionTruncationNote("选项"))
		}
		if truncated {
			result = append(result, interactionTruncationNote("选项说明"))
		}
		return result
	}
	result := []element{{"tag": "markdown", "content": content, "text_size": "notation", "margin": "0px"}}
	if labelTruncated {
		result = append(result, interactionTruncationNote("选项"))
	}
	return result
}

func interactionAnswerElements(answer string, maxRunes int) []element {
	if strings.TrimSpace(answer) == "" {
		return nil
	}
	display, truncated := interactionClip(answer, maxRunes)
	if !interactionLong(answer) && !truncated {
		return interactionNormalElements(answer, maxRunes)
	}
	children := publicBodyElements(display, "normal")
	if truncated {
		children = append(children, interactionTruncationNote("回答"))
	}
	return []element{interactionPanelElements("回答详情", children)}
}

func interactionAnswerEvidence(v InteractionView) bool {
	return v.AnswerStatus == "recorded" || v.AnswerStatus == "submitted"
}

func interactionAnsweredQuestionCount(v InteractionView) int {
	if !interactionAnswerEvidence(v) {
		return 0
	}
	count := 0
	for _, question := range v.Questions {
		for _, answer := range question.Answers {
			if strings.TrimSpace(answer) != "" {
				count++
				break
			}
		}
	}
	return count
}

func interactionAnswerValueCount(v InteractionView) int {
	if !interactionAnswerEvidence(v) {
		return 0
	}
	count := 0
	for _, question := range v.Questions {
		for _, answer := range question.Answers {
			if strings.TrimSpace(answer) != "" {
				count++
			}
		}
	}
	return count
}

func interactionPermissionSummary(v InteractionView, limits interactionLimits) []element {
	if len(v.Permissions) == 0 || limits.options <= 0 {
		return nil
	}
	var lines []string
	for i, permission := range v.Permissions {
		if i >= limits.options {
			break
		}
		label := permission.Label
		if label == "" {
			label = permission.Value
		}
		line := interactionText(label, limits.textRunes)
		if strings.TrimSpace(permission.Description) != "" {
			line += "：" + interactionText(permission.Description, limits.textRunes)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil
	}
	return []element{{"tag": "markdown", "content": strings.Join(lines, "\n"), "text_size": "normal", "margin": "0px"}}
}

func interactionLong(value string) bool {
	return utf8.RuneCountInString(value) > 180 || strings.ContainsAny(value, "\r\n")
}

func interactionDetails(details []InteractionDetail, maxRunes, maxDetails int) []element {
	if maxDetails <= 0 || len(details) == 0 {
		return nil
	}
	limit := len(details)
	if limit > maxDetails {
		limit = maxDetails
	}
	result := make([]element, 0, limit)
	for i := 0; i < limit; i++ {
		title := strings.TrimSpace(details[i].Title)
		if title == "" {
			title = fmt.Sprintf("详细信息 %d", i+1)
		}
		panel := interactionPanel(interactionLine(title, 80), details[i].Content, maxRunes)
		if utf8.RuneCountInString(title) > 80 {
			fullTitle, truncated := interactionClip(title, maxRunes)
			children := []element{{"tag": "markdown", "content": Safe(fullTitle), "text_size": "notation", "margin": "0px"}}
			if truncated {
				children = append(children, interactionTruncationNote("详情标题"))
			}
			panel["elements"] = append(children, panel["elements"].([]element)...)
		}
		result = append(result, panel)
	}
	return result
}

func interactionPanel(title, content string, maxRunes int) element {
	display, truncated := interactionMarkdown(content, maxRunes)
	children := []element{{"tag": "markdown", "content": display, "text_size": "normal", "margin": "0px"}}
	if truncated {
		children = append(children, interactionTruncationNote("详情"))
	}
	return interactionPanelElements(title, children)
}

func interactionPanelElements(title string, children []element) element {
	return element{
		"tag": "collapsible_panel", "expanded": false, "padding": "0px", "vertical_spacing": "4px", "margin": "0px",
		"header": element{
			"title":         element{"tag": "plain_text", "content": Safe(interactionLine(title, 80)), "text_size": "notation", "text_color": "grey"},
			"padding":       "0px",
			"icon":          element{"tag": "standard_icon", "token": "down-small-ccm_outlined", "size": "16px 16px"},
			"icon_position": "right",
		},
		"elements": children,
	}
}

func interactionMarkdown(value string, maxRunes int) (string, bool) {
	if value == "" {
		return "", false
	}
	clipped, truncated := interactionClip(value, maxRunes)
	return SafeMarkdown(clipped), truncated
}

func interactionText(value string, maxRunes int) string {
	clipped, truncated := interactionClip(value, maxRunes)
	if truncated {
		clipped += "…"
	}
	return SafeMarkdown(clipped)
}

func interactionLine(value string, maxRunes int) string {
	line := strings.Join(strings.Fields(value), " ")
	clipped, truncated := interactionClip(line, maxRunes)
	if truncated {
		clipped += "…"
	}
	return clipped
}

func interactionClip(value string, maxRunes int) (string, bool) {
	if maxRunes <= 0 || utf8.RuneCountInString(value) <= maxRunes {
		return value, false
	}
	runes := []rune(value)
	return string(runes[:maxRunes]), true
}

func interactionDecisionControls(v InteractionView, maxRunes int) []element {
	var controls []element
	if len(v.Decisions) == 0 {
		return controls
	}
	decisions := v.Decisions
	if len(decisions) > interactionMaxDecisions {
		decisions = decisions[:interactionMaxDecisions]
	}
	if descriptions := interactionDecisionDescriptions(decisions, maxRunes); descriptions != "" {
		controls = append(controls, element{"tag": "markdown", "content": descriptions, "text_size": "normal", "margin": "0px"})
	}
	buttons := make([]element, 0, len(decisions))
	for _, decision := range decisions {
		label := decision.Label
		if strings.TrimSpace(label) == "" {
			label = decision.Value
		}
		buttons = append(buttons, element{"tag": "column", "width": "auto", "elements": []element{interactionCallbackButton(interactionLine(label, 80), decision.Value, v.Token, v.Revision)}})
	}
	if len(buttons) > 0 {
		controls = append(controls, element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "columns": buttons})
	}
	return controls
}

func interactionDecisionDescriptions(decisions []InteractionDecision, maxRunes int) string {
	var lines []string
	for _, decision := range decisions {
		if strings.TrimSpace(decision.Description) == "" {
			continue
		}
		label := decision.Label
		if label == "" {
			label = decision.Value
		}
		lines = append(lines, interactionText(label, maxRunes)+"："+interactionText(decision.Description, maxRunes))
	}
	return strings.Join(lines, "\n")
}

func interactionCallbackButton(label, decision, token string, revision int64) element {
	button := Button(Safe(label), "interaction_submit", token)
	button["type"] = interactionButtonType(decision)
	button["behaviors"] = []element{{"type": "callback", "value": element{
		"action": "interaction_submit", "token": token, "revision": revision, "decision": decision,
	}}}
	return button
}

func interactionButtonType(decision string) string {
	switch decision {
	case "accept", "acceptForSession", "submit":
		return "primary"
	case "decline", "cancel":
		return "default"
	default:
		return "default"
	}
}

func interactionFormSubmitButton(label, name, token string, revision int64) element {
	return interactionFormButton(label, name, token, revision, "", false)
}

func interactionFormDecisionButton(label, name, decision, token string, revision int64) element {
	return interactionFormButton(label, name, token, revision, decision, true)
}

func interactionFormButton(label, name, token string, revision int64, decision string, includeDecision bool) element {
	value := element{"action": "interaction_submit", "token": token, "revision": revision}
	buttonType := interactionButtonType("submit")
	if includeDecision {
		value["decision"] = decision
		buttonType = interactionButtonType(decision)
	}
	return element{
		"tag": "button", "type": buttonType, "size": "small", "name": name,
		"text":             element{"tag": "plain_text", "content": Safe(label)},
		"form_action_type": "submit",
		"behaviors":        []element{{"type": "callback", "value": value}},
	}
}

func interactionPermissionControls(v InteractionView, limits interactionLimits) []element {
	var controls []element
	if descriptions := interactionPermissionDescriptions(v.Permissions, limits.textRunes, limits.options); descriptions != "" {
		controls = append(controls, element{"tag": "markdown", "content": descriptions, "text_size": "normal", "margin": "0px"})
	}
	if len(v.Permissions) > 0 && limits.options > 0 {
		options := make([]element, 0, interactionMinInt(len(v.Permissions), limits.options))
		for i, permission := range v.Permissions {
			if i >= limits.options {
				break
			}
			options = append(options, interactionOption(permission))
		}
		formElements := []element{{
			"tag": "multi_select_static", "name": "permissions",
			"placeholder": element{"tag": "plain_text", "content": "选择要授予的范围"},
			"options":     options,
		}, interactionPermissionButtonRow(v.Token, v.Revision)}
		controls = append(controls, element{"tag": "form", "name": interactionPermissionsName, "elements": formElements})
	} else {
		// Keep the fixed decisions in the same form row even when the native
		// request has no selectable grant. The allow-subset button remains
		// submit-shaped, and the backend rejects an empty selection.
		controls = append(controls, element{"tag": "form", "name": interactionPermissionsName, "elements": []element{interactionPermissionButtonRow(v.Token, v.Revision)}})
	}
	return controls
}

func interactionPermissionButtonRow(token string, revision int64) element {
	buttons := []element{
		{"tag": "column", "width": "auto", "elements": []element{interactionFormSubmitButton("允许", "submit_permissions", token, revision)}},
		{"tag": "column", "width": "auto", "elements": []element{interactionFormDecisionButton("允许本轮全部", "allow_all", "accept", token, revision)}},
		{"tag": "column", "width": "auto", "elements": []element{interactionFormDecisionButton("拒绝", "decline", "decline", token, revision)}},
	}
	return element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "columns": buttons}
}

func interactionPermissionDescriptions(options []InteractionOption, maxRunes, maxOptions int) string {
	if len(options) == 0 || maxOptions <= 0 {
		return ""
	}
	var lines []string
	for i, option := range options {
		if i >= maxOptions {
			break
		}
		if strings.TrimSpace(option.Description) == "" {
			continue
		}
		label := option.Label
		if label == "" {
			label = option.Value
		}
		lines = append(lines, interactionText(label, maxRunes)+"："+interactionText(option.Description, maxRunes))
	}
	return strings.Join(lines, "\n")
}

func interactionQuestionForm(v InteractionView, limits interactionLimits) []element {
	if len(v.Questions) == 0 || limits.questions <= 0 {
		return nil
	}
	questions := v.Questions
	if len(questions) > limits.questions {
		questions = questions[:limits.questions]
	}
	used := map[string]bool{}
	formElements := make([]element, 0, len(questions)*5+1)
	for i, question := range questions {
		if i > 0 {
			formElements = append(formElements, element{"tag": "hr", "margin": "0px"})
		}
		field := interactionFieldName(question.Field, fmt.Sprintf("q_%d", i), used)
		if header := interactionQuestionHeader(i, len(questions), question.Header); header != "" {
			formElements = append(formElements, element{"tag": "markdown", "content": Safe(header), "text_size": "notation", "margin": "0px"})
		}
		formElements = append(formElements, interactionQuestionPrompt(question.Question, limits.textRunes)...)
		options := question.Options
		if len(options) > limits.options && limits.options > 0 {
			options = options[:limits.options]
		}
		for _, option := range options {
			formElements = append(formElements, interactionQuestionOptionDescription(option, limits.textRunes)...)
		}
		if len(options) > 0 {
			selectOptions := make([]element, 0, len(options))
			for _, option := range options {
				selectOptions = append(selectOptions, interactionOption(option))
			}
			formElements = append(formElements, element{
				"tag": "select_static", "name": field,
				"width":       "fill",
				"placeholder": element{"tag": "plain_text", "content": "请选择一个答案"},
				"options":     selectOptions,
			})
		}
		input := element{
			"tag": "input", "name": field + "_notes", "input_type": "multiline_text",
			"width":       "fill",
			"placeholder": element{"tag": "plain_text", "content": interactionQuestionPlaceholder(len(options) > 0)},
			"max_length":  1000, "rows": 1, "auto_resize": true, "max_rows": 4,
		}
		if len(options) == 0 {
			input["required"] = true
		}
		formElements = append(formElements, input)
	}
	formElements = append(formElements, interactionButtonRow(interactionFormSubmitButton("提交回答", "submit_answers", v.Token, v.Revision)))
	return []element{{"tag": "form", "name": interactionAnswersName, "elements": formElements}}
}

func interactionQuestionHeader(index, total int, header string) string {
	header = interactionLine(header, 80)
	if header == "" {
		return ""
	}
	if total > 1 {
		return fmt.Sprintf("%d / %d · %s", index+1, total, header)
	}
	return header
}

func interactionQuestionPrompt(question string, maxRunes int) []element {
	if strings.TrimSpace(question) == "" {
		return nil
	}
	display, truncated := interactionClip(question, maxRunes)
	result := []element{{"tag": "markdown", "content": SafeMarkdown("**" + display + "**"), "text_size": "normal", "margin": "0px"}}
	if truncated {
		result = append(result, interactionTruncationNote("问题"))
	}
	return result
}

func interactionQuestionOptionDescription(option InteractionOption, maxRunes int) []element {
	if strings.TrimSpace(option.Description) == "" {
		return nil
	}
	label := option.Label
	if label == "" {
		label = option.Value
	}
	description, truncated := interactionMarkdown(option.Description, maxRunes)
	result := []element{{"tag": "markdown", "content": Safe(interactionLine(label, 80)) + "：" + description, "text_size": "notation", "margin": "0px"}}
	if truncated {
		result = append(result, interactionTruncationNote("选项说明"))
	}
	return result
}

func interactionQuestionPlaceholder(hasOptions bool) string {
	if hasOptions {
		return "补充说明或其他答案（选填）"
	}
	return "填写回答"
}

func interactionButtonRow(button element) element {
	return element{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "8px", "columns": []element{{"tag": "column", "width": "auto", "elements": []element{button}}}}
}

func interactionOption(option InteractionOption) element {
	label := option.Label
	if strings.TrimSpace(label) == "" {
		label = option.Value
	}
	return element{"text": element{"tag": "plain_text", "content": Safe(label)}, "value": option.Value}
}

func interactionFieldName(value, fallback string, used map[string]bool) string {
	name := value
	if !interactionSafeName(name) {
		name = fallback
	}
	if name == "" {
		name = fallback
	}
	base := name
	for n := 2; used[name]; n++ {
		name = fmt.Sprintf("%s_%d", base, n)
	}
	used[name] = true
	used[name+"_notes"] = true
	return name
}

func interactionSafeName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			if i == 0 && r >= '0' && r <= '9' {
				return false
			}
			continue
		}
		return false
	}
	return true
}

func interactionOmittedSummary(v InteractionView, limits interactionLimits) string {
	var notes []string
	if limits.details >= 0 && len(v.Details) > limits.details {
		notes = append(notes, fmt.Sprintf("%d 条详情已省略，请回 Codex App 查看。", len(v.Details)-limits.details))
	}
	if !interactionIsQuestion(v.Kind) && limits.options >= 0 && len(v.Permissions) > limits.options && v.Kind == "permissions" {
		notes = append(notes, fmt.Sprintf("%d 个权限范围未在卡片显示，请回 Codex App 查看。", len(v.Permissions)-limits.options))
	}
	if interactionIsQuestion(v.Kind) && limits.questions >= 0 && len(v.Questions) > limits.questions {
		notes = append(notes, fmt.Sprintf("%d 个问题已省略，请回 Codex App 查看。", len(v.Questions)-limits.questions))
	}
	if !interactionIsQuestion(v.Kind) && len(v.Decisions) > interactionMaxDecisions {
		notes = append(notes, fmt.Sprintf("%d 个决定未在卡片显示，请回 Codex App 查看。", len(v.Decisions)-interactionMaxDecisions))
	}
	if interactionIsQuestion(v.Kind) && limits.options >= 0 {
		omitted := 0
		for _, question := range v.Questions {
			if len(question.Options) > limits.options {
				omitted += len(question.Options) - limits.options
			}
		}
		if omitted > 0 {
			notes = append(notes, fmt.Sprintf("%d 个选项已省略，请回 Codex App 查看。", omitted))
		}
	}
	if interactionIsQuestion(v.Kind) && v.State != "pending" && interactionAnswerEvidence(v) && limits.answers >= 0 {
		if omitted := interactionAnswerValueCount(v) - limits.answers; omitted > 0 {
			notes = append(notes, fmt.Sprintf("%d 个回答已省略，请回 Codex App 查看。", omitted))
		}
	}
	return strings.Join(notes, "\n")
}

func renderInteractionCard(elements []element, width string) string {
	base := renderWidth(elements, width)
	var doc element
	if err := json.Unmarshal([]byte(base), &doc); err != nil {
		// renderWidth is already a JSON serializer; this is unreachable unless
		// its implementation changes. Returning its output preserves the pure
		// renderer contract without introducing an error return to the API.
		return base
	}
	config, ok := doc["config"].(map[string]any)
	if !ok {
		config = element{}
		doc["config"] = config
	}
	config["enable_forward"] = false
	config["enable_forward_interaction"] = false
	encoded, err := json.Marshal(doc)
	if err != nil {
		return base
	}
	return string(encoded)
}

func interactionMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
