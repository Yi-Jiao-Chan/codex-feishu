package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/rules"
)

type feedbackReadbackGateway struct {
	requestGateway
	chat                             string
	listCalls, getCalls, memberCalls int
	readErr                          error
	cardOverride                     string
}

func (g *feedbackReadbackGateway) record() feishu.CardMessage {
	deleted := false
	card := g.card
	if g.cardOverride != "" {
		card = g.cardOverride
	}
	return feishu.CardMessage{ID: "readback-card", ChatID: g.chat, Type: "interactive", SenderID: "app", SenderIDType: "app_id", SenderType: "app", ParentID: &g.parent, Deleted: &deleted, Content: card}
}
func (g *feedbackReadbackGateway) ReadCardMessage(context.Context, string) (feishu.CardMessage, error) {
	g.getCalls++
	return g.record(), g.readErr
}
func (g *feedbackReadbackGateway) ListCardMessages(_ context.Context, kind, id, cursor string) (feishu.CardMessagePage, error) {
	g.listCalls++
	if kind != "chat" || id != g.chat || cursor != "" {
		return feishu.CardMessagePage{}, errors.New("wrong readback scope")
	}
	return feishu.CardMessagePage{Items: []feishu.CardMessage{g.record()}}, g.readErr
}
func (g *feedbackReadbackGateway) MemberIDs(context.Context, string) ([]string, error) {
	g.memberCalls++
	return []string{"ou_user"}, nil
}

func TestCommandReadbackConfirmsOriginalReplyWithoutRepeatingEffect(t *testing.T) {
	ctx := context.Background()
	c, s, in, now := appCommandFixture(t)
	in.Text = "/notify at off"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	g := &feedbackReadbackGateway{requestGateway: requestGateway{sendErr: &feishu.Unknown{Operation: "fixture"}}, chat: in.ChatID}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	id, wire := g.uuid, g.card
	p, err := s.NotificationPolicy(ctx, "app", in.UserID)
	if err != nil {
		t.Fatal(err)
	}
	p.Enabled, p.Revision = true, p.Revision+1
	if err := s.SetNotificationPolicyAt(ctx, "app", in.UserID, p.Revision-1, p, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverCommandDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote || g.calls != 1 || g.listCalls != 1 || g.getCalls != 1 {
		t.Fatal("unknown response was not read back", wrote, err, g)
	}
	current, err := s.CommandReply(ctx, "host", "app", in.MessageID)
	if err != nil || current.State != "confirmed" || current.ID != id || current.RemoteID != "readback-card" {
		t.Fatal(current, err)
	}
	var frozen string
	if err := s.DB.QueryRow(`SELECT card_json FROM command_reply_attempts WHERE attempt_id=?`, id).Scan(&frozen); err != nil || frozen != wire {
		t.Fatal("original wire changed", err)
	}
	p, err = s.NotificationPolicy(ctx, "app", in.UserID)
	if err != nil || !p.Enabled || p.Revision != 2 {
		t.Fatal("readback reapplied old off", p, err)
	}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || wrote || g.calls != 1 || g.listCalls != 1 {
		t.Fatal("confirmed reply repeated", wrote, err)
	}
}

func TestResolvedAppRequestReadbackDoesNotReopenEligibilityOrTouchMembership(t *testing.T) {
	ctx := context.Background()
	tr, now, _ := liveRetryTracker(t)
	g := &feedbackReadbackGateway{requestGateway: requestGateway{sendErr: &feishu.Unknown{Operation: "fixture"}}, chat: "chat"}
	tr.Gateway, tr.Membership = g, g
	if wrote, err := tr.DispatchOne(ctx, "thread"); err != nil || !wrote || g.memberCalls != 1 {
		t.Fatal(wrote, err)
	}
	if err := tr.ObserveContext(ctx, routedEvent("serverRequest/resolved", "19", 1)); err != nil {
		t.Fatal(err)
	}
	p, _ := rules.DefaultPolicy("UTC")
	p.Enabled, p.Revision = false, 1
	*now = now.Add(time.Second)
	if err := tr.Store.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, *now); err != nil {
		t.Fatal(err)
	}
	if err := tr.Store.RecoverRequestReminders(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	if wrote, err := tr.DispatchOne(ctx, "thread"); err != nil || !wrote || g.calls != 1 || g.memberCalls != 1 || g.getCalls != 1 {
		t.Fatal("resolved request recovery submitted or regenerated reminder", wrote, err, g)
	}
	var state, reminder string
	if err := tr.Store.DB.QueryRow(`SELECT state,reminder_state FROM app_requests`).Scan(&state, &reminder); err != nil || state != "resolved" || reminder != "confirmed" {
		t.Fatal(state, reminder, err)
	}
	if d := trackerDecision(t, tr, "19"); d.State != "confirmed" || d.AttemptID != g.uuid {
		t.Fatal(d)
	}
	if wrote, err := tr.DispatchOne(ctx, "thread"); err != nil || wrote || g.calls != 1 {
		t.Fatal("resolved reminder repeated", wrote, err)
	}
}

func TestCommandReadbackFailureDoesNotBlockIndependentReadyReply(t *testing.T) {
	ctx := context.Background()
	c, _, in, _ := appCommandFixture(t)
	in.Text = "/notify at status"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	g := &feedbackReadbackGateway{requestGateway: requestGateway{sendErr: &feishu.Unknown{Operation: "fixture"}}, chat: in.ChatID}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	in.MessageID = "later-command"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	g.readErr = errors.New("fixture GET failure")
	g.sendErr = nil
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote || g.calls != 2 || g.parent != in.MessageID {
		t.Fatal("readback starved later reply", wrote, err, g)
	}
}
