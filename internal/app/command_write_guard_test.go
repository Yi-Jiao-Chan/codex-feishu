package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type commandGuardGateway struct {
	*runtimeGateway
	reply func(context.Context, string, string, string) (string, error)
	calls int
}

func (g *commandGuardGateway) ReplyCard(ctx context.Context, parent, card, uuid string) (string, error) {
	g.calls++
	if g.reply != nil {
		return g.reply(ctx, parent, card, uuid)
	}
	return "remote-command", nil
}

type commandGuardScopeKey struct{}

func TestCommandWriteGuardRejectsFirstWithoutBlockingOrdinaryReply(t *testing.T) {
	c, s, in, _ := appCommandFixture(t)
	ctx := context.Background()
	in.UserID, in.ChatType, in.ChatID, in.Text = "ou_operator", "group", "chat", "/help"
	// Empty ThreadID still needs the hook: the chat can have a verification route.
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	first, _ := s.CommandReply(ctx, "host", "app", in.MessageID)
	in.MessageID, in.ChatID, in.ChatType = "ordinary-second", "private", "p2p"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	second, _ := s.CommandReply(ctx, "host", "app", in.MessageID)
	begins, releases := 0, 0
	c.BeginDelivery = func(parent context.Context, candidate store.CommandReply) (context.Context, func(), error) {
		begins++
		current, err := s.CommandReply(parent, candidate.HostID, candidate.AppID, candidate.MessageID)
		if err != nil || current != candidate || current.State != "ready" {
			t.Fatal("hook ran after claim", err)
		}
		if candidate.ID == first.ID {
			if candidate.ThreadID != "" || candidate.ChatID != "chat" {
				t.Fatal("hook lost chat identity")
			}
			return nil, func() { releases++ }, errors.New("PRIVATE fixture refused")
		}
		return context.WithValue(parent, commandGuardScopeKey{}, candidate.ID), func() { releases++ }, nil
	}
	g := &commandGuardGateway{runtimeGateway: &runtimeGateway{}, reply: func(ctx context.Context, parent, _ string, id string) (string, error) {
		if parent != second.MessageID || id != second.ID || ctx.Value(commandGuardScopeKey{}) != second.ID {
			t.Fatal("reply inherited wrong candidate scope")
		}
		return "remote-command", nil
	}}
	if wrote, err := c.DispatchOne(ctx, g); !wrote || err != nil || g.calls != 1 || begins != 2 || releases != 2 {
		t.Fatal("ordinary reply blocked", wrote, err, begins, releases)
	}
	current, err := s.CommandReply(ctx, first.HostID, first.AppID, first.MessageID)
	if err != nil || current != first {
		t.Fatal("refusal changed original candidate", err)
	}
	var attempts int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_reply_attempts WHERE message_id=?`, first.MessageID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal("refusal claimed attempt", err)
	}
	if wrote, err := c.DispatchOne(ctx, g); wrote || err == nil || strings.Contains(err.Error(), "PRIVATE") || g.calls != 1 {
		t.Fatal("all-refused result was missing/unsafe", wrote, err)
	}
}

func TestCommandWriteGuardCompetitionCannotUseGrantForAnotherReply(t *testing.T) {
	c, s, in, now := appCommandFixture(t)
	ctx := context.Background()
	in.Text = "/help"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	first, _ := s.CommandReply(ctx, "host", "app", in.MessageID)
	in.MessageID = "second"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	second, _ := s.CommandReply(ctx, "host", "app", in.MessageID)
	var granted []string
	releases := 0
	c.BeginDelivery = func(parent context.Context, candidate store.CommandReply) (context.Context, func(), error) {
		granted = append(granted, candidate.ID)
		if candidate.ID != first.ID {
			return nil, nil, errors.New("second not authorized")
		}
		completed := make(chan error, 1)
		go func() {
			r, claimed, err := s.ClaimCommandReplyCandidate(parent, candidate, now.UnixMilli())
			if err == nil && (!claimed || r.ID != first.ID) {
				err = errors.New("competing fixture claim failed")
			}
			completed <- err
		}()
		if err := <-completed; err != nil {
			t.Fatal(err)
		}
		return context.WithValue(parent, commandGuardScopeKey{}, candidate.ID), func() { releases++ }, nil
	}
	g := &commandGuardGateway{runtimeGateway: &runtimeGateway{}}
	if wrote, err := c.DispatchOne(ctx, g); wrote || err == nil || g.calls != 0 || releases != 1 || len(granted) != 2 || granted[1] != second.ID {
		t.Fatal("competition reused an old grant", wrote, err, granted)
	}
	current, err := s.CommandReply(ctx, second.HostID, second.AppID, second.MessageID)
	if err != nil || current != second {
		t.Fatal("ungranted reply partially claimed", err)
	}
	var attempts int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_reply_attempts`).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatal("extra competitive attempt", attempts, err)
	}
}

