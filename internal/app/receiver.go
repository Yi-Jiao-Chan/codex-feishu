package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/creation"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
	"codex-feishu/internal/verification"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// InboundCommand has an authenticated installed-application user identity.
// The command handler must still check its operator/recipient scope.
type InboundCommand struct {
	SourceRootID, SourceThreadID              string
	MessageID, ChatID, ChatType, UserID, Text string
	ThreadID                                  string // Empty for a private/unbound chat.
	Unsupported                               bool
	RouteFeedback                             string
}

// Receiver authorizes bound routes before local durable work. Callbacks are
// bounded; thread actors consume committed receipts after their claims.
// There is intentionally no Native field or client policy configuration.
type Receiver struct {
	Store              *store.Store
	Config             config.Config
	BotID              string
	Wake               func(string)
	Command            func(context.Context, InboundCommand) error
	Create             func(context.Context, creation.Inbound) (bool, error)
	CreationAction     func(context.Context, string, string, string, string, int64, string, map[string]any, map[string]any) error
	InteractionAction  func(context.Context, store.SubmissionScope, string, []string, string, string, string, int64, string, map[string]any) (string, error)
	TaskAction         func(context.Context, string, string, string, string, int64) (string, error)
	ActionAllowed      func(string) bool
	BeginBound         func(context.Context, store.Binding, string) (context.Context, func(), error)
	Notice             func(string)
	Verification       *verification.Manager
	VerificationGroups *verification.GroupManager
}

func (r *Receiver) notice(code string) {
	if r.Notice != nil {
		r.Notice(code)
	}
}
func (r *Receiver) wake(thread string) {
	if r.Wake != nil {
		r.Wake(thread)
	}
}

func (r *Receiver) beginBound(ctx context.Context, b store.Binding, user string) (context.Context, func(), error) {
	if r.BeginBound != nil {
		opctx, release, err := r.BeginBound(ctx, b, user)
		if err == nil && opctx != nil && release != nil && opctx.Err() == nil {
			return opctx, release, nil
		}
		if release != nil {
			release()
		}
		return nil, nil, store.ErrIneligible
	}
	_, err := r.Store.VerificationBindingForThread(ctx, r.Config.Host.ID, b.ThreadID)
	if errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
		return ctx, func() {}, nil
	}
	return nil, nil, store.ErrIneligible
}

func (r *Receiver) actionBinding(ctx context.Context, chat, message string) (store.Binding, error) {
	if b, err := r.Store.BindingByChat(ctx, r.Config.Host.ID, chat); err == nil {
		return b, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return b, err
	}
	return r.Store.BindingByMessage(ctx, r.Config.Host.ID, chat, message)
}

