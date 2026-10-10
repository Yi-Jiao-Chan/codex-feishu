// Package feishu uses the official SDK transport, without Channel's send fallback.
package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	core "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	ws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type discardLogger struct{}

func (discardLogger) Debug(context.Context, ...interface{}) {}
func (discardLogger) Info(context.Context, ...interface{})  {}
func (discardLogger) Warn(context.Context, ...interface{})  {}
func (discardLogger) Error(context.Context, ...interface{}) {}

type Rejected struct {
	Code                 int
	HTTPStatus           int
	RequestID            string
	RequiredPermissions  []string
	CardValidationDetail string
}

func (e *Rejected) Error() string {
	return fmt.Sprintf("Feishu rejected: code=%d status=%d request_id=%s", e.Code, e.HTTPStatus, e.RequestID)
}

type Unknown struct{ Operation string }

func (e *Unknown) Error() string { return "Feishu response unknown: " + e.Operation }

type Gateway struct {
	cfg    *core.Config
	appID  string
	secret string
	ready  atomic.Bool
}

func New(appID, secret string) *Gateway {
	c := &core.Config{BaseUrl: "https://open.feishu.cn", AppId: appID, AppSecret: secret, AppType: core.AppTypeSelfBuilt, EnableTokenCache: true, ReqTimeout: 20 * time.Second, Logger: discardLogger{}, HttpClient: &http.Client{Timeout: 20 * time.Second}}
	core.NewLogger(c)
	core.NewCache(c)
	core.NewSerialization(c)
	return &Gateway{cfg: c, appID: appID, secret: secret}
}
func (g *Gateway) Ready() bool { return g.ready.Load() }