func TestCommandWriteGuardHoldsScopeThroughEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"confirmed", "unknown", "rate_limited", "finish_failure"} {
		t.Run(outcome, func(t *testing.T) {
			c, s, in, _ := appCommandFixture(t)
			ctx := context.Background()
			in.Text = "/help"
			if err := c.Handle(ctx, in); err != nil {
				t.Fatal(err)
			}
			original, _ := s.CommandReply(ctx, "host", "app", in.MessageID)
			if outcome == "finish_failure" {
				if _, err := s.DB.Exec(`CREATE TRIGGER command_finish_failure BEFORE UPDATE ON command_reply_attempts WHEN NEW.state='confirmed' BEGIN SELECT RAISE(FAIL,'fixture failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			var opctx context.Context
			releases := 0
			c.BeginDelivery = func(parent context.Context, candidate store.CommandReply) (context.Context, func(), error) {
				var cancel context.CancelFunc
				opctx, cancel = context.WithCancel(context.WithValue(parent, commandGuardScopeKey{}, candidate.ID))
				return opctx, func() {
					current, err := s.CommandReply(context.Background(), original.HostID, original.AppID, original.MessageID)
					want := map[string]string{"confirmed": "confirmed", "unknown": "unknown", "rate_limited": "ready", "finish_failure": "dispatching"}[outcome]
					if err != nil || current.State != want || opctx.Err() != nil {
						t.Fatal("lease released before outcome save", current.State, err)
					}
					releases++
					cancel()
				}, nil
			}
			g := &commandGuardGateway{runtimeGateway: &runtimeGateway{}, reply: func(scope context.Context, parent, card, id string) (string, error) {
				if scope != opctx || scope.Value(commandGuardScopeKey{}) != original.ID || releases != 0 || scope.Err() != nil || parent != original.MessageID || id != original.ID {
					t.Fatal("RPC lost lease or identity")
				}
				var wire, state string
				if err := s.DB.QueryRowContext(scope, `SELECT card_json,state FROM command_reply_attempts WHERE attempt_id=?`, id).Scan(&wire, &state); err != nil || state != "dispatching" || wire != card {
					t.Fatal("RPC preceded exact frozen claim", err)
				}
				switch outcome {
				case "unknown":
					return "", errors.New("reply lost")
				case "rate_limited":
					return "", &feishu.RateLimited{RetryAfter: time.Second}
				default:
					return "remote-command", nil
				}
			}}
			wrote, err := c.DispatchOne(ctx, g)
			if !wrote || g.calls != 1 || releases != 1 || (outcome == "finish_failure") != (err != nil) || !errors.Is(opctx.Err(), context.Canceled) {
				t.Fatal("scope completion failed", wrote, err, releases)
			}
			if outcome == "unknown" {
				if wrote, err := c.DispatchOne(ctx, g); wrote || err != nil || g.calls != 1 {
					t.Fatal("unknown command was resent", wrote, err)
				}
			}
		})
	}
}

func TestCommandWriteGuardCanceledScopeCannotClaimOrSend(t *testing.T) {
	c, s, in, _ := appCommandFixture(t)
	ctx := context.Background()
	in.Text = "/help"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	original, _ := s.CommandReply(ctx, "host", "app", in.MessageID)
	releases := 0
	c.BeginDelivery = func(parent context.Context, _ store.CommandReply) (context.Context, func(), error) {
		scope, cancel := context.WithCancel(parent)
		cancel()
		return scope, func() { releases++ }, nil
	}
	g := &commandGuardGateway{runtimeGateway: &runtimeGateway{}}
	if wrote, err := c.DispatchOne(ctx, g); wrote || err == nil || releases != 1 || g.calls != 0 {
		t.Fatal("canceled scope issued RPC", wrote, err)
	}
	current, err := s.CommandReply(ctx, "host", "app", in.MessageID)
	if err != nil || current != original {
		t.Fatal("canceled scope claimed candidate", err)
	}
	var attempts int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_reply_attempts`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal("canceled scope acquired attempt", err)
	}
}

func TestCommandWriteGuardClaimFailureReleasesWithoutPartialAttempt(t *testing.T) {
	c, s, in, _ := appCommandFixture(t)
	ctx := context.Background()
	in.Text = "/help"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	original, _ := s.CommandReply(ctx, "host", "app", in.MessageID)
	if _, err := s.DB.Exec(`CREATE TRIGGER command_claim_failure BEFORE INSERT ON command_reply_attempts BEGIN SELECT RAISE(FAIL,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	releases := 0
	c.BeginDelivery = func(parent context.Context, _ store.CommandReply) (context.Context, func(), error) {
		return parent, func() {
			releases++
			current, err := s.CommandReply(ctx, original.HostID, original.AppID, original.MessageID)
			if err != nil || current != original {
				t.Fatal("failed claim retained partial state", err)
			}
		}, nil
	}
	g := &commandGuardGateway{runtimeGateway: &runtimeGateway{}}
	if wrote, err := c.DispatchOne(ctx, g); wrote || err == nil || releases != 1 || g.calls != 0 {
		t.Fatal("failed claim crossed release/RPC boundary", wrote, err, releases)
	}
	var attempts int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_reply_attempts`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal("failed claim wrote partial attempt", attempts, err)
	}
}

func TestCommandWriteGuardCanceledAfterClaimKeepsConservativeUnknown(t *testing.T) {
	c, s, in, now := appCommandFixture(t)
	ctx := context.Background()
	in.Text = "/help"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	original, _ := s.CommandReply(ctx, "host", "app", in.MessageID)
	opctx, cancel := context.WithCancel(ctx)
	claimed, fresh, err := s.ClaimCommandReplyCandidate(opctx, original, now.UnixMilli())
	if err != nil || !fresh {
		t.Fatal(err)
	}
	cancel()
	g := &commandGuardGateway{runtimeGateway: &runtimeGateway{}}
	if wrote, err := c.dispatchClaimedReply(opctx, g, claimed); !wrote || !errors.Is(err, context.Canceled) || g.calls != 0 {
		t.Fatal("revoked claim called RPC", wrote, err)
	}
	if err := s.RecoverCommandDispatches(ctx, original.HostID); err != nil {
		t.Fatal(err)
	}
	current, err := s.CommandReply(ctx, original.HostID, original.AppID, original.MessageID)
	if err != nil || current.State != "unknown" {
		t.Fatal("revoked claim not recovered as unknown", err)
	}
	c.BeginDelivery = func(context.Context, store.CommandReply) (context.Context, func(), error) {
		t.Fatal("unknown candidate was reauthorized")
		return nil, nil, nil
	}
	if wrote, err := c.DispatchOne(ctx, g); wrote || err != nil || g.calls != 0 {
		t.Fatal("revoked claim was resent", wrote, err)
	}
}