func (r *Receiver) Message(parent context.Context, e *im.P2MessageReceiveV1) error {
	ingressStarted := time.Now().UnixNano()
	if r.Store == nil || r.BotID == "" || e == nil || e.Event == nil || e.Event.Message == nil || e.Event.Sender == nil {
		return nil
	}
	msg, sender := e.Event.Message, e.Event.Sender
	if ptr(sender.SenderType) != "user" || sender.SenderId == nil || ptr(msg.MessageId) == "" || ptr(msg.ChatId) == "" {
		return nil
	}
	chatType, user := ptr(msg.ChatType), ptr(sender.SenderId.OpenId)
	if user == "" || chatType != "group" && chatType != "p2p" {
		return nil
	}
	standalone := r.Config.Attachments.Enabled && r.Config.Attachments.ReceiveStandalone && (ptr(msg.MessageType) == "image" || ptr(msg.MessageType) == "file")
	if chatType == "group" {
		mentioned := false
		for _, mention := range msg.Mentions {
			if mention != nil && mention.Id != nil && ptr(mention.Id.OpenId) == r.BotID {
				mentioned = true
				break
			}
		}
		if !mentioned && !standalone {
			return nil
		}
	}
	text, err := Text(msg, r.BotID)
	var content feishu.InboundMessage
	if r.Config.Attachments.Enabled {
		content, err = feishu.ParseInbound(msg, r.BotID)
		text = strings.TrimSpace(content.Text)
	}
	unsupported := err != nil
	if unsupported {
		r.notice("input.unsupported_content")
	}
	hasResources := false
	for _, part := range content.Parts {
		hasResources = hasResources || part.Type == "image" || part.Type == "file"
	}
	if text == "" && !unsupported && !hasResources {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	if r.VerificationGroups != nil && chatType == "group" && !unsupported && strings.HasPrefix(strings.TrimSpace(text), "/verify-group ") {
		_, err := r.VerificationGroups.Message(ctx, user, strings.TrimPrefix(strings.TrimSpace(text), "/verify-group "), ptr(msg.MessageId), ptr(msg.ChatId))
		return err
	}
	if r.Verification != nil && chatType == "p2p" && !unsupported && strings.HasPrefix(strings.TrimSpace(text), "/verify ") {
		_, err := r.Verification.PrivateMessage(ctx, user, strings.TrimPrefix(strings.TrimSpace(text), "/verify "), ptr(msg.MessageId))
		return err
	}
	b, bindingErr := r.Store.BindingByTarget(ctx, r.Config.Host.ID, ptr(msg.ChatId), ptr(msg.RootId), ptr(msg.ThreadId))
	bound := bindingErr == nil && chatType == "group"
	sourceRoot, sourceThread := "", ""
	if b.FollowMode == "topic" || ptr(msg.ThreadId) != "" {
		sourceRoot, sourceThread = ptr(msg.RootId), ptr(msg.ThreadId)
		if sourceRoot == "" && bound {
			sourceRoot = b.RootMessageID
		}
	}
	if bindingErr != nil && !errors.Is(bindingErr, sql.ErrNoRows) {
		return bindingErr
	}
	if standalone && (!bound || !r.Config.Operator(user)) {
		return nil
	}
	if bound {
		if b.RepairRequired {
			return store.ErrTargetRepair
		}
		if b.FollowMode == "topic" && b.PlatformThreadID == "" && ptr(msg.ThreadId) != "" {
			if err := r.Store.RecordBindingPlatformThread(ctx, b.HostID, b.ThreadID, ptr(msg.ThreadId)); err != nil {
				return err
			}
			b.PlatformThreadID = ptr(msg.ThreadId)
		}
		opctx, release, err := r.beginBound(ctx, b, user)
		if err != nil {
			return err
		}
		defer release()
		ctx = opctx
	}
	var topics int
	if !bound && chatType == "group" {
		if err := r.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM bindings WHERE host_id=? AND chat_id=? AND follow_mode='topic'`, r.Config.Host.ID, ptr(msg.ChatId)).Scan(&topics); err != nil {
			return err
		}
	}
	fields := strings.Fields(text)
	explicitNew := len(fields) > 0 && fields[0] == "/new"
	if !unsupported && !hasResources && r.Create != nil && (topics == 0 || explicitNew) {
		handled, err := r.Create(ctx, creation.Inbound{MessageID: ptr(msg.MessageId), ChatID: ptr(msg.ChatId), ChatType: chatType, UserID: user, Text: text, RootID: ptr(msg.RootId), PlatformThreadID: ptr(msg.ThreadId), Bound: bound, Topic: b.FollowMode == "topic" || !bound && ptr(msg.ThreadId) != ""})
		if errors.Is(err, creation.ErrTaskTooLong) {
			if r.Command == nil {
				return nil
			}
			feedback := InboundCommand{MessageID: ptr(msg.MessageId), ChatID: ptr(msg.ChatId), ChatType: chatType, UserID: user, Text: "/help", SourceRootID: sourceRoot, SourceThreadID: sourceThread, RouteFeedback: fmt.Sprintf("任务正文最多支持 %d 字，请精简后重新发送 /new。", creation.MaxTaskRunes)}
			if bound {
				feedback.ThreadID = b.ThreadID
			}
			return r.Command(ctx, feedback)
		}
		if handled || err != nil {
			return err
		}
	}
	if !bound && chatType == "group" && !strings.HasPrefix(strings.TrimSpace(text), "/") {
		if topics > 0 {
			// A workgroup's main stream never selects its most recent topic.
			if r.Command != nil {
				return r.Command(ctx, InboundCommand{MessageID: ptr(msg.MessageId), ChatID: ptr(msg.ChatId), ChatType: chatType, UserID: user, Text: "/help", RouteFeedback: "请进入对应任务话题 @机器人，或使用 /new 新建任务。", SourceRootID: sourceRoot, SourceThreadID: sourceThread})
			}
			return nil
		}
	}
	if unsupported || strings.HasPrefix(strings.TrimSpace(text), "/") || !bound {
		if r.Command != nil {
			command := InboundCommand{MessageID: ptr(msg.MessageId), ChatID: ptr(msg.ChatId), ChatType: chatType, UserID: user, Text: text, Unsupported: unsupported, SourceRootID: sourceRoot, SourceThreadID: sourceThread}
			if bound {
				command.ThreadID = b.ThreadID
			}
			return r.Command(ctx, command)
		}
		return nil
	}
	if !r.Config.Operator(user) {
		return nil
	}
	scope := store.SubmissionScope{HostID: r.Config.Host.ID, AppID: r.Config.Feishu.AppID, ThreadID: b.ThreadID}
	if _, err := r.Store.SubmissionBinding(ctx, scope); err != nil {
		return err
	}
	if r.Config.Attachments.Enabled {
		in := store.AttachmentInput{Scope: scope, ChatID: b.ChatID, SenderID: user, MessageID: ptr(msg.MessageId), Body: text, IngressStartedNS: ingressStarted}
		for _, part := range content.Parts {
			in.Parts = append(in.Parts, store.AttachmentPart{Kind: part.Type, Text: part.Text, ResourceKey: part.ResourceKey, Name: part.Name})
		}
		if draft, err := r.Store.OpenAttachmentBatch(ctx, scope, user); err == nil && text != "" {
			in.ExpectedBatch = draft.ID
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		limits := r.Config.AttachmentLimits()
		_, _, err := r.Store.ReceiveAttachmentInput(ctx, in, limits.MaxResources, limits.MaxImages)
		if errors.Is(err, store.ErrAttachmentLimit) || errors.Is(err, store.ErrSubmissionStale) {
			// Admission committed a terminal rejection and its feedback together.
			r.wake(b.ThreadID)
			return nil
		}
		if err == nil {
			r.wake(b.ThreadID)
		}
		return err
	}
	_, _, err = r.Store.Claim(ctx, store.Receipt{AppID: scope.AppID, MessageID: ptr(msg.MessageId), ThreadID: b.ThreadID, ChatID: b.ChatID, Body: text})
	if err == nil {
		// Replays wake recovery after a receipt committed just before disconnect.
		r.wake(b.ThreadID)
	}
	return err
}

func (r *Receiver) Action(parent context.Context, e *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	respond := func(kind, text string) *callback.CardActionTriggerResponse {
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: kind, Content: text}}
	}
	if r.Store == nil || e == nil || e.Event == nil || e.Event.Operator == nil || e.Event.Context == nil || e.Event.Action == nil {
		return respond("error", "无效操作"), nil
	}
	a := e.Event
	if !r.Config.Operator(a.Operator.OpenID) || a.Context.OpenChatID == "" || a.Context.OpenMessageID == "" {
		return respond("error", "无执行权限"), nil
	}
	token, _ := a.Action.Value["token"].(string)
	kind, _ := a.Action.Value["action"].(string)
	if strings.HasPrefix(kind, "creation_") && r.Config.Creation.Enabled && r.CreationAction != nil {
		revision, valid := callbackRevision(a.Action.Value["revision"])
		if !valid || token == "" {
			return respond("info", "创建卡已更新，请使用最新卡片"), nil
		}
		ctx, cancel := context.WithTimeout(parent, 2*time.Second)
		defer cancel()
		// A standalone select reports its choice in action.option, separately
		// from the server callback value. Copy rather than mutate the event.
		value := make(map[string]any, len(a.Action.Value)+1)
		for key, field := range a.Action.Value {
			if key != "option" {
				value[key] = field
			}
		}
		if a.Action.Tag == "select_static" && a.Action.Option != "" {
			value["option"] = a.Action.Option
		}
		err := r.CreationAction(ctx, a.Operator.OpenID, a.Context.OpenChatID, a.Context.OpenMessageID, token, revision, kind, value, a.Action.FormValue)
		if errors.Is(err, store.ErrSubmissionStale) || errors.Is(err, store.ErrConflict) {
			return respond("info", "创建卡已更新或失效，请查看当前进度"), nil
		}
		if err != nil {
			return respond("error", "保存创建操作失败，请重试"), err
		}
		message := "已收到，正在处理"
		switch kind {
		case "creation_browse", "creation_goto", "creation_panel":
			message = "正在打开目录…"
		case "creation_lookup", "creation_filter", "creation_search", "creation_search_next":
			message = "正在查找目录…"
		case "creation_pick", "creation_path", "creation_search_pick":
			message = "正在保存所选目录…"
		case "creation_projects":
			message = "正在加载最近目录与项目…"
		}
		return respond("info", message), nil
	}
	if strings.HasPrefix(kind, "attachment_") && r.Config.Attachments.Enabled {
		revision, valid := callbackRevision(a.Action.Value["revision"])
		if !valid || token == "" {
			return respond("info", "附件卡已更新，请查看最新卡片"), nil
		}
		ctx, cancel := context.WithTimeout(parent, 2*time.Second)
		defer cancel()
		b, err := r.actionBinding(ctx, a.Context.OpenChatID, a.Context.OpenMessageID)
		if err != nil {
			return respond("info", "会话已失效"), nil
		}
		opctx, release, err := r.beginBound(ctx, b, a.Operator.OpenID)
		if err != nil {
			return respond("info", "会话暂不可用"), nil
		}
		defer release()
		notes, _ := a.Action.FormValue["attachment_notes"].(string)
		part, _ := a.Action.Value["part_id"].(string)
		scope := store.SubmissionScope{HostID: r.Config.Host.ID, AppID: r.Config.Feishu.AppID, ThreadID: b.ThreadID}
		_, err = r.Store.AttachmentAction(opctx, scope, a.Operator.OpenID, b.ChatID, a.Context.OpenMessageID, token, revision, kind, part, notes)
		if errors.Is(err, store.ErrSubmissionStale) || errors.Is(err, store.ErrSubmissionBusy) || errors.Is(err, store.ErrConflict) {
			return respond("info", "本批已发送或状态已更新，请查看最新卡片"), nil
		}
		if err != nil {
			return respond("error", "保存附件操作失败，请重试"), err
		}
		r.wake(b.ThreadID)
		return respond("info", "已收到附件操作"), nil
	}
	if kind == "verification_confirm" && r.Verification != nil {
		ctx, cancel := context.WithTimeout(parent, 2*time.Second)
		defer cancel()
		ok, err := r.Verification.Callback(ctx, a.Operator.OpenID, token, a.Context.OpenMessageID, a.Context.OpenChatID)
		if err != nil || !ok {
			return respond("error", "验证已过期或无法确认，请回到配置终端查看状态"), nil
		}
		return respond("success", "已记录卡片回调，请继续发送验证命令"), nil
	}

	if kind == "interaction_submit" && r.InteractionAction != nil && r.Config.Interactions.Enabled {
		revision, valid := callbackRevision(a.Action.Value["revision"])
		if token == "" || !valid {
			return respond("info", "卡片已更新，请使用最新操作"), nil
		}
		ctx, cancel := context.WithTimeout(parent, 2*time.Second)
		defer cancel()
		b, err := r.actionBinding(ctx, a.Context.OpenChatID, a.Context.OpenMessageID)
		if err != nil || r.ActionAllowed != nil && !r.ActionAllowed(b.ThreadID) {
			return respond("info", "连接暂不可用，请恢复同步后重试"), nil
		}
		opctx, release, err := r.beginBound(ctx, b, a.Operator.OpenID)
		if err != nil {
			return respond("info", "会话暂不可用"), nil
		}
		defer release()
		decision, _ := a.Action.Value["decision"].(string)
		scope := store.SubmissionScope{HostID: r.Config.Host.ID, AppID: r.Config.Feishu.AppID, ThreadID: b.ThreadID}
		thread, err := r.InteractionAction(opctx, scope, a.Operator.OpenID, r.Config.Access.OperatorOpenIDs, a.Context.OpenChatID, a.Context.OpenMessageID, token, revision, decision, a.Action.FormValue)
		if err != nil {
			if errors.Is(err, codexclient.ErrInvalidAnswer) {
				return respond("info", "请先选择授权范围，或填写完整回答"), nil
			}
			if errors.Is(err, store.ErrSubmissionBusy) {
				return respond("info", "这份操作已收到，请勿重复提交"), nil
			}
			if errors.Is(err, store.ErrSubmissionStale) || errors.Is(err, store.ErrConflict) || errors.Is(err, sql.ErrNoRows) {
				return respond("info", "请求已更新，请查看最新卡片"), nil
			}
			return respond("error", "保存操作失败，请稍后重试"), err
		}
		r.wake(thread)
		return respond("info", "已收到，正在提交"), nil
	}
	queue, _ := a.Action.Value["queue_id"].(string)
	revision, validRevision := callbackRevision(a.Action.Value["revision"])
	if (kind == "stop" || kind == "continue") && token != "" && validRevision && r.TaskAction != nil {
		ctx, cancel := context.WithTimeout(parent, 2*time.Second)
		defer cancel()
		b, err := r.actionBinding(ctx, a.Context.OpenChatID, a.Context.OpenMessageID)
		if err != nil {
			return respond("info", "会话已失效"), nil
		}
		opctx, release, err := r.beginBound(ctx, b, a.Operator.OpenID)
		if err != nil {
			return respond("info", "会话暂不可用，请恢复同步后重试"), nil
		}
		defer release()
		ctx = opctx
		thread, err := r.TaskAction(ctx, a.Context.OpenChatID, a.Context.OpenMessageID, kind, token, revision)
		if err != nil {
			if errors.Is(err, store.ErrSubmissionStale) || errors.Is(err, store.ErrSubmissionBusy) || errors.Is(err, store.ErrConflict) {
				return respond("info", "状态已更新，请查看最新卡片"), nil
			}
			return respond("error", "保存操作失败，请稍后重试"), err
		}
		r.wake(thread)
		return respond("info", "已收到，正在处理"), nil
	}
	if kind == "cancel_pending" && token != "" && queue == "" && validRevision {
		ctx, cancel := context.WithTimeout(parent, 2*time.Second)
		defer cancel()
		b, err := r.actionBinding(ctx, a.Context.OpenChatID, a.Context.OpenMessageID)
		if err != nil {
			return respond("info", "会话已失效"), nil
		}
		opctx, release, err := r.beginBound(ctx, b, a.Operator.OpenID)
		if err != nil {
			return respond("info", "会话暂不可用，请恢复同步后重试"), nil
		}
		defer release()
		ctx = opctx
		scope := store.SubmissionScope{HostID: r.Config.Host.ID, AppID: r.Config.Feishu.AppID, ThreadID: b.ThreadID}
		_, err = r.Store.CancelPendingSubmission(ctx, scope, a.Operator.OpenID, r.Config.Access.OperatorOpenIDs, a.Context.OpenChatID, a.Context.OpenMessageID, token, revision)
		if err != nil {
			if errors.Is(err, store.ErrSubmissionStale) || errors.Is(err, store.ErrConflict) {
				return respond("info", "状态已更新，请查看最新卡片"), nil
			}
			return respond("error", "保存操作失败，请稍后重试"), err
		}
		r.wake(b.ThreadID)
		return respond("info", "已取消本地待提交消息"), nil
	}
	if token == "" || queue == "" || !validRevision || kind != "steer" && kind != "remove" {
		return respond("info", "卡片已更新，请使用最新操作"), nil
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	b, err := r.actionBinding(ctx, a.Context.OpenChatID, a.Context.OpenMessageID)
	if err != nil {
		return respond("info", "会话已失效"), nil
	}
	opctx, release, err := r.beginBound(ctx, b, a.Operator.OpenID)
	if err != nil {
		return respond("info", "会话暂不可用，请恢复同步后重试"), nil
	}
	defer release()
	ctx = opctx
	if r.ActionAllowed != nil && !r.ActionAllowed(b.ThreadID) {
		return respond("info", "连接暂不可用，请恢复同步后重试"), nil
	}
	n, err := r.Store.ByToken(ctx, token)
	if err != nil || n.AppID != r.Config.Feishu.AppID || n.ThreadID != b.ThreadID || n.ChatID != b.ChatID {
		return respond("info", "卡片已失效"), nil
	}
	scope := store.SubmissionScope{HostID: r.Config.Host.ID, AppID: r.Config.Feishu.AppID, ThreadID: b.ThreadID}
	m, err := r.Store.SubmissionCard(ctx, scope, n.ID)
	if err != nil || m.RemoteID == "" || m.RemoteID != a.Context.OpenMessageID {
		return respond("info", "卡片已失效"), nil
	}
	c := submissions.Controller{Store: r.Store, HostID: r.Config.Host.ID, AppID: r.Config.Feishu.AppID, ThreadID: b.ThreadID}
	result, err := c.ClaimAction(ctx, n.ID, kind, queue, revision)
	if err != nil {
		if errors.Is(err, store.ErrSubmissionStale) || errors.Is(err, store.ErrSubmissionBusy) || errors.Is(err, store.ErrConflict) {
			return respond("info", "状态已更新，请查看最新卡片"), nil
		}
		return respond("error", "保存操作失败，请稍后重试"), err
	}
	r.wake(b.ThreadID)
	if result.State == "confirmed" || result.State == "rejected" {
		return respond("info", "操作已处理，请查看最新状态"), nil
	}
	return respond("info", "已收到，正在处理"), nil
}

func callbackRevision(v any) (int64, bool) {
	// Official SDK JSON values are float64; never round an inexact revision.
	switch n := v.(type) {
	case float64:
		if n > 0 && n <= 1<<53-1 && math.Trunc(n) == n {
			return int64(n), true
		}
	case int64:
		return n, n > 0 && n <= 1<<53-1
	case int:
		return int64(n), n > 0 && int64(n) <= 1<<53-1
	}
	return 0, false
}
