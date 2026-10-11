package sync

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type outboundGateway struct {
	s                  *store.Store
	t                  *testing.T
	calls              []string
	err                error
	missingID, closeDB bool
}

func (g *outboundGateway) dispatched(ctx context.Context, operation, target, card, uuid string) (string, error) {
	g.t.Helper()
	if operation != "update" {
		a, err := g.s.MirrorAttempt(ctx, uuid)
		if err != nil || a.State != "dispatching" || a.Operation != operation || a.Card != card {
			g.t.Fatalf("remote call before durable journal: %+v %v", a, err)
		}
		if operation == "reply" && a.ParentMessageID != target || operation == "send" && a.ChatID != target {
			g.t.Fatal("remote target differs from frozen dispatch")
		}
	} else {
		var journalled int
		err := g.s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_attempts WHERE operation='update' AND state='dispatching' AND target_message_id=? AND card_json=?`, target, card).Scan(&journalled)
		if err != nil || journalled != 1 {
			g.t.Fatalf("PATCH before durable journal: %d %v", journalled, err)
		}
	}
	g.calls = append(g.calls, operation)
	if g.closeDB {
		g.s.Close()
	}
	if g.missingID {
		return "", nil
	}
	return "remote-message", g.err
}

func (g *outboundGateway) SendCard(ctx context.Context, targetType, target, card, uuid string) (string, error) {
	if targetType != "chat_id" {
		g.t.Fatal("incorrect group target type")
	}
	return g.dispatched(ctx, "send", target, card, uuid)
}
func (g *outboundGateway) ReplyCard(ctx context.Context, parent, card, uuid string) (string, error) {
	return g.dispatched(ctx, "reply", parent, card, uuid)
}
func (g *outboundGateway) UpdateCard(ctx context.Context, target, card string) error {
	_, err := g.dispatched(ctx, "update", target, card, "")
	return err
}

func outboundFixture(t *testing.T, kind string) (*store.Store, string, store.Mirror) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bridge.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.BeginBaseline(ctx, "host", "app", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveNew(ctx, "host", "thread", "/project", 101); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEligible(ctx, "host", "thread", "user", "/project", "/project"); err != nil {
		t.Fatal(err)
	}
	i, err := s.PrepareGroup(ctx, "host", "thread", "owner", "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimGroupDispatch(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmGroup(ctx, i.ID, "chat"); err != nil {
		t.Fatal(err)
	}
	if err = s.BindConfirmedGroup(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	m := store.Mirror{Key: "document", TurnID: "turn", Kind: kind, Card: `{"body":"public text"}`}
	if kind != "human" {
		m.ParentMessageID = "original-input"
	}
	if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 102}); err != nil {
		t.Fatal(err)
	}
	return s, path, m
}

func TestOutboundOnlySendsOnceAndPatchesExistingCard(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"human", "progress", "final"} {
		t.Run(kind, func(t *testing.T) {
			s, _, m := outboundFixture(t, kind)
			g := &outboundGateway{s: s, t: t}
			o := Outbound{Store: s, Gateway: g}
			r, err := o.DispatchOne(ctx, "host", "thread", 1000)
			if err != nil || r.State != "confirmed" || r.MessageID != "remote-message" {
				t.Fatalf("delivery: %+v %v", r, err)
			}
			expect := "reply"
			if kind == "human" {
				expect = "send"
			}
			if g.calls[0] != expect {
				t.Fatalf("incorrect operation: %s", g.calls[0])
			}
			if r, err = o.DispatchOne(ctx, "host", "thread", 1001); err != nil || r.AttemptID != "" || len(g.calls) != 1 {
				t.Fatalf("duplicate dispatch: %+v %v", r, err)
			}
			m.Card = `{"body":"updated public text"}`
			if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 103}); err != nil {
				t.Fatal(err)
			}
			if r, err = o.DispatchOne(ctx, "host", "thread", 3000); err != nil || r.State != "confirmed" || g.calls[1] != "update" {
				t.Fatalf("second card created instead of update: %+v %v", r, err)
			}
		})
	}
}

func TestOutboundAmbiguousAndRejectedResultsAreFrozenWithoutFallback(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, state string
		err         error
		missingID   bool
	}{
		{"transport", "unknown", &feishu.Unknown{Operation: "reply"}, false},
		{"unclassified", "unknown", errors.New("connection lost"), false},
		{"missing id", "unknown", nil, true},
		{"rejected", "rejected", &feishu.Rejected{Code: 230002}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := outboundFixture(t, "final")
			g := &outboundGateway{s: s, t: t, err: tc.err, missingID: tc.missingID}
			o := Outbound{Store: s, Gateway: g}
			r, err := o.DispatchOne(ctx, "host", "thread", 1000)
			if err != nil || r.State != tc.state || len(g.calls) != 1 || g.calls[0] != "reply" {
				t.Fatalf("unsafe delivery/fallback: %+v %v %v", r, err, g.calls)
			}
			if r, err = o.DispatchOne(ctx, "host", "thread", 2000); err != nil || r.AttemptID != "" || len(g.calls) != 1 {
				t.Fatalf("unknown/rejected resent: %+v %v", r, err)
			}
		})
	}
}

func TestOutboundRemoteSuccessBeforeDatabaseFailureNeverResends(t *testing.T) {
	ctx := context.Background()
	s, path, _ := outboundFixture(t, "human")
	g := &outboundGateway{s: s, t: t, closeDB: true}
	o := Outbound{Store: s, Gateway: g}
	r, err := o.DispatchOne(ctx, "host", "thread", 1000)
	if err == nil || r.AttemptID == "" || r.State != "unknown" || len(g.calls) != 1 {
		t.Fatalf("commit failure was lost: %+v %v", r, err)
	}
	restarted, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err = restarted.RecoverMirrorDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	a, err := restarted.MirrorAttempt(ctx, r.AttemptID)
	if err != nil || a.State != "unknown" {
		t.Fatalf("accepted-but-unsaved request not frozen: %+v %v", a, err)
	}
	g.s, g.closeDB, o.Store = restarted, false, restarted
	if result, err := o.DispatchOne(ctx, "host", "thread", 5000); err != nil || result.AttemptID != "" || len(g.calls) != 1 {
		t.Fatalf("restart resent accepted request: %+v %v", result, err)
	}
}

func TestOutboundDatabaseFailurePreventsRemoteWrite(t *testing.T) {
	s, _, _ := outboundFixture(t, "human")
	g := &outboundGateway{s: s, t: t}
	s.Close()
	_, err := (Outbound{Store: s, Gateway: g}).DispatchOne(context.Background(), "host", "thread", 1000)
	if err == nil || len(g.calls) != 0 {
		t.Fatalf("remote write without local journal: %v %v", err, g.calls)
	}
}
