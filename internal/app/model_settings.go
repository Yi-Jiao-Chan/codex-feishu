package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type modelSettingsSelection struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
	Fast   string `json:"fast"`
}

type modelSettingsInputError struct{ reason string }

func (e *modelSettingsInputError) Error() string { return e.reason }

func modelSettingsValue(value string) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 128 &&
		strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func modelSettingsSummary(current codexclient.ThreadSettings) string {
	return cards.ModelSettingsText(current.Model, current.ReasoningEffort, current.ServiceTier, current.ServiceTierKnown)
}

func modelSettingsDefaults(current codexclient.ThreadSettings, models []codexclient.Model) modelSettingsSelection {
	selection := modelSettingsSelection{Effort: "keep", Fast: "keep"}
	if model, ok := findSettingsModel(models, current.Model); ok {
		selection.Model = model.ID
	}
	if current.ReasoningEffort != nil {
		selection.Effort = *current.ReasoningEffort
	}
	if current.ServiceTierKnown && current.ServiceTier != nil {
		switch *current.ServiceTier {
		case "fast", "priority":
			selection.Fast = "on"
		case "default":
			selection.Fast = "off"
		}
	}
	return selection
}

func modelSettingsView(current codexclient.ThreadSettings, models []codexclient.Model, selection modelSettingsSelection, token string, revision int64) cards.ModelSettingsView {
	v := cards.ModelSettingsView{State: "editing", Current: modelSettingsSummary(current), Model: selection.Model, Effort: selection.Effort, Fast: selection.Fast, Token: token, Revision: revision}
	seen := map[string]bool{}
	if current.ReasoningEffort == nil {
		v.Efforts = append(v.Efforts, cards.ModelSettingsOption{Label: "默认", Value: "keep"})
		seen["keep"] = true
	}
	for _, model := range models {
		if model.Hidden || !modelSettingsValue(model.ID) || !modelSettingsValue(model.Model) {
			continue
		}
		v.Models = append(v.Models, cards.ModelSettingsOption{Label: model.ID, Value: model.ID})
		for _, effort := range model.SupportedReasoningEfforts {
			if modelSettingsValue(effort.ReasoningEffort) && !seen[effort.ReasoningEffort] {
				v.Efforts = append(v.Efforts, cards.ModelSettingsOption{Label: effort.ReasoningEffort, Value: effort.ReasoningEffort})
				seen[effort.ReasoningEffort] = true
			}
		}
	}
	// A current value outside the catalog remains visible in the summary, but
	// it is not silently added as an actionable unsupported choice.
	if !seen[v.Effort] {
		v.Effort = ""
	}
	return v
}

func sameModelSettings(a, b codexclient.ThreadSettings) bool {
	equal := func(left, right *string) bool {
		return left == nil && right == nil || left != nil && right != nil && *left == *right
	}
	return a.ThreadID == b.ThreadID && a.Model == b.Model && equal(a.ReasoningEffort, b.ReasoningEffort) &&
		a.ServiceTierKnown == b.ServiceTierKnown && equal(a.ServiceTier, b.ServiceTier)
}

// requestedModelSettings validates a whole form and creates one native patch.
// No provider, approval, sandbox or other session policy enters this request.
func requestedModelSettings(selection modelSettingsSelection, current codexclient.ThreadSettings, models []codexclient.Model) (codexclient.SettingsPatch, string) {
	var patch codexclient.SettingsPatch
	if !modelSettingsValue(selection.Model) || !modelSettingsValue(selection.Effort) {
		return patch, "请先选择模型和思考强度。"
	}
	model, found := findSettingsModel(models, selection.Model)
	if !found || model.Hidden || model.ID != selection.Model || !modelSettingsValue(model.Model) {
		return patch, "该模型已不可用，请重新发送 /model。"
	}
	patch.Model = &model.Model
	if selection.Effort == "keep" && current.ReasoningEffort == nil {
		if model.Model != current.Model {
			if !model.SupportsReasoningEffort(model.DefaultReasoningEffort) {
				return patch, "无法确定目标模型的默认思考强度，请明确选择。"
			}
			patch.ReasoningEffort = &model.DefaultReasoningEffort
		}
	} else {
		if !model.SupportsReasoningEffort(selection.Effort) {
			return patch, "所选模型不支持此思考强度，请选择该模型支持的强度。"
		}
		patch.ReasoningEffort = &selection.Effort
	}
	switch selection.Fast {
	case "off":
		tier := "default"
		patch.ServiceTier = &codexclient.ServiceTierChange{Value: &tier}
	case "on":
		tier := ""
		for _, candidate := range []string{"fast", "priority"} {
			if model.SupportsServiceTier(candidate) {
				tier = candidate
				break
			}
		}
		if tier == "" {
			return patch, "所选模型不支持 Fast，请关闭 Fast 后保存。"
		}
		patch.ServiceTier = &codexclient.ServiceTierChange{Value: &tier}
	case "keep":
		if modelSettingsDefaults(current, models).Fast != "keep" {
			return patch, "请选择开启或关闭 Fast。"
		}
		if current.ServiceTier != nil && !model.SupportsServiceTier(*current.ServiceTier) {
			return patch, "所选模型不支持当前服务档位，请明确选择 Fast 开关。"
		}
	default:
		return patch, "请选择开启或关闭 Fast。"
	}
	return patch, ""
}

