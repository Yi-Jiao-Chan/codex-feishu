package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type retryCommandGateway struct {
	t                   *testing.T
	db                  *store.Store
	ids, parents, cards []string
	err                 error
	onReply             func()
}

func (g *retryCommandGateway) ReplyCard(ctx context.Context, parent, card, uuid string) (string, error) {
	g.t.Helper()
	var state, savedCard, savedParent string
	if err := g.db.DB.QueryRowContext(ctx, "SELECT state,card_json,message_id FROM command_reply_attempts WHERE attempt_id=?", uuid).Scan(&state, &savedCard, &savedParent); err != nil || state != "dispatching" || savedCard != card || savedParent != parent {
		g.t.Fatal("reply lacked exact frozen journal", err)
	}
	g.ids = append(g.ids, uuid)
	g.parents = append(g.parents, parent)
	g.cards = append(g.cards, card)
	if g.onReply != nil {
		g.onReply()
	}
	return "remote-command-reply", g.err
}
func (g *retryCommandGateway) SendCard(context.Context, string, string, string, string) (string, error) {
	g.t.Fatal("reply fell back to send")
	return "", nil
}
func (g *retryCommandGateway) UpdateCard(context.Context, string, string) error {
	g.t.Fatal("reply became PATCH")
	return nil
}

func TestCommandsRetryReplyDoesNotReapplyOldNotificationAction(t *testing.T) {
	ctx := context.Background()
	c, s, in, now := appCommandFixture(t)
	at := now
	c.Now = func() time.Time { return at }
	in.Text = "/notify at off"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	g := &retryCommandGateway{t: t, db: s, err: &feishu.RateLimited{RetryAfter: 5 * time.Second}, onReply: func() { at = now.Add(3 * time.Second) }}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote {
		t.Fatal("rate rejection not deferred", err)
	}
	reopen, err := store.Open(c.Config.Storage.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	s.Close()
	s, c.Store, g.db = reopen, reopen, reopen
	if err := s.RecoverCommandDispatches(ctx, c.Config.Host.ID); err != nil {
		t.Fatal(err)
	}
	g.onReply = nil
	g.err = nil
	at = now.Add(7999 * time.Millisecond)
	if wrote, err := c.DispatchOne(ctx, g); err != nil || wrote || len(g.ids) != 1 {
		t.Fatal("reply resent before response-time deadline", err)
	}
	later := in
	later.MessageID = "notify-on"
	later.Text = "/notify at on"
	if err := c.Handle(ctx, later); err != nil {
		t.Fatal(err)
	}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote || g.parents[1] != later.MessageID {
		t.Fatal("older limit blocked newer command reply", err)
	}
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	at = now.Add(8 * time.Second)
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote || len(g.ids) != 3 {
		t.Fatal("due reply not recovered", err)
	}
	if g.ids[0] == g.ids[2] || g.parents[0] != g.parents[2] || !sameVisibleTestCard(t, s, g.ids[0], g.cards[0], g.ids[2], g.cards[2]) {
		t.Fatal("retry changed original context or reused UUID")
	}
	p, err := s.NotificationPolicy(ctx, "app", in.UserID)
	if err != nil || !p.Enabled || p.Revision != 2 {
		t.Fatal("old off action replayed", err)
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM notification_policy_history WHERE app_id='app' AND open_id=?", in.UserID).Scan(&count); err != nil || count != 3 {
		t.Fatal("reply recovery changed policy audit", err)
	}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || wrote || len(g.ids) != 3 {
		t.Fatal("confirmed reply resent", err)
	}
}

func TestCommandsRetryReplyDoesNotDuplicateStopAction(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := appCommandFixture(t)
	in.UserID = c.Config.Access.OperatorOpenIDs[0]
	in.MessageID = "stop-command"
	in.ChatType = "group"
	in.ChatID = "chat"
	in.ThreadID = "thread"
	in.Text = "/stop"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	g := &retryCommandGateway{t: t, db: s, err: &feishu.RateLimited{RetryAfter: time.Second}}
	if _, err := c.DispatchOne(ctx, g); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := c.Handle(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM submission_actions WHERE kind='stop'").Scan(&count); err != nil || count != 1 {
		t.Fatal("reply replay duplicated native action", count, err)
	}
}

func TestCommandsUnknownAfterRateRetryStaysFrozen(t *testing.T) {
	ctx := context.Background()
	c, s, in, now := appCommandFixture(t)
	at := now
	c.Now = func() time.Time { return at }
	in.Text = "/help"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	g := &retryCommandGateway{t: t, db: s, err: &feishu.RateLimited{RetryAfter: time.Second}}
	if _, err := c.DispatchOne(ctx, g); err != nil {
		t.Fatal(err)
	}
	at = now.Add(time.Second)
	g.err = errors.New("response lost")
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote {
		t.Fatal(err)
	}
	if err := s.RecoverCommandDispatches(ctx, c.Config.Host.ID); err != nil {
		t.Fatal(err)
	}
	at = now.Add(time.Hour)
	g.err = nil
	if wrote, err := c.DispatchOne(ctx, g); err != nil || wrote || len(g.ids) != 2 {
		t.Fatal("unknown retry repeated", err)
	}
}
