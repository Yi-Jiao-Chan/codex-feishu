package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

// Commands performs local work only in Handle. DispatchOne consumes its durable
// reply outbox; native task RPCs remain exclusively in the submission actor.
type Commands struct {
	Store  *store.Store
	Config config.Config
	Wake   func(string)
	Now    func() time.Time
}

func (c Commands) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func commandContains(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func (c Commands) Handle(ctx context.Context, in InboundCommand) error {
	if c.Store == nil {
		return errors.New("commands require durable storage")
	}
	operator := c.Config.Operator(in.UserID)
	recipient := commandContains(c.Config.Notifications.TargetOpenIDs, in.UserID)
	if !operator && !recipient {
		return nil
	} // Strangers receive no spam response.
	saved, err := c.Store.CommandReply(ctx, c.Config.Host.ID, c.Config.Feishu.AppID, in.MessageID)
	if err == nil {
		if saved.UserID != in.UserID || saved.ChatID != in.ChatID || saved.ChatType != in.ChatType {
			return store.ErrConflict
		}
		if saved.ThreadID != "" && c.Wake != nil {
			c.Wake(saved.ThreadID)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := c.now()
	req := store.CommandRequest{HostID: c.Config.Host.ID, AppID: c.Config.Feishu.AppID, MessageID: in.MessageID, ChatID: in.ChatID, ChatType: in.ChatType, UserID: in.UserID, ThreadID: in.ThreadID, Operators: c.Config.Access.OperatorOpenIDs, NotificationTargets: c.Config.Notifications.TargetOpenIDs, AllowedRoots: c.Config.Discovery.AllowedRoots}
	fields := strings.Fields(in.Text)
	command := ""
	if len(fields) > 0 {
		command = fields[0]
	}
	feedback := func(text string) { req.Kind, req.Feedback = "feedback", text }
	switch command {
	case "/mute-dir", "/unmute-dir":
		if !operator {
			feedback("目录设置仅限操作者修改。")
			break
		}
		parsed, err := rules.ParseDirectoryCommand(in.Text)
		if err != nil {
			feedback("目录命令无效。请使用 /mute-dir /绝对目录 [--recursive]、/unmute-dir /绝对目录 [--recursive] 或 /mute-dir list；目录须存在。")
			break
		}
		req.Kind, req.Directory = "directory", parsed
		if parsed.Action != rules.DirectoryList {
			for _, configured := range c.Config.Discovery.AllowedRoots {
				root, err := rules.CanonicalDirectory(configured)
				if err != nil {
					feedback("已授权目录当前不可用，未修改规则。")
					break
				}
				req.ScopeRoots = append(req.ScopeRoots, root)
			}
			if req.Kind == "feedback" {
				break
			}
			scope, err := rules.ResolveScope(ctx, parsed.Rule.Path, req.ScopeRoots)
			if err != nil || !scope.Allowed {
				feedback("该目录不可用或不在已授权项目范围内，未修改规则。")
				break
			}
			req.DirectoryScope = scope
		}
	case "/notify":
		if !recipient {
			feedback("你不在本部署的通知接收者列表中，不能修改通知设置。")
			break
		}
		p, err := c.Store.NotificationPolicy(ctx, req.AppID, in.UserID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				feedback("通知策略尚未初始化，请联系操作者。")
				break
			}
			return err
		}
		parsed, err := rules.ParseNotificationCommand(in.Text, now, p.Timezone)
		if err != nil {
			feedback("通知命令无效。支持 /notify at off、on、snooze 2h、until YYYY-MM-DD HH:mm、quiet HH:mm-HH:mm、quiet off、status；只修改你自己的设置。")
			break
		}
		req.Kind, req.Notification, req.NotificationTimezone = "notify", parsed, p.Timezone
	case "/stop", "/continue":
		if !operator {
			feedback("任务控制仅限操作者使用。")
			break
		}
		if len(fields) != 1 {
			feedback("任务控制命令不接受参数，请使用 /stop 或 /continue。")
			break
		}
		if in.ThreadID == "" || in.ChatType != "group" {
			feedback("请在 Codex App 发起任务，并在对应的已绑定任务群中使用 /stop 或 /continue；这里不创建新任务。")
			break
		}
		req.Kind = strings.TrimPrefix(command, "/")
	case "/help", "/status":
		if len(fields) != 1 {
			feedback("/help 和 /status 不接受参数。")
		} else {
			req.Kind = strings.TrimPrefix(command, "/")
		}
	default:
		req.Kind = "help"
	}
	if in.Unsupported {
		feedback("暂时仅支持文字和纯文字富文本，请将内容以文字发送。")
	}
	r, _, err := c.Store.ApplyLocalCommand(ctx, req, now)
	if err != nil {
		return err
	}
	if r.ThreadID != "" && c.Wake != nil {
		c.Wake(r.ThreadID)
	}
	return nil
}

// DispatchOne replies to the original command message only. Its UUID and card
// are committed before the gateway call. Only definite rate-limit rejections
// reopen delivery; unknown and permanent rejections stay frozen.
func (c Commands) DispatchOne(ctx context.Context, gateway messages.MessageGateway) (bool, error) {
	if c.Store == nil || gateway == nil {
		return false, errors.New("command delivery requires storage and gateway")
	}
	r, claimed, err := c.Store.ClaimCommandReply(ctx, c.Config.Host.ID, c.now().UnixMilli())
	if err != nil || !claimed {
		return false, err
	}
	remoteID, sendErr := gateway.ReplyCard(ctx, r.MessageID, r.Card, r.ID)
	var limited *feishu.RateLimited
	if errors.As(sendErr, &limited) {
		at := c.now().UnixMilli()
		if at < r.DispatchedAtMS {
			at = r.DispatchedAtMS
		}
		delay := limited.RetryAfter.Milliseconds()
		if delay < 1000 {
			delay = 1000
		}
		return true, c.Store.DeferCommandReplyAttempt(ctx, r.ID, at, delay)
	}
	state := "confirmed"
	if sendErr != nil || remoteID == "" {
		state, remoteID = "unknown", ""
		var rejected *feishu.Rejected
		if errors.As(sendErr, &rejected) {
			state = "rejected"
		}
	}
	if err := c.Store.FinishCommandReply(ctx, r.ID, state, remoteID); err != nil {
		return true, err
	}
	return true, nil
}
