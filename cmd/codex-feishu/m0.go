package main

import (
	"codex-feishu/internal/app"
	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"context"
	"errors"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"strings"
	"time"
)

func toast(kind, msg string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: kind, Content: msg}}
}
func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func listenM0(ctx context.Context, cfg config.Config, g *feishu.Gateway, db *store.Store, o *output, bridge *app.M0) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bridgeDone := make(chan error, 1)
	if bridge != nil {
		bridge.OnEvidence = func(id string, data map[string]any) {
			status, ok, required := "pass", true, true
			if strings.HasSuffix(id, "_unavailable") {
				status, ok = "pending", false
				required = id == "m0.final_unavailable"
				if data["state"] == "rejected" {
					status = "fail"
				}
			}
			emit(output{Command: "m0-listen", OK: ok, State: "m0-evidence", Checks: []check{{id, status, required, data}}})
		}
		go func() { bridgeDone <- bridge.Run(ctx) }()
	}
	done := make(chan error, 1)
	go func() {
		done <- g.Listen(ctx, func(ctx context.Context, e *im.P2MessageReceiveV1) error {
			if e == nil || e.Event == nil || e.Event.Sender == nil || e.Event.Message == nil {
				return nil
			}
			s, m := e.Event.Sender, e.Event.Message
			if value(s.SenderType) != "user" || s.SenderId == nil || value(s.SenderId.OpenId) != cfg.Access.OwnerOpenID {
				return nil
			}
			if bridge != nil {
				if err := bridge.Message(ctx, e); err != nil {
					return err
				}
			}
			key := "m0.receive." + value(m.ChatType)
			fresh, err := db.Begin(ctx, key, "feishu.receive_event")
			if err != nil {
				return err
			}
			if fresh {
				if err = db.Finish(ctx, key, "confirmed", value(m.MessageId)); err != nil {
					return err
				}
				emit(output{Command: "m0-listen", OK: true, State: "message-event-observed", Checks: []check{{"feishu.message_event", "pass", true, map[string]any{"chat_type": value(m.ChatType), "message_id": value(m.MessageId), "message_type": value(m.MessageType), "mention_count": len(m.Mentions), "sender_verified": true}}}})
			}
			return nil
		}, func(ctx context.Context, e *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			if e == nil || e.Event == nil || e.Event.Operator == nil || e.Event.Action == nil || e.Event.Context == nil {
				return toast("error", "无效回调"), nil
			}
			if e.Event.Operator.OpenID != cfg.Access.OwnerOpenID {
				return toast("error", "仅测试用户可以确认"), nil
			}
			if bridge != nil && e.Event.Action.Value["action"] != "probe" {
				return bridge.Action(ctx, e)
			}
			_, token, err := db.Operation(ctx, "m0-test-card-token")
			if err != nil {
				return toast("error", "验证令牌不可用"), nil
			}
			_, mid, err := db.Operation(ctx, "m0-test-card")
			if err != nil {
				return toast("error", "验证卡不可用"), nil
			}
			if e.Event.Action.Value["action"] != "probe" || e.Event.Action.Value["token"] != token || e.Event.Context.OpenMessageID != mid {
				return toast("error", "卡片已失效"), nil
			}
			fresh, err := db.Begin(ctx, "m0.card_callback", "feishu.card_callback")
			if err != nil {
				return toast("error", "保存回调失败"), err
			}
			if fresh {
				if err = db.Finish(ctx, "m0.card_callback", "confirmed", mid); err != nil {
					return toast("error", "保存回调失败"), err
				}
				emit(output{Command: "m0-listen", OK: true, State: "callback-observed", Checks: []check{{"feishu.card_callback", "pass", true, map[string]string{"message_id": mid, "operator": "owner"}}}})
			}
			return toast("success", "已确认：卡片回调可用"), nil
		})
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	ready := false
	for {
		select {
		case err := <-bridgeDone:
			if ctx.Err() != nil {
				return 0
			}
			o.OK = false
			o.State = "m0-bridge-stopped"
			if err != nil {
				o.Checks = append(o.Checks, check{"m0.runtime", "fail", true, err.Error()})
			}
			emit(*o)
			return 5
		case err := <-done:
			if ctx.Err() != nil {
				return 0
			}
			o.OK = false
			o.State = "feishu-listener-ended"
			if err != nil {
				o.Checks = append(o.Checks, check{"feishu.long_connection", "fail", true, "official SDK listener ended; credentials/event configuration require checking"})
			}
			emit(*o)
			return 4
		case <-ticker.C:
			if g.Ready() && !ready {
				ready = true
				fresh, err := db.Begin(ctx, "m0.long_connection", "feishu.long_connection")
				if err != nil {
					return 7
				}
				if fresh {
					if err = db.Finish(ctx, "m0.long_connection", "confirmed", ""); err != nil {
						return 7
					}
				}
				o.OK = true
				o.State = "m0-listening"
				o.Checks = append(o.Checks, check{"feishu.long_connection", "pass", true, nil})
				emit(*o)
			}
		case <-ctx.Done():
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					return 4
				}
				return 0
			case <-time.After(5 * time.Second):
				return 7
			}
		}
	}
}
