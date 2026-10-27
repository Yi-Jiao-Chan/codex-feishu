package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

func testCardHash(card string) string {
	sum := sha256.Sum256([]byte(card))
	return hex.EncodeToString(sum[:])
}

type outboundGateway struct {
	s                  *store.Store
	t                  *testing.T
	calls              []string
	cards              []string
	err                error
	errs               []error
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
	g.cards = append(g.cards, card)
	if g.closeDB {
		g.s.Close()
	}
	if g.missingID {
		return "", nil
	}
	err := g.err
	if len(g.errs) >= len(g.calls) {
		err = g.errs[len(g.calls)-1]
	}
	return "remote-message", err
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
	if kind == "final" {
		m.Terminal = true
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

func TestOutboundCorrectedFinalRetriesOnceThenSendsFailureNotice(t *testing.T) {
	ctx := context.Background()
	s, _, m := outboundFixture(t, "final")
	firstRejection := &feishu.Rejected{Code: 230099, HTTPStatus: 400, RequestID: "req-first"}
	retryRejection := &feishu.Rejected{Code: 230099, HTTPStatus: 400, RequestID: "req-retry"}
	g := &outboundGateway{s: s, t: t, errs: []error{firstRejection, retryRejection, nil}}
	o := Outbound{Store: s, Gateway: g}

	first, err := o.DispatchOne(ctx, "host", "thread", 1000)
	if err != nil || first.State != "rejected" || len(g.calls) != 1 {
		t.Fatalf("initial definite rejection: %+v %v calls=%v", first, err, g.calls)
	}
	if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 103}); err != nil {
		t.Fatal(err)
	}
	if result, err := o.DispatchOne(ctx, "host", "thread", 1500); err != nil || result.AttemptID != "" || len(g.calls) != 1 {
		t.Fatalf("identical rejected payload was retried: %+v %v calls=%v", result, err, g.calls)
	}

	m.Card = `{"body":"corrected public text"}`
	if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 104}); err != nil {
		t.Fatal(err)
	}
	retry, err := o.DispatchOne(ctx, "host", "thread", 2000)
	if err != nil || retry.State != "rejected" || len(g.calls) != 2 {
		t.Fatalf("corrected final was not retried once: %+v %v calls=%v", retry, err, g.calls)
	}
	attempt, err := s.MirrorAttempt(ctx, retry.AttemptID)
	if err != nil || attempt.RetryOfAttemptID != first.AttemptID || attempt.RejectionCode != 230099 ||
		attempt.RejectionHTTPStatus != 400 || attempt.RejectionRequestID != "req-retry" {
		t.Fatalf("repair/rejection evidence was not persisted: %+v %v", attempt, err)
	}

	notice, err := o.DispatchOne(ctx, "host", "thread", 2001)
	if err != nil || notice.State != "confirmed" || notice.Kind != "progress" ||
		!strings.HasPrefix(notice.Key, "ui:delivery-failure:") || len(g.calls) != 3 ||
		!strings.Contains(g.cards[2], "最终回复发送失败") || !strings.Contains(g.cards[2], "req-retry") ||
		strings.Contains(g.cards[2], "corrected public text") {
		t.Fatalf("failure notice was not isolated and delivered: %+v %v card=%q", notice, err, g.cards[2])
	}
	if result, err := o.DispatchOne(ctx, "host", "thread", 3000); err != nil || result.AttemptID != "" || len(g.calls) != 3 {
		t.Fatalf("repair or failure notice recursed: %+v %v calls=%v", result, err, g.calls)
	}
}

func TestOutboundCorrectedFinalSuccessDoesNotSendFailureNotice(t *testing.T) {
	ctx := context.Background()
	s, _, m := outboundFixture(t, "final")
	g := &outboundGateway{s: s, t: t, errs: []error{&feishu.Rejected{Code: 230099, HTTPStatus: 400}, nil}}
	o := Outbound{Store: s, Gateway: g}
	first, err := o.DispatchOne(ctx, "host", "thread", 1000)
	if err != nil || first.State != "rejected" {
		t.Fatalf("initial rejection: %+v %v", first, err)
	}
	m.Card = `{"body":"corrected public text"}`
	if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 103}); err != nil {
		t.Fatal(err)
	}
	retry, err := o.DispatchOne(ctx, "host", "thread", 2000)
	if err != nil || retry.State != "confirmed" || len(g.calls) != 2 {
		t.Fatalf("corrected final did not confirm: %+v %v calls=%v", retry, err, g.calls)
	}
	if result, err := o.DispatchOne(ctx, "host", "thread", 2001); err != nil || result.AttemptID != "" || len(g.calls) != 2 {
		t.Fatalf("successful repair created a failure notice: %+v %v calls=%v", result, err, g.calls)
	}
	var failures int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM mirrors WHERE mirror_key LIKE 'ui:delivery-failure:%'`).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("successful repair persisted failure mirrors: %d %v", failures, err)
	}
}

func TestOutboundRejectedFailureNoticeDoesNotRecurse(t *testing.T) {
	ctx := context.Background()
	s, _, m := outboundFixture(t, "final")
	rejected := &feishu.Rejected{Code: 230099, HTTPStatus: 400}
	g := &outboundGateway{s: s, t: t, errs: []error{rejected, rejected, rejected}}
	o := Outbound{Store: s, Gateway: g}
	if result, err := o.DispatchOne(ctx, "host", "thread", 1000); err != nil || result.State != "rejected" {
		t.Fatalf("initial rejection: %+v %v", result, err)
	}
	m.Card = `{"body":"corrected public text"}`
	if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 103}); err != nil {
		t.Fatal(err)
	}
	if result, err := o.DispatchOne(ctx, "host", "thread", 2000); err != nil || result.State != "rejected" {
		t.Fatalf("repair rejection: %+v %v", result, err)
	}
	if result, err := o.DispatchOne(ctx, "host", "thread", 2001); err != nil || result.State != "rejected" || !strings.HasPrefix(result.Key, "ui:delivery-failure:") {
		t.Fatalf("failure-notice rejection: %+v %v", result, err)
	}
	if result, err := o.DispatchOne(ctx, "host", "thread", 3000); err != nil || result.AttemptID != "" || len(g.calls) != 3 {
		t.Fatalf("rejected failure notice recursed: %+v %v calls=%v", result, err, g.calls)
	}
	var failures int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM mirrors WHERE mirror_key LIKE 'ui:delivery-failure:%'`).Scan(&failures); err != nil || failures != 1 {
		t.Fatalf("failure notice count=%d err=%v", failures, err)
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