// request exposes only safe API error metadata; never token/body/SDK error strings.
func (g *Gateway) request(ctx context.Context, method, path string, q core.QueryParams, body any, dst any) error {
	r, err := core.Request(ctx, &core.ApiReq{HttpMethod: method, ApiPath: path, QueryParams: q, Body: body, SupportedAccessTokenTypes: []core.AccessTokenType{core.AccessTokenTypeTenant}}, g.cfg)
	if err != nil {
		return &Unknown{method + " " + path}
	}
	var status struct {
		Code    int    `json:"code"`
		Message string `json:"msg"`
		Error   struct {
			PermissionViolations []struct {
				Subject string `json:"subject"`
			} `json:"permission_violations"`
		} `json:"error"`
	}
	if json.Unmarshal(r.RawBody, &status) != nil {
		return &Unknown{method + " " + path}
	}
	if status.Code != 0 || r.StatusCode < 200 || r.StatusCode >= 300 {
		e := &Rejected{Code: status.Code, HTTPStatus: r.StatusCode, RequestID: r.RequestId()}
		if status.Code == 230099 {
			e.CardValidationDetail = strings.ReplaceAll(status.Message, g.secret, "[redacted]")
		}
		for _, p := range status.Error.PermissionViolations {
			e.RequiredPermissions = append(e.RequiredPermissions, p.Subject)
		}
		return e
	}
	if dst != nil && json.Unmarshal(r.RawBody, dst) != nil {
		return &Unknown{method + " " + path}
	}
	return nil
}
func (g *Gateway) Bot(ctx context.Context) (string, error) {
	var r struct {
		Bot struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	err := g.request(ctx, "GET", "/open-apis/bot/v3/info", nil, nil, &r)
	if err == nil && r.Bot.OpenID == "" {
		err = errors.New("bot identity absent; enable robot capability")
	}
	return r.Bot.OpenID, err
}
func (g *Gateway) CreateGroup(ctx context.Context, name, owner string, members []string, uuid string) (string, error) {
	var r struct {
		Data struct {
			ChatID string `json:"chat_id"`
		} `json:"data"`
	}
	body := map[string]any{"name": name, "owner_id": owner, "user_id_list": members, "chat_mode": "group", "chat_type": "private", "description": "Codex Feishu M0 联调测试群"}
	err := g.request(ctx, "POST", "/open-apis/im/v1/chats", core.QueryParams{"user_id_type": {"open_id"}, "uuid": {uuid}}, body, &r)
	if err == nil && r.Data.ChatID == "" {
		err = &Unknown{"create group response missing chat_id"}
	}
	return r.Data.ChatID, err
}
func (g *Gateway) SendCard(ctx context.Context, targetType, target, card, uuid string) (string, error) {
	var r struct {
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	body := map[string]any{"receive_id": target, "msg_type": "interactive", "content": card, "uuid": uuid}
	err := g.request(ctx, "POST", "/open-apis/im/v1/messages", core.QueryParams{"receive_id_type": {targetType}}, body, &r)
	if err == nil && r.Data.MessageID == "" {
		err = &Unknown{"send card response missing message_id"}
	}
	return r.Data.MessageID, err
}
func (g *Gateway) UpdateCard(ctx context.Context, id, card string) error {
	return g.request(ctx, "PATCH", "/open-apis/im/v1/messages/"+url.PathEscape(id), nil, map[string]string{"content": card}, nil)
}
func (g *Gateway) Chat(ctx context.Context, id string) error {
	return g.request(ctx, "GET", "/open-apis/im/v1/chats/"+url.PathEscape(id), nil, nil, nil)
}
func (g *Gateway) ReadMessage(ctx context.Context, id string) error {
	return g.request(ctx, "GET", "/open-apis/im/v1/messages/"+url.PathEscape(id), nil, nil, nil)
}
func (g *Gateway) Recall(ctx context.Context, id string) error {
	return g.request(ctx, "DELETE", "/open-apis/im/v1/messages/"+url.PathEscape(id), nil, nil, nil)
}
func (g *Gateway) Scopes(ctx context.Context) (json.RawMessage, error) {
	var r struct {
		Data json.RawMessage `json:"data"`
	}
	err := g.request(ctx, "GET", "/open-apis/application/v6/scopes", nil, nil, &r)
	return r.Data, err
}
func (g *Gateway) Listen(ctx context.Context, onMessage func(context.Context, *im.P2MessageReceiveV1) error, onAction func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error)) error {
	d := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(onMessage).OnP2CardActionTrigger(onAction)
	d.Config.Logger = discardLogger{}
	w := ws.NewClient(g.appID, g.secret, ws.WithEventHandler(d), ws.WithLogger(discardLogger{}), ws.WithOnReady(func() { g.ready.Store(true) }), ws.WithOnReconnected(func() { g.ready.Store(true) }), ws.WithOnDisconnected(func() { g.ready.Store(false) }), ws.WithOnReconnecting(func() { g.ready.Store(false) }))
	defer func() { g.ready.Store(false); w.Close() }()
	return w.Start(ctx)
}

func (g *Gateway) AddReaction(ctx context.Context, messageID, emoji string) (string, error) {
	var r struct {
		Data struct {
			ID string `json:"reaction_id"`
		} `json:"data"`
	}
	err := g.request(ctx, "POST", "/open-apis/im/v1/messages/"+url.PathEscape(messageID)+"/reactions", nil, map[string]any{"reaction_type": map[string]string{"emoji_type": emoji}}, &r)
	if err == nil && r.Data.ID == "" {
		err = &Unknown{"reaction response missing reaction_id"}
	}
	return r.Data.ID, err
}
func (g *Gateway) DeleteReaction(ctx context.Context, messageID, reactionID string) error {
	return g.request(ctx, "DELETE", "/open-apis/im/v1/messages/"+url.PathEscape(messageID)+"/reactions/"+url.PathEscape(reactionID), nil, nil, nil)
}

func (g *Gateway) BotInChat(ctx context.Context, chatID string) (bool, error) {
	var r struct {
		Data struct {
			Present *bool `json:"is_in_chat"`
		} `json:"data"`
	}
	err := g.request(ctx, "GET", "/open-apis/im/v1/chats/"+url.PathEscape(chatID)+"/members/is_in_chat", nil, nil, &r)
	if err == nil && r.Data.Present == nil {
		return false, &Unknown{"membership response missing is_in_chat"}
	}
	if err != nil {
		return false, err
	}
	return *r.Data.Present, nil
}
func (g *Gateway) MemberIDs(ctx context.Context, chatID string) ([]string, error) {
	var ids []string
	cursor := ""
	seen := map[string]bool{}
	for {
		var r struct {
			Data struct {
				Items []struct {
					ID string `json:"member_id"`
				} `json:"items"`
				More      bool   `json:"has_more"`
				Token     string `json:"page_token"`
				Truncated bool   `json:"trigger_security_conf_limit"`
			} `json:"data"`
		}
		q := core.QueryParams{"member_id_type": {"open_id"}, "page_size": {"100"}}
		if cursor != "" {
			q["page_token"] = []string{cursor}
		}
		if err := g.request(ctx, "GET", "/open-apis/im/v1/chats/"+url.PathEscape(chatID)+"/members", q, nil, &r); err != nil {
			return nil, err
		}
		if r.Data.Truncated {
			return nil, errors.New("member list restricted by security configuration")
		}
		for _, member := range r.Data.Items {
			if member.ID != "" {
				ids = append(ids, member.ID)
			}
		}
		if !r.Data.More {
			return ids, nil
		}
		if r.Data.Token == "" || seen[r.Data.Token] {
			return nil, errors.New("invalid membership pagination")
		}
		seen[r.Data.Token] = true
		cursor = r.Data.Token
	}
}

// Reply remains a group reply; never downgrade a rejected reply to SendCard.
func (g *Gateway) ReplyCard(ctx context.Context, parent, card, uuid string) (string, error) {
	var r struct {
		Data struct {
			ID string `json:"message_id"`
		} `json:"data"`
	}
	body := map[string]any{"msg_type": "interactive", "content": card, "uuid": uuid, "reply_in_thread": false}
	err := g.request(ctx, "POST", "/open-apis/im/v1/messages/"+url.PathEscape(parent)+"/reply", nil, body, &r)
	if err == nil && r.Data.ID == "" {
		err = &Unknown{"reply response missing message_id"}
	}
	return r.Data.ID, err
}

func (g *Gateway) MessageParent(ctx context.Context, id string) (string, error) {
	var r struct {
		Data struct {
			Items []struct {
				ID     string `json:"message_id"`
				Parent string `json:"parent_id"`
			} `json:"items"`
		} `json:"data"`
	}
	err := g.request(ctx, "GET", "/open-apis/im/v1/messages/"+url.PathEscape(id), nil, nil, &r)
	if err != nil {
		return "", err
	}
	for _, item := range r.Data.Items {
		if item.ID == id {
			return item.Parent, nil
		}
	}
	return "", &Unknown{"readback missing message"}
}
