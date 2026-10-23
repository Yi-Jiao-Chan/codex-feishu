package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type sessionSettingsNative interface {
	ReadThreadSettings(context.Context, string) (codexclient.ThreadSettings, error)
	ListModels(context.Context) ([]codexclient.Model, error)
	UpdateThreadSettings(context.Context, string, codexclient.SettingsPatch) error
}

var errSessionSettingsPending = errors.New("session settings await verification")

// SessionSettings runs inside the existing thread actor, before ordinary input
// dispatch. Only explicit commands create requests; reads never override state.
type SessionSettings struct {
	Store         *store.Store
	Native        sessionSettingsNative
	HostID, AppID string
	Operators     []string
	BeginWrite    func(context.Context, string) (context.Context, func(), error)
	Now           func() time.Time
}

func (s SessionSettings) nowMS() int64 {
	if s.Now != nil {
		return s.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

func (s SessionSettings) Tick(ctx context.Context, thread string) error {
	// Reserve the actor's remaining time for stop and ordinary input recovery.
	// The parent stays live for journaling even if a native settings RPC times out.
	nativeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	requests, err := s.Store.SessionSettingsRequests(ctx, store.SubmissionScope{HostID: s.HostID, AppID: s.AppID, ThreadID: thread})
	if err != nil {
		return err
	}
	pending := false
	for _, req := range requests {
		if nativeCtx.Err() != nil {
			return errSessionSettingsPending
		}
		if req.State != "ready" && req.NextCheckAtMS > s.nowMS() {
			pending = true
			continue
		}
		unresolved, err := s.process(ctx, nativeCtx, req, pending)
		if err != nil {
			return err
		}
		pending = pending || unresolved
	}
	if pending {
		return errSessionSettingsPending
	}
	return nil
}

func settingsCard(conclusion, detail string, current codexclient.ThreadSettings) string {
	return cards.Settings(cards.SettingsView{
		Conclusion: conclusion, DetailsTitle: "可用设置与命令", Details: detail,
		Source: cards.ModelSettingsText(current.Model, current.ReasoningEffort, current.ServiceTier, current.ServiceTierKnown),
	})
}

func (s SessionSettings) process(journalCtx, ctx context.Context, req store.SessionSettingsRequest, blocked bool) (bool, error) {
	form, err := s.Store.SessionSettingsForm(journalCtx, req.ID)
	if err == nil {
		return s.processForm(journalCtx, ctx, req, form, blocked)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return s.processLegacy(journalCtx, ctx, req, blocked)
}

// processLegacy reconciles requests journaled by the earlier command interface.
// New /model interactions use the persistent form and mirror lifecycle.
func (s SessionSettings) processLegacy(journalCtx, ctx context.Context, req store.SessionSettingsRequest, blocked bool) (bool, error) {
	current := codexclient.ThreadSettings{}
	finish := func(state, conclusion, detail string) (bool, error) {
		err := s.Store.FinishSessionSettings(journalCtx, req, state, settingsCard(conclusion, detail, current), s.nowMS())
		return state == "unknown", err
	}
	if req.State == "ready" {
		command, err := s.Store.CommandReply(ctx, req.HostID, req.AppID, req.MessageID)
		if err != nil {
			return false, err
		}
		if !commandContains(s.Operators, command.UserID) {
			return finish("rejected", "你已不在本部署的操作者列表中，未修改设置。", "")
		}
		if blocked && req.Value != "" {
			return finish("rejected", "上一条设置变更仍在核验，未发送新的修改。", "可使用 /model 查看当前设置；核验完成后在卡片中修改。")
		}
		if s.Native == nil {
			return finish("rejected", "当前连接暂不支持会话设置，请检查 Codex 版本。", "")
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
		current = codexclient.ThreadSettings{}
		observed, err := s.Native.ReadThreadSettings(ctx, req.ThreadID)
		if err != nil || observed.ThreadID != req.ThreadID {
			return false
		}
		current = observed
		return true
	}
	if req.State != "ready" {
		if read() && settingsPayloadMatches(req.Payload, current) {
			return finish("confirmed", "已核验：当前会话设置与请求一致，将用于后续轮次。", "")
		}
		return finish("unknown", "设置结果尚未确认，正在核验；不会重复发送修改。", "可使用 /model 查看当前设置。当前正在执行的轮次保持原设置。")
	}
	if !read() {
		return finish("rejected", "当前会话设置读取失败，未修改设置。请稍后重试。", "")
	}
	models, catalogErr := s.Native.ListModels(ctx)
	if req.Value == "" {
		conclusion := "当前会话设置；修改用于后续轮次，正在执行的轮次保持原设置。"
		if blocked {
			conclusion += "\n之前的设置请求仍在核验。"
		}
		return finish("confirmed", conclusion, modelSettingsHelp(current, models, catalogErr))
	}
	if catalogErr != nil && !(req.Setting == "fast" && req.Value == "off") {
		return finish("rejected", "可用模型列表读取失败，未修改设置。请稍后重试。", "")
	}
	patch, reason := requestedSettings(req, current, models)
	if reason != "" {
		return finish("rejected", reason, modelSettingsHelp(current, models, catalogErr))
	}
	payload, err := codexclient.SettingsRequestJSON(req.ThreadID, patch)
	if err != nil {
		return finish("rejected", "设置参数无效，未修改。", "")
	}
	if settingsPayloadMatches(payload, current) {
		return finish("confirmed", "当前会话已使用这些设置。", "")
	}
	claimed, err := s.Store.ClaimSessionSettings(ctx, req, payload)
	if err != nil {
		return false, err
	}
	if !claimed {
		return true, errSessionSettingsPending
	}
	req.Payload = payload
	writeErr := s.Native.UpdateThreadSettings(ctx, req.ThreadID, patch)
	if writeErr != nil {
		var rejected *codexclient.RPCError
		if errors.As(writeErr, &rejected) || errors.Is(writeErr, codexclient.ErrInvalidSettings) {
			return finish("rejected", "Codex 未接受此设置，未自动重试。请用 /model 检查可用选项。", "")
		}
		if read() && settingsPayloadMatches(payload, current) {
			return finish("confirmed", "已核验：当前会话设置与请求一致，将用于后续轮次。", "")
		}
		return finish("unknown", "设置结果尚未确认，正在核验；不会重复发送修改。", "可使用 /model 查看当前设置。当前正在执行的轮次保持原设置。")
	}
	current = codexclient.ThreadSettings{}
	if !read() {
		return finish("confirmed", "设置请求已接受，将用于后续轮次；当前设置暂时无法读回。", "可使用 /model 重新查看。")
	}
	if !settingsPayloadMatches(payload, current) {
		return finish("confirmed", "设置请求已接受，但当前读回值已发生变化，请以下方会话设置为准。", "")
	}
	return finish("confirmed", "会话设置已更新，将用于后续轮次；正在执行的轮次保持原设置。", "")
}

func findSettingsModel(models []codexclient.Model, id string) (codexclient.Model, bool) {
	for _, model := range models {
		if model.ID == id {
			return model, true
		}
	}
	var found codexclient.Model
	count := 0
	for _, model := range models {
		if model.Model == id {
			found, count = model, count+1
		}
	}
	return found, count == 1
}

func requestedSettings(req store.SessionSettingsRequest, current codexclient.ThreadSettings, models []codexclient.Model) (codexclient.SettingsPatch, string) {
	var patch codexclient.SettingsPatch
	value := req.Value
	if req.Setting == "fast" && value == "off" {
		tier := "default"
		patch.ServiceTier = &codexclient.ServiceTierChange{Value: &tier}
		return patch, ""
	}
	id := current.Model
	fields := strings.Fields(value)
	if req.Setting == "model" && len(fields) > 0 {
		id = fields[0]
	}
	model, found := findSettingsModel(models, id)
	if !found {
		return patch, "找不到该模型的可用选项，请使用 /model 查看模型列表。"
	}
	switch req.Setting {
	case "model":
		if len(fields) < 1 || len(fields) > 2 {
			return patch, "请发送 /model，在设置卡片中选择模型和思考强度。"
		}
		patch.Model = &model.Model
		if len(fields) == 2 {
			if !model.SupportsReasoningEffort(fields[1]) {
				return patch, "该模型不支持此思考强度，未修改设置。"
			}
			patch.ReasoningEffort = &fields[1]
		} else if current.ReasoningEffort == nil || !model.SupportsReasoningEffort(*current.ReasoningEffort) {
			if !model.SupportsReasoningEffort(model.DefaultReasoningEffort) {
				return patch, "无法确定目标模型的默认思考强度，请明确指定。"
			}
			patch.ReasoningEffort = &model.DefaultReasoningEffort
		}
		if current.ServiceTier != nil && *current.ServiceTier != "default" && !model.SupportsServiceTier(*current.ServiceTier) {
			return patch, "目标模型未提供当前服务档位。请发送 /model，在设置卡片中关闭 Fast 并选择目标模型。"
		}
	case "reasoning":
		if !model.SupportsReasoningEffort(value) {
			return patch, "当前模型不支持此思考强度，未修改设置。"
		}
		patch.ReasoningEffort = &value
	case "fast":
		tier := ""
		for _, candidate := range []string{"fast", "priority"} {
			if model.SupportsServiceTier(candidate) {
				tier = candidate
				break
			}
		}
		if value != "on" || tier == "" {
			return patch, "当前模型未提供可用的 Fast 模式，未修改设置。"
		}
		patch.ServiceTier = &codexclient.ServiceTierChange{Value: &tier}
	default:
		return patch, "不支持此会话设置。"
	}
	return patch, ""
}

func settingsPayloadMatches(payload string, current codexclient.ThreadSettings) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &fields) != nil || len(fields) < 2 {
		return false
	}
	var thread string
	if json.Unmarshal(fields["threadId"], &thread) != nil || current.ThreadID != thread {
		return false
	}
	for name, raw := range fields {
		if name == "threadId" {
			continue
		}
		var want *string
		if json.Unmarshal(raw, &want) != nil {
			return false
		}
		var actual *string
		switch name {
		case "model":
			actual = &current.Model
		case "effort":
			actual = current.ReasoningEffort
		case "serviceTier":
			if !current.ServiceTierKnown {
				return false
			}
			actual = current.ServiceTier
		default:
			return false
		}
		if want == nil && actual != nil || want != nil && (actual == nil || *want != *actual) {
			return false
		}
	}
	return true
}

func modelSettingsHelp(current codexclient.ThreadSettings, models []codexclient.Model, catalogErr error) string {
	text := "发送 /model，在卡片中选择模型、思考强度和 Fast，点击保存或取消；只影响当前会话的后续轮次。"
	if catalogErr != nil {
		return text + "\n可用模型列表暂时无法读取。"
	}
	text += "\n\n可用模型与思考强度："
	for i, model := range models {
		if i >= 32 || len(text) > 10000 {
			text += "\n列表过长，仅显示部分模型。"
			break
		}
		var efforts []string
		for _, option := range model.SupportedReasoningEfforts {
			if len(option.ReasoningEffort) <= 128 {
				efforts = append(efforts, option.ReasoningEffort)
			}
		}
		if len(model.ID) > 128 {
			continue
		}
		text += "\n" + model.ID + "：" + strings.Join(efforts, " / ")
		if model.SupportsServiceTier("fast") || model.SupportsServiceTier("priority") {
			text += "；支持 Fast"
		}
		if model.Model == current.Model {
			text += "（当前）"
		}
	}
	return text
}
