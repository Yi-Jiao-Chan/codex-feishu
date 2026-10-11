package store

import (
	"codex-feishu/internal/cards"
	"codex-feishu/internal/rules"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func requestFixture(t *testing.T, p rules.Policy, event time.Time) (*Store, string, MentionOptions, PendingRequest) {
	t.Helper()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	if _, err := s.InitNotificationPolicyAt(context.Background(), "app", "ou_user", p, event.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	m := Mirror{Key: "progress", TurnID: "turn", Kind: "progress", Card: cards.Progress("等待 Codex App 处理", "public"), ParentMessageID: "original-input"}
	stage(t, s, event.UnixMilli()+1000, m)
	options := MentionOptions{Enabled: true, Targets: []string{"ou_user"}, MentionOn: []string{"needs_user"}, ChatID: "chat", MemberIDs: []string{"ou_user"}}
	return s, path, options, PendingRequest{Key: "request", ThreadID: "thread", TurnID: "turn", Method: "item/tool/requestUserInput", EventAt: event.Unix()}
}
func observeRequest(t *testing.T, s *Store, r PendingRequest, o MentionOptions, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := s.ObserveAppRequest(ctx, "host", "app", r, o, at.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.StageRequestReminders(ctx, "host", "app", "thread"); err != nil {
		t.Fatal(err)
	}
}
func requestDecision(t *testing.T, s *Store) MentionDecision {
	t.Helper()
	d, e := s.RequestMentionDecisions(context.Background(), "host", "app", "thread", "request")
	if e != nil || len(d) != 1 {
		t.Fatalf("decisions %+v %v", d, e)
	}
	return d[0]
}
func claimRequest(t *testing.T, s *Store, o MentionOptions, at time.Time) RequestReminderAttempt {
	t.Helper()
	a, ok, e := s.ClaimRequestReminder(context.Background(), "host", "app", "thread", at.UnixMilli(), o)
	if e != nil || !ok {
		t.Fatalf("claim %t %v", ok, e)
	}
	return a
}

func TestRequestQuietEventNeverQueuesMorningReminder(t *testing.T) {
	ctx := context.Background()
	event := time.Date(2026, 10, 12, 16, 0, 0, 0, time.UTC)
	p := rules.Policy{Enabled: true, Timezone: "Asia/Shanghai", QuietStart: "23:00", QuietEnd: "08:00"}
	s, _, o, r := requestFixture(t, p, event)
	observeRequest(t, s, r, o, event)
	bindMirrorThread(t, s)
	if d := requestDecision(t, s); d.State != "suppressed" || d.Reason != "quiet" {
		t.Fatal(d)
	}
	if _, ok, e := s.ClaimRequestReminder(ctx, "host", "app", "thread", event.Add(9*time.Hour).UnixMilli(), o); e != nil || ok {
		t.Fatalf("morning backlog %t %v", ok, e)
	}
}
func TestRequestSendRechecksPolicyMembershipAndResolution(t *testing.T) {
	for _, variant := range []string{"off", "quiet", "missing_member", "wrong_chat", "resolved", "disabled", "removed"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			p, _ := rules.DefaultPolicy("UTC")
			event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
			s, _, o, r := requestFixture(t, p, event)
			observeRequest(t, s, r, o, event)
			bindMirrorThread(t, s)
			switch variant {
			case "off":
				p.Enabled = false
				p.Revision = 1
				if e := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, event.Add(time.Minute)); e != nil {
					t.Fatal(e)
				}
			case "quiet":
				p.QuietStart = "10:01"
				p.QuietEnd = "11:00"
				p.Revision = 1
				if e := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, event.Add(time.Minute)); e != nil {
					t.Fatal(e)
				}
			case "missing_member":
				o.MemberIDs = nil
			case "wrong_chat":
				o.ChatID = "other"
			case "resolved":
				if e := s.ResolveAppRequest(ctx, "host", "app", "thread", r.Key); e != nil {
					t.Fatal(e)
				}
			case "disabled":
				o.Enabled = false
			case "removed":
				o.Targets = nil
			}
			if _, ok, e := s.ClaimRequestReminder(ctx, "host", "app", "thread", event.Add(2*time.Minute).UnixMilli(), o); e != nil || ok {
				t.Fatalf("unsafe claim %t %v", ok, e)
			}
			if d := requestDecision(t, s); d.State != "suppressed" {
				t.Fatal(d)
			}
			o.MemberIDs = []string{"ou_user"}
			o.ChatID = "chat"
			o.Enabled = true
			o.Targets = []string{"ou_user"}
			observeRequest(t, s, r, o, event.Add(2*time.Hour))
			if _, ok, e := s.ClaimRequestReminder(ctx, "host", "app", "thread", event.Add(2*time.Hour).UnixMilli(), o); e != nil || ok {
				t.Fatalf("suppression resurrected %t %v", ok, e)
			}
		})
	}
}
func TestRequestUnknownReplayAndFirstConfigurationFreeze(t *testing.T) {
	for _, variant := range []string{"replay", "unknown", "disabled", "empty_targets", "event_disabled"} {
		t.Run(variant, func(t *testing.T) {
			p, _ := rules.DefaultPolicy("UTC")
			event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
			s, _, o, r := requestFixture(t, p, event)
			first := o
			switch variant {
			case "replay":
				r.Replay = true
			case "unknown":
				r.EventAt = 0
			case "disabled":
				first.Enabled = false
			case "empty_targets":
				first.Targets = nil
			case "event_disabled":
				first.MentionOn = []string{"completed"}
			}
			observeRequest(t, s, r, first, event)
			r.Replay = false
			r.EventAt = event.Add(time.Hour).Unix()
			observeRequest(t, s, r, o, event.Add(time.Hour))
			bindMirrorThread(t, s)
			if _, ok, e := s.ClaimRequestReminder(context.Background(), "host", "app", "thread", event.Add(time.Hour).UnixMilli(), o); e != nil || ok {
				t.Fatalf("replay acquired eligibility %t %v", ok, e)
			}
		})
	}
}
func TestRequestJournalReopenUnknownNeverResends(t *testing.T) {
	ctx := context.Background()
	p, _ := rules.DefaultPolicy("UTC")
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	for _, outcome := range []string{"crash", "unknown", "rejected", "confirmed"} {
		t.Run(outcome, func(t *testing.T) {
			s, path, o, r := requestFixture(t, p, event)
			observeRequest(t, s, r, o, event)
			bindMirrorThread(t, s)
			a := claimRequest(t, s, o, event.Add(time.Minute))
			if a.ParentMessageID != "original-input" || !hasMention(a.Card) || !strings.Contains(a.Card, "Codex App") {
				t.Fatalf("wrong reply %+v", a)
			}
			if outcome != "crash" {
				remote := ""
				if outcome == "confirmed" {
					remote = "remote"
				}
				if e := s.FinishRequestReminder(ctx, a.ID, outcome, remote); e != nil {
					t.Fatal(e)
				}
			}
			s.Close()
			var e error
			s, e = Open(path)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			if e = s.RecoverRequestReminders(ctx, "host"); e != nil {
				t.Fatal(e)
			}
			r.Replay = true
			r.EventAt = 0
			observeRequest(t, s, r, o, event.Add(time.Hour))
			if _, ok, e := s.ClaimRequestReminder(ctx, "host", "app", "thread", event.Add(time.Hour).UnixMilli(), o); e != nil || ok {
				t.Fatalf("recovery resent %t %v", ok, e)
			}
			var card, parent string
			if e = s.DB.QueryRow(`SELECT card_json,parent_message_id FROM request_reminder_attempts WHERE attempt_id=?`, a.ID).Scan(&card, &parent); e != nil || card != a.Card || parent != a.ParentMessageID {
				t.Fatalf("frozen payload lost %v", e)
			}
		})
	}
}
func TestRequestRequiresConfirmedInputAndScope(t *testing.T) {
	ctx := context.Background()
	p, _ := rules.DefaultPolicy("UTC")
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	s, _, o, r := requestFixture(t, p, event)
	if _, e := s.DB.Exec(`UPDATE mirrors SET parent_key='human',parent_message_id=''`); e != nil {
		t.Fatal(e)
	}
	stage(t, s, event.UnixMilli()+2000, Mirror{Key: "human", TurnID: "turn", Kind: "human", Terminal: true, Card: cards.Message("人类", "Codex", "public")})
	observeRequest(t, s, r, o, event)
	bindMirrorThread(t, s)
	if _, ok, e := s.ClaimRequestReminder(ctx, "host", "app", "thread", event.Add(time.Minute).UnixMilli(), o); e != nil || ok {
		t.Fatalf("unconfirmed parent %t %v", ok, e)
	}
	a := claimMirror(t, s, "human", event.Add(time.Minute).UnixMilli())
	if e := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "native-input"); e != nil {
		t.Fatal(e)
	}
	reminder := claimRequest(t, s, o, event.Add(2*time.Minute))
	if reminder.ParentMessageID != "native-input" {
		t.Fatal(reminder)
	}
	if e := s.ObserveAppRequest(ctx, "host", "foreign", r, o, event.UnixMilli()); !errors.Is(e, ErrConflict) {
		t.Fatalf("foreign app %v", e)
	}
}
func TestRequestTransactionsRollbackEffectsAndPreserveFrozenUnknown(t *testing.T) {
	ctx := context.Background()
	p, _ := rules.DefaultPolicy("UTC")
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	s, _, o, r := requestFixture(t, p, event)
	if _, e := s.DB.Exec(`CREATE TRIGGER reject_request_mention BEFORE INSERT ON request_mentions BEGIN SELECT RAISE(ABORT,'fixture'); END`); e != nil {
		t.Fatal(e)
	}
	if e := s.ObserveAppRequest(ctx, "host", "app", r, o, event.UnixMilli()); e == nil {
		t.Fatal("partial observation accepted")
	}
	var count int
	s.DB.QueryRow(`SELECT COUNT(*) FROM app_requests`).Scan(&count)
	if count != 0 {
		t.Fatal("partial observation retained")
	}
	s.DB.Exec(`DROP TRIGGER reject_request_mention`)
	observeRequest(t, s, r, o, event)
	bindMirrorThread(t, s)
	s.DB.Exec(`CREATE TRIGGER reject_request_attempt BEFORE INSERT ON request_reminder_attempts BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	if _, ok, e := s.ClaimRequestReminder(ctx, "host", "app", "thread", event.Add(time.Minute).UnixMilli(), o); e == nil || ok {
		t.Fatal("partial claim accepted")
	}
	if d := requestDecision(t, s); d.State != "eligible" || d.AttemptID != "" {
		t.Fatal(d)
	}
	s.DB.Exec(`DROP TRIGGER reject_request_attempt`)
	a := claimRequest(t, s, o, event.Add(time.Minute))
	s.DB.Exec(`CREATE TRIGGER reject_request_finish BEFORE UPDATE ON app_requests BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	if e := s.FinishRequestReminder(ctx, a.ID, "confirmed", "remote"); e == nil {
		t.Fatal("partial finish accepted")
	}
	var state, card string
	s.DB.QueryRow(`SELECT state,card_json FROM request_reminder_attempts WHERE attempt_id=?`, a.ID).Scan(&state, &card)
	if state != "dispatching" || card != a.Card {
		t.Fatal("failed confirmation discarded frozen journal")
	}
}

func TestRequestAuditOffOnBeforeDelayedObservationAndTerminalRecovery(t *testing.T) {
	ctx := context.Background()
	p, _ := rules.DefaultPolicy("UTC")
	event := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	s, _, o, r := requestFixture(t, p, event)
	p.Enabled = false
	p.Revision = 1
	if e := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, event); e != nil {
		t.Fatal(e)
	}
	p.Enabled = true
	p.Revision = 2
	if e := s.SetNotificationPolicyAt(ctx, "app", "ou_user", 1, p, event.Add(500*time.Millisecond)); e != nil {
		t.Fatal(e)
	}
	observeRequest(t, s, r, o, event.Add(time.Hour))
	if d := requestDecision(t, s); d.Reason != "off" || d.State != "suppressed" {
		t.Fatal("off/on lost original suppression", d)
	}
	if e := s.RecoverRequestReminders(ctx, "host"); e != nil {
		t.Fatal(e)
	}
	if e := s.ReconcileAppRequests(ctx, "host", "app", "thread", []string{"turn"}); e != nil {
		t.Fatal(e)
	}
	var state string
	if e := s.DB.QueryRow(`SELECT state FROM app_requests WHERE request_key='request'`).Scan(&state); e != nil || state != "resolved" {
		t.Fatal("unreplayed terminal request retained", state, e)
	}
}