func decodeModelSettingsForm(form store.SessionSettingsForm) (codexclient.ThreadSettings, []codexclient.Model, error) {
	var current codexclient.ThreadSettings
	var models []codexclient.Model
	if json.Unmarshal([]byte(form.Snapshot), &current) != nil || json.Unmarshal([]byte(form.Catalog), &models) != nil || current.ThreadID == "" || current.Model == "" {
		return current, nil, store.ErrSubmissionStale
	}
	return current, models, nil
}

// ModelSettingsAction does local work only. The thread actor performs the
// native read/write after the exact user's choice is durably claimed.
func (c Commands) ModelSettingsAction(ctx context.Context, scope store.SubmissionScope, user, chat, message, token string, revision int64, kind string, values map[string]any) (string, error) {
	if !c.Config.Operator(user) || scope.HostID != c.Config.Host.ID || scope.AppID != c.Config.Feishu.AppID {
		return "", store.ErrSubmissionStale
	}
	form, err := c.Store.SessionSettingsFormByToken(ctx, scope, token)
	if err != nil {
		return "", err
	}
	current, models, err := decodeModelSettingsForm(form)
	if err != nil || current.ThreadID != scope.ThreadID {
		return "", store.ErrSubmissionStale
	}
	action, selection, pending := "", "{}", ""
	switch kind {
	case cards.ModelSettingsSaveAction:
		action, pending = "save", "saving"
		model, _ := values[cards.ModelSettingsModelField].(string)
		effort, _ := values[cards.ModelSettingsEffortField].(string)
		fast, _ := values[cards.ModelSettingsFastField].(string)
		choice := modelSettingsSelection{Model: model, Effort: effort, Fast: fast}
		if _, reason := requestedModelSettings(choice, current, models); reason != "" {
			return "", &modelSettingsInputError{reason: reason}
		}
		raw, err := json.Marshal(choice)
		if err != nil {
			return "", err
		}
		selection = string(raw)
	case cards.ModelSettingsCancelAction:
		action, pending = "cancel", "canceling"
	default:
		return "", store.ErrSubmissionStale
	}
	card, err := cards.ModelSettingsCard(cards.ModelSettingsView{State: pending, Current: modelSettingsSummary(current)})
	if err != nil {
		return "", err
	}
	_, _, err = c.Store.ClaimSessionSettingsFormAction(ctx, scope, user, chat, message, token, revision, action, selection, card, c.now().UnixMilli())
	if err != nil {
		return "", err
	}
	if c.Wake != nil {
		c.Wake(scope.ThreadID)
	}
	return scope.ThreadID, nil
}

