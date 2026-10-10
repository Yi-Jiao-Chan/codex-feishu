// M0 is a deliberately scoped live harness, not the production discovery service.
package app

import (
	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"strings"
	"time"
)

type M0 struct {
	cfg               config.Config
	c                 *codexclient.Client
	g                 messageGateway
	progress          map[string]progressState
	turnParents       map[string]string
	db                *store.Store
	thread, chat, bot string
	OnEvidence        func(string, map[string]any)
}

func NewM0(ctx context.Context, cfg config.Config, c *codexclient.Client, g *feishu.Gateway, db *store.Store, thread, chat, bot string) (*M0, error) {
	_, approved, err := db.Operation(ctx, "m0-test-group")
	if err != nil || chat != approved {
		return nil, errors.New("M0 requires the previously created private test group")
	}
	meta, err := c.Metadata(ctx, thread)
	if err != nil {
		return nil, err
	}
	if !meta.InteractiveMain() || !cfg.Allows(meta.CWD) {
		return nil, errors.New("M0 thread must be a persistent main thread in allowed roots")
	}
	if !strings.Contains(meta.Preview, "M0_APP_ORIGIN_20261010") && !strings.Contains(meta.Preview, "M0_APP_ORIGIN_V2_20261010") {
		return nil, errors.New("M0 requires the explicit App test marker; existing work threads are excluded")
	}
	if _, err = c.Attach(ctx, thread); err != nil {
		return nil, err
	}
	if err = db.Bind(ctx, thread, chat); err != nil {
		return nil, err
	}
	return &M0{cfg: cfg, c: c, g: g, db: db, thread: thread, chat: chat, bot: bot}, nil
}
func ptr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
func (m *M0) evidence(id string, data map[string]any) {
	if m.OnEvidence != nil {
		m.OnEvidence(id, data)
	}
}
func (m *M0) Message(ctx context.Context, e *im.P2MessageReceiveV1) error {
	if e == nil || e.Event == nil || e.Event.Message == nil || e.Event.Sender == nil {
		return nil
	}
	msg, s := e.Event.Message, e.Event.Sender
	if ptr(msg.ChatId) != m.chat || ptr(msg.ChatType) != "group" || ptr(s.SenderType) != "user" || s.SenderId == nil || !m.cfg.Operator(ptr(s.SenderId.OpenId)) {
		return nil
	}
	mentioned := false
	for _, a := range msg.Mentions {
		if a != nil && a.Id != nil && ptr(a.Id.OpenId) == m.bot {
			mentioned = true
		}
	}
	if !mentioned {
		return nil
	}
	text, err := Text(msg, m.bot)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(text), "/") {
		return errors.New("M0 commands are not implemented; use plain test text")
	}
	// Persist before acknowledging the Feishu event; Run consumes received rows.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, fresh, err := m.db.Claim(ctx, store.Receipt{AppID: m.cfg.Feishu.AppID, MessageID: ptr(msg.MessageId), ThreadID: m.thread, ChatID: m.chat, Body: text})
	if err == nil && fresh {
		m.evidence("m0.input_received", map[string]any{"message_id": ptr(msg.MessageId), "thread_id": m.thread})
	}
	return err
}

