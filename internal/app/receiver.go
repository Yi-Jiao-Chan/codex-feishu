package app

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// InboundCommand has an authenticated installed-application user identity.
// The command handler must still check its operator/recipient scope.
type InboundCommand struct {
	MessageID, ChatID, ChatType, UserID, Text string
	ThreadID                                  string // Empty for a private/unbound chat.
	Unsupported                               bool
}

// Receiver performs local durable work only. Slow/offline native and Feishu
// APIs cannot hold callbacks open; thread actors consume committed receipts.
// There is intentionally no Native field or client policy configuration.
type Receiver struct {
	Store         *store.Store
	Config        config.Config
	BotID         string
	Wake          func(string)
	Command       func(context.Context, InboundCommand) error
	TaskAction    func(context.Context, string, string, string, string, int64) (string, error)
	ActionAllowed func(string) bool
	Notice        func(string)
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

func (r *Receiver) Message(parent context.Context, e *im.P2MessageReceiveV1) error {
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
	if chatType == "group" {
		mentioned := false
		for _, mention := range msg.Mentions {
			if mention != nil && mention.Id != nil && ptr(mention.Id.OpenId) == r.BotID {
				mentioned = true
				break
			}
		}
		if !mentioned {
			return nil
		}
	}
	text, err := Text(msg, r.BotID)
	unsupported := err != nil
	if unsupported {
		r.notice("input.unsupported_content")
	}
	if text == "" && !unsupported {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	b, bindingErr := r.Store.BindingByChat(ctx, r.Config.Host.ID, ptr(msg.ChatId))
	bound := bindingErr == nil && chatType == "group"
	if bindingErr != nil && !errors.Is(bindingErr, sql.ErrNoRows) {
		return bindingErr
	}
	if unsupported || strings.HasPrefix(strings.TrimSpace(text), "/") || !bound {
		if r.Command != nil {
			command := InboundCommand{MessageID: ptr(msg.MessageId), ChatID: ptr(msg.ChatId), ChatType: chatType, UserID: user, Text: text, Unsupported: unsupported}
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
	queue, _ := a.Action.Value["queue_id"].(string)
	revision, validRevision := callbackRevision(a.Action.Value["revision"])
	if (kind == "stop" || kind == "continue") && token != "" && validRevision && r.TaskAction != nil {
		ctx, cancel := context.WithTimeout(parent, 2*time.Second)
		defer cancel()
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
		b, err := r.Store.BindingByChat(ctx, r.Config.Host.ID, a.Context.OpenChatID)
		if err != nil {
			return respond("info", "会话已失效"), nil
		}
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
	b, err := r.Store.BindingByChat(ctx, r.Config.Host.ID, a.Context.OpenChatID)
	if err != nil {
		return respond("info", "会话已失效"), nil
	}
	if r.ActionAllowed != nil && !r.ActionAllowed(b.ThreadID) {
		return respond("info", "连接暂不可用，请恢复同步后重试"), nil
	}
	n, err := r.Store.ByToken(ctx, token)
	if err != nil || n.AppID != r.Config.Feishu.AppID || n.ThreadID != b.ThreadID || n.ChatID != b.ChatID {
		return respond("info", "卡片已失效"), nil
	}
	m, err := r.Store.Mirror(ctx, r.Config.Host.ID, b.ThreadID, store.QueueCardKey(n.ID))
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