func (s SessionSettings) processForm(journalCtx, ctx context.Context, req store.SessionSettingsRequest, form store.SessionSettingsForm, blocked bool) (bool, error) {
	var current codexclient.ThreadSettings
	observed := false
	finish := func(nativeState, state, notice string) (bool, error) {
		next := form
		next.Revision++
		next.Phase = "closed"
		if nativeState == "unknown" {
			next.Phase = "saving"
		}
		summary := ""
		if observed {
			raw, err := json.Marshal(current)
			if err != nil {
				return false, err
			}
			next.Snapshot = string(raw)
			summary = modelSettingsSummary(current)
		}
		card, err := cards.ModelSettingsCard(cards.ModelSettingsView{State: state, Current: summary, Notice: notice})
		if err != nil {
			return false, err
		}
		err = s.Store.FinishSessionSettingsForm(journalCtx, req, form, next, nativeState, card, s.nowMS())
		return nativeState == "unknown", err
	}
	if req.State == "ready" {
		command, err := s.Store.CommandReply(journalCtx, req.HostID, req.AppID, req.MessageID)
		if err != nil {
			return false, err
		}
		if !commandContains(s.Operators, command.UserID) {
			if form.Phase == "canceling" {
				return finish("confirmed", "canceled", "未修改设置；当前值暂时无法读取。")
			}
			return finish("rejected", "failed", "你已不在操作者列表中，未修改设置。")
		}
		if s.Native == nil {
			if form.Phase == "canceling" {
				return finish("confirmed", "canceled", "未修改设置；当前值暂时无法读取。")
			}
			return finish("rejected", "failed", "会话设置暂不可用，请稍后重新发送 /model。")
		}
	}
	if s.Native == nil {
		return true, errSessionSettingsPending
	}
	if s.BeginWrite != nil {
		opctx, release, err := s.BeginWrite(ctx, req.ThreadID)
		if err != nil || opctx == nil || release == nil {
			if release != nil {
				release()
			}
			return true, errSessionSettingsPending
		}
		defer release()
		ctx = opctx
	}
	if _, err := s.Store.SubmissionBinding(ctx, store.SubmissionScope{HostID: req.HostID, AppID: req.AppID, ThreadID: req.ThreadID}); err != nil {
		return true, err
	}
	read := func() bool {
		current, observed = codexclient.ThreadSettings{}, false
		value, err := s.Native.ReadThreadSettings(ctx, req.ThreadID)
		if err != nil || value.ThreadID != req.ThreadID {
			return false
		}
		current, observed = value, true
		return true
	}
	if req.State != "ready" {
		if read() && settingsPayloadMatches(req.Payload, current) {
			return finish("confirmed", "saved", "")
		}
		return finish("unknown", "unknown", "保存结果尚未确认，正在核验。")
	}
	if form.Phase == "canceling" {
		if !read() {
			return finish("confirmed", "canceled", "未修改设置；当前值暂时无法读取。")
		}
		return finish("confirmed", "canceled", "")
	}
	if !read() {
		return finish("rejected", "failed", "当前设置读取失败，请重新发送 /model。")
	}
	if form.Phase == "saving" && blocked {
		return finish("rejected", "failed", "上一条设置变更仍在核验，请稍后重新发送 /model。")
	}
	models, err := s.Native.ListModels(ctx)
	if err != nil {
		return finish("rejected", "failed", "可用模型读取失败，请重新发送 /model。")
	}
	if form.Phase == "loading" {
		next := form
		next.Revision++
		next.Phase = "editing"
		selection := modelSettingsDefaults(current, models)
		snapshot, _ := json.Marshal(current)
		catalog, _ := json.Marshal(models)
		selected, _ := json.Marshal(selection)
		next.Snapshot, next.Catalog, next.Selection = string(snapshot), string(catalog), string(selected)
		view := modelSettingsView(current, models, selection, form.Token, next.Revision)
		if blocked {
			view.Notice = "上一条设置变更仍在核验，暂时无法保存新的修改。"
		}
		card, err := cards.ModelSettingsCard(view)
		if err != nil {
			return finish("rejected", "failed", "可用选项暂时无法完整显示，请稍后重新发送 /model。")
		}
		return false, s.Store.StageSessionSettingsForm(journalCtx, req, form, next, card)
	}
	if form.Phase != "saving" || form.Action != "save" {
		return false, store.ErrConflict
	}
	baseline, _, err := decodeModelSettingsForm(form)
	if err != nil || !sameModelSettings(baseline, current) {
		return finish("rejected", "failed", "会话设置已变化，未保存本次修改；请重新发送 /model。")
	}
	var selection modelSettingsSelection
	if json.Unmarshal([]byte(form.Selection), &selection) != nil {
		return finish("rejected", "failed", "设置选项无效，请重新发送 /model。")
	}
	patch, reason := requestedModelSettings(selection, current, models)
	if reason != "" {
		return finish("rejected", "failed", reason)
	}
	payload, err := codexclient.SettingsRequestJSON(req.ThreadID, patch)
	if err != nil {
		return finish("rejected", "failed", "设置选项无效，请重新发送 /model。")
	}
	if settingsPayloadMatches(payload, current) {
		return finish("confirmed", "saved", "")
	}
	claimed, err := s.Store.ClaimSessionSettings(journalCtx, req, payload)
	if err != nil || !claimed {
		return true, err
	}
	req.Payload = payload
	if err := ctx.Err(); err != nil {
		return finish("unknown", "unknown", "保存结果尚未确认，正在核验。")
	}
	writeErr := s.Native.UpdateThreadSettings(ctx, req.ThreadID, patch)
	if writeErr != nil {
		var rejected *codexclient.RPCError
		if errors.As(writeErr, &rejected) || errors.Is(writeErr, codexclient.ErrInvalidSettings) {
			return finish("rejected", "failed", "Codex 未接受此设置，请重新发送 /model。")
		}
		if read() && settingsPayloadMatches(payload, current) {
			return finish("confirmed", "saved", "")
		}
		return finish("unknown", "unknown", "保存结果尚未确认，正在核验。")
	}
	if !read() {
		return finish("confirmed", "saved", "当前值暂时无法读回，可重新发送 /model 查看。")
	}
	if !settingsPayloadMatches(payload, current) {
		return finish("confirmed", "saved", "请求已接受，当前值已发生变化，以此处显示为准。")
	}
	return finish("confirmed", "saved", "")
}