// Text retains actual content and removes only structured mentions of this bot.
func Text(msg *im.EventMessage, bot string) (string, error) {
	kind := ptr(msg.MessageType)
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(ptr(msg.Content)), &raw) != nil {
		return "", errors.New("invalid message JSON")
	}
	var text string
	switch kind {
	case "text":
		if json.Unmarshal(raw["text"], &text) != nil {
			return "", errors.New("invalid text message")
		}
	case "post":
		// Feishu uses either an unlocalized post or language-keyed posts.
		var post struct {
			Title   string `json:"title"`
			Content [][]struct {
				Tag    string `json:"tag"`
				Text   string `json:"text"`
				UserID string `json:"user_id"`
			} `json:"content"`
		}
		b := []byte(ptr(msg.Content))
		if _, ok := raw["content"]; !ok {
			if v, ok := raw["zh_cn"]; ok {
				b = v
			} else if v, ok := raw["en_us"]; ok {
				b = v
			} else {
				return "", errors.New("unsupported post language")
			}
		}
		if json.Unmarshal(b, &post) != nil {
			return "", errors.New("invalid post")
		}
		var lines []string
		if post.Title != "" {
			lines = append(lines, post.Title)
		}
		for _, row := range post.Content {
			var line strings.Builder
			for _, el := range row {
				switch el.Tag {
				case "text", "a":
					line.WriteString(el.Text)
				case "at":
					if el.UserID != bot {
						line.WriteString("@" + el.UserID)
					}
				default:
					return "", errors.New("M0 accepts text-only posts")
				}
			}
			lines = append(lines, line.String())
		}
		text = strings.Join(lines, "\n")
	default:
		return "", errors.New("M0 accepts text and text-only post messages")
	}
	for _, mention := range msg.Mentions {
		if mention != nil && mention.Id != nil && ptr(mention.Id.OpenId) == bot {
			text = strings.ReplaceAll(text, ptr(mention.Key), "")
		}
	}
	return strings.TrimSpace(text), nil
}
func (m *M0) Action(ctx context.Context, e *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	respond := func(t, s string) *callback.CardActionTriggerResponse {
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: t, Content: s}}
	}
	if e == nil || e.Event == nil || e.Event.Operator == nil || e.Event.Action == nil || e.Event.Context == nil {
		return respond("error", "无效操作"), nil
	}
	a := e.Event
	if !m.cfg.Operator(a.Operator.OpenID) || a.Context.OpenChatID != m.chat {
		return respond("error", "无执行权限"), nil
	}
	token, ok := a.Action.Value["token"].(string)
	if !ok {
		return respond("error", "卡片已失效"), nil
	}
	kind, ok := a.Action.Value["action"].(string)
	if !ok || (kind != "steer" && kind != "remove") {
		return respond("error", "不支持的操作"), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	r, err := m.db.ByToken(ctx, token)
	if err != nil || r.ThreadID != m.thread || r.ChatID != m.chat || r.CardID != a.Context.OpenMessageID || r.State != "queued" {
		return respond("info", "状态已更新，请查看最新卡片"), nil
	}
	_, err = m.db.DB.ExecContext(ctx, "INSERT OR IGNORE INTO m0_actions(receipt_id,kind,state) VALUES(?,?,'received')", r.ID, kind)
	if err != nil {
		return respond("error", "保存操作失败"), err
	}
	return respond("info", "已收到，正在处理"), nil
}
func (m *M0) Run(ctx context.Context) error {
	if err := m.reconcile(ctx, true); err != nil {
		return err
	}
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-m.c.Events():
			if !ok {
				return m.c.Err()
			}
			var p struct {
				ThreadID string           `json:"threadId"`
				Item     codexclient.Item `json:"item"`
				Turn     codexclient.Turn `json:"turn"`
			}
			if json.Unmarshal(event.Params, &p) != nil || p.ThreadID != m.thread {
				continue
			}
			if event.Method == "item/completed" && p.Item.Type == "userMessage" {
				if err := m.mirror(ctx, p.Item); err != nil {
					return err
				}
			}
			if event.Method == "turn/completed" {
				if err := m.reconcile(ctx, false); err != nil {
					return err
				}
			}
			if len(event.RequestID) > 0 {
				m.evidence("m0.needs_app", map[string]any{"thread_id": m.thread, "method": event.Method})
			}
		case <-timer.C:
			if err := m.reconcile(ctx, false); err != nil {
				return err
			}
			if err := m.submitReceived(ctx); err != nil {
				return err
			}
			if err := m.processActions(ctx); err != nil {
				return err
			}
		}
	}
}
func (m *M0) submitReceived(ctx context.Context) error {
	receipts, err := m.db.Receipts(ctx, m.thread)
	if err != nil {
		return err
	}
	for _, r := range receipts {
		if r.State != "received" {
			continue
		}
		m.syncReaction(ctx, r, codexclient.Turn{})
		key := "m0.add." + r.ID
		fresh, err := m.db.Begin(ctx, key, "codex.queue.add")
		if err != nil {
			return err
		}
		if !fresh {
			r.State = "unknown"
			if err = m.db.SaveReceipt(ctx, r); err != nil {
				return err
			}
			continue
		}
		call, cancel := context.WithTimeout(ctx, 20*time.Second)
		q, err := m.c.Add(call, m.thread, r.Body, r.ClientID)
		cancel()
		if err != nil {
			r.State = "unknown"
			var rejected *codexclient.RPCError
			if errors.As(err, &rejected) {
				r.State = "failed"
			}
			if e := m.db.Finish(ctx, key, r.State, ""); e != nil {
				return e
			}
		} else {
			r.QueueID = q.ID
			r.State = "accepted"
			if err = m.db.Finish(ctx, key, "confirmed", q.ID); err != nil {
				return err
			}
			pending, err := m.c.Queue(ctx, m.thread)
			if err != nil {
				return err
			}
			for _, entry := range pending {
				if entry.ID == q.ID {
					r.State = "queued"
				}
			}
		}
		if err = m.db.SaveReceipt(ctx, r); err != nil {
			return err
		}
		if err = m.receiptCard(ctx, &r); err != nil {
			return err
		}
		m.evidence("m0.native_submission", map[string]any{"receipt_id": r.ID, "thread_id": m.thread, "state": r.State, "native_queue_id": r.QueueID})
	}
	return nil
}
func (m *M0) receiptCard(ctx context.Context, r *store.Receipt) error {
	// Native consumption on an idle thread needs no bot echo of human input.
	if r.CardID == "" && (r.State == "accepted" || r.State == "consumed") {
		return nil
	}
	card := cards.Queue(r.State, r.ActionToken)
	if r.CardID != "" {
		return m.g.UpdateCard(ctx, r.CardID, card)
	}
	key := "m0.receipt-card." + r.ID
	fresh, err := m.db.Begin(ctx, key, "feishu.send_card")
	if err != nil {
		return err
	}
	if !fresh {
		_, id, e := m.db.Operation(ctx, key)
		if e != nil {
			return e
		}
		if id != "" {
			r.CardID = id
			return m.db.SaveReceipt(ctx, *r)
		}
		return nil
	}
	id, err := m.g.ReplyCard(ctx, r.MessageID, card, uuid(key))
	if err != nil {
		if e := m.db.Finish(ctx, key, resultState(err), ""); e != nil {
			return e
		}
		return err
	}
	if err = m.db.Finish(ctx, key, "confirmed", id); err != nil {
		return err
	}
	r.CardID = id
	return m.db.SaveReceipt(ctx, *r)
}
func (m *M0) mirror(ctx context.Context, item codexclient.Item) error {
	text := item.PublicText()
	if text == "" || item.Type != "userMessage" {
		return nil
	}
	if item.Type == "userMessage" && item.ClientID != nil {
		receipts, err := m.db.Receipts(ctx, m.thread)
		if err != nil {
			return err
		}
		for _, r := range receipts {
			if r.ClientID == *item.ClientID {
				return nil
			}
		}
	}
	role, source := "人类发送", "Codex App"
	runes := []rune(text)
	for start := 0; start < len(runes); start += 3500 {
		end := start + 3500
		if end > len(runes) {
			end = len(runes)
		}
		key := fmt.Sprintf("m0.mirror.%s.%s.%d", m.thread, item.ID, start)
		fresh, err := m.db.Begin(ctx, key, "feishu.mirror")
		if err != nil {
			return err
		}
		if !fresh {
			continue
		}
		id, err := m.g.SendCard(ctx, "chat_id", m.chat, cards.Message(role, source, string(runes[start:end])), store.ID())
		if err != nil {
			if e := m.db.Finish(ctx, key, resultState(err), ""); e != nil {
				return e
			}
			return err
		}
		if err = m.db.Finish(ctx, key, "confirmed", id); err != nil {
			return err
		}
		m.evidence("m0.mirror", map[string]any{"thread_id": m.thread, "item_id": item.ID, "role": item.Type, "message_id": id})
	}
	return nil
}
func (m *M0) reconcile(ctx context.Context, initial bool) error {
	call, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	turns, err := m.c.Turns(call, m.thread)
	if err != nil {
		return err
	}
	q, err := m.c.Queue(call, m.thread)
	if err != nil {
		return err
	}
	native := map[string]string{}
	history := map[string]codexclient.Turn{}
	for _, v := range q {
		if v.ClientUserMessageID != nil {
			native[*v.ClientUserMessageID] = v.ID
		}
	}
	for _, t := range turns {
		for _, i := range t.Items {
			if i.ClientID != nil {
				history[*i.ClientID] = t
			}
			if initial || i.Type == "userMessage" || t.Status != "inProgress" {
				if err = m.mirror(ctx, i); err != nil {
					return err
				}
			}
		}
		if err = m.syncTurn(ctx, t); err != nil {
			return err
		}
	}
	receipts, err := m.db.Receipts(ctx, m.thread)
	if err != nil {
		return err
	}
	for _, r := range receipts {
		before := r.State
		if _, ok := history[r.ClientID]; ok {
			if r.State != "steered" {
				r.State = "consumed"
			}
		} else if id, ok := native[r.ClientID]; ok {
			r.QueueID = id
			r.State = "queued"
		}
		// Absence is not rejection. An uncertain input remains uncertain.
		if before != r.State {
			if err = m.db.SaveReceipt(ctx, r); err != nil {
				return err
			}
			if err = m.receiptCard(ctx, &r); err != nil {
				return err
			}
		}
		m.syncReaction(ctx, r, history[r.ClientID])
	}
	return nil
}
func resultState(err error) string {
	var a *codexclient.RPCError
	var b *feishu.Rejected
	if errors.As(err, &a) || errors.As(err, &b) {
		return "rejected"
	}
	return "unknown"
}
func (m *M0) processActions(ctx context.Context) error {
	rows, err := m.db.DB.QueryContext(ctx, "SELECT receipt_id,kind FROM m0_actions WHERE state='received'")
	if err != nil {
		return err
	}
	type action struct{ id, kind string }
	var list []action
	for rows.Next() {
		var a action
		if err = rows.Scan(&a.id, &a.kind); err != nil {
			rows.Close()
			return err
		}
		list = append(list, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	receipts, err := m.db.Receipts(ctx, m.thread)
	if err != nil {
		return err
	}
	for _, a := range list {
		var r store.Receipt
		for _, v := range receipts {
			if v.ID == a.id {
				r = v
				break
			}
		}
		if r.ID == "" {
			return errors.New("action receipt absent")
		}
		// Mark processing before remote writes. A crash leaves it frozen for audit.
		if _, err = m.db.DB.ExecContext(ctx, "UPDATE m0_actions SET state='processing' WHERE receipt_id=? AND state='received'", a.id); err != nil {
			return err
		}
		if err = m.act(ctx, &r, a.kind); err != nil {
			return err
		}
		if _, err = m.db.DB.ExecContext(ctx, "UPDATE m0_actions SET state='done' WHERE receipt_id=?", a.id); err != nil {
			return err
		}
	}
	return nil
}
func (m *M0) act(ctx context.Context, r *store.Receipt, kind string) error {
	call, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	turns, err := m.c.Turns(call, m.thread)
	if err != nil {
		return err
	}
	expected := ""
	for _, t := range turns {
		if t.Status == "inProgress" {
			expected = t.ID
		}
	}
	if kind == "steer" && expected == "" {
		return m.reconcile(ctx, false)
	}
	// Exact deletion is the ownership claim, not a preliminary queue-list check.
	key := "m0.delete." + r.ID
	fresh, err := m.db.Begin(ctx, key, "codex.queue.delete")
	if err != nil {
		return err
	}
	if !fresh {
		return nil
	}
	deleted, err := m.c.Delete(call, m.thread, r.QueueID)
	if err != nil {
		r.State = "unknown"
		if e := m.db.Finish(ctx, key, resultState(err), ""); e != nil {
			return e
		}
	} else {
		if err = m.db.Finish(ctx, key, "confirmed", fmt.Sprint(deleted)); err != nil {
			return err
		}
		if !deleted {
			return m.reconcile(ctx, false)
		}
		r.State = "removed"
		if kind == "steer" {
			steerKey := "m0.steer." + r.ID
			fresh, e := m.db.Begin(ctx, steerKey, "codex.turn.steer")
			if e != nil {
				return e
			}
			if !fresh {
				r.State = "unknown"
			} else {
				err = m.c.Steer(call, m.thread, expected, r.Body, r.ClientID)
				if err == nil {
					r.State = "steered"
					if e = m.db.Finish(ctx, steerKey, "confirmed", expected); e != nil {
						return e
					}
				} else {
					if e = m.db.Finish(ctx, steerKey, resultState(err), expected); e != nil {
						return e
					}
					r.State = "unknown"
					var rejected *codexclient.RPCError
					if errors.As(err, &rejected) {
						restoreKey := "m0.restore." + r.ID
						fresh, e = m.db.Begin(ctx, restoreKey, "codex.queue.add.restore")
						if e != nil {
							return e
						}
						if fresh {
							q, e := m.c.Add(call, m.thread, r.Body, r.ClientID)
							if e == nil {
								r.QueueID = q.ID
								r.State = "queued"
								if e = m.db.Finish(ctx, restoreKey, "confirmed", q.ID); e != nil {
									return e
								}
							} else {
								if e = m.db.Finish(ctx, restoreKey, resultState(e), ""); e != nil {
									return e
								}
							}
						}
					}
				}
			}
		}
	}
	if err = m.db.SaveReceipt(ctx, *r); err != nil {
		return err
	}
	if err = m.receiptCard(ctx, r); err != nil {
		return err
	}
	m.evidence("m0.action", map[string]any{"receipt_id": r.ID, "kind": kind, "state": r.State, "expected_turn_id": expected})
	return nil
}
