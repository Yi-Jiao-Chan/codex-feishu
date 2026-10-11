package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

func TestRequestTrackerPreservesAppOwnershipAndTypedRequestIdentity(t *testing.T) {
	tracker := &RequestTracker{}
	event := func(method, thread, turn, id string) codexclient.Event {
		b, _ := json.Marshal(map[string]any{"threadId": thread, "turnId": turn, "requestId": json.RawMessage(id)})
		return codexclient.Event{Method: method, Params: b, Generation: 3, RequestID: json.RawMessage(id)}
	}
	numeric := event("item/tool/requestUserInput", "thread", "turn", "19")
	text := event("item/commandExecution/requestApproval", "thread", "turn", "\"19\"")
	tracker.Observe(numeric)
	tracker.Observe(numeric)
	tracker.Observe(text)
	if !tracker.Waiting("thread", "turn") || len(tracker.requests) != 2 {
		t.Fatal("request replay or typed ID lost")
	}
	tracker.Observe(event("item/agentMessage/delta", "thread", "turn", "19"))
	tracker.Observe(event("serverRequest/resolved", "other-thread", "", "19"))
	if !tracker.Waiting("thread", "turn") {
		t.Fatal("ordinary output/foreign resolution answered request")
	}
	tracker.Observe(event("serverRequest/resolved", "thread", "", "19"))
	if !tracker.Waiting("thread", "turn") || len(tracker.requests) != 1 {
		t.Fatal("numeric resolution consumed string request")
	}
	tracker.Reconcile("thread", []codexclient.Turn{{ID: "turn", Status: "inProgress"}})
	if !tracker.Waiting("thread", "turn") {
		t.Fatal("nonterminal snapshot cleared pending")
	}
	tracker.Reconcile("thread", []codexclient.Turn{{ID: "turn", Status: "interrupted"}})
	if tracker.Waiting("thread", "turn") {
		t.Fatal("cancelled turn retained pending request")
	}
	tracker.Observe(numeric)
	newGeneration := &RequestTracker{}
	if newGeneration.Waiting("thread", "turn") {
		t.Fatal("new connection reused stale request IDs")
	}
}

func durableTrackerFixture(t *testing.T) (*RequestTracker, *time.Time, store.MentionOptions) {
	t.Helper()
	now := time.Date(2026, 10, 12, 10, 0, 30, 0, time.UTC)
	s, e := store.Open(filepath.Join(t.TempDir(), "requests.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	if _, e = s.BeginBaseline(ctx, "host", "app", now.Add(-time.Hour).UnixMilli()); e != nil {
		t.Fatal(e)
	}
	if e = s.ObserveNew(ctx, "host", "thread", "/project", now.Add(-30*time.Second).Unix()); e != nil {
		t.Fatal(e)
	}
	if e = s.MarkEligible(ctx, "host", "thread", "input", "/project", "/project"); e != nil {
		t.Fatal(e)
	}
	p, _ := rules.DefaultPolicy("UTC")
	if _, e = s.InitNotificationPolicyAt(ctx, "app", "ou_user", p, now.Add(-time.Hour)); e != nil {
		t.Fatal(e)
	}
	o := store.MentionOptions{Enabled: true, Targets: []string{"ou_user"}, MentionOn: []string{"needs_user"}}
	return &RequestTracker{Store: s, HostID: "host", AppID: "app", Options: o, Now: func() time.Time { return now }, GenerationStartedAtMS: now.Add(-31 * time.Second).UnixMilli()}, &now, o
}
func routedEvent(method, id string, generation uint64) codexclient.Event {
	b, _ := json.Marshal(map[string]any{"threadId": "thread", "turnId": "turn", "requestId": json.RawMessage(id), "command": "SECRET-COMMAND", "questions": []string{"SECRET-QUESTION"}, "answers": "SECRET-ANSWER"})
	return codexclient.Event{Method: method, Generation: generation, RequestID: json.RawMessage(id), Params: b}
}
func trackerDecision(t *testing.T, tr *RequestTracker, id string) store.MentionDecision {
	t.Helper()
	d, e := tr.Store.RequestMentionDecisions(context.Background(), "host", "app", "thread", requestRoutingKey("turn", json.RawMessage(id)))
	if e != nil || len(d) != 1 {
		t.Fatalf("decision %+v %v", d, e)
	}
	return d[0]
}
func trackerTurn(now time.Time) codexclient.Turn {
	at := now.Add(-20 * time.Second).Unix()
	return codexclient.Turn{ID: "turn", Status: "inProgress", StartedAt: &at}
}
func stageTrackerProgress(t *testing.T, tr *RequestTracker, now time.Time) {
	t.Helper()
	if e := tr.Store.StageMirrors(context.Background(), "host", "thread", []store.Mirror{{Key: "progress", TurnID: "turn", Kind: "progress", Card: cards.Progress("等待 Codex App", "public"), ParentMessageID: "original-input"}}, store.Watermark{ObservedAtMS: now.UnixMilli(), TurnID: "turn"}); e != nil {
		t.Fatal(e)
	}
}
func bindTracker(t *testing.T, tr *RequestTracker) {
	t.Helper()
	ctx := context.Background()
	i, e := tr.Store.PrepareGroup(ctx, "host", "thread", "owner", "fixture", nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tr.Store.ClaimGroupDispatch(ctx, i.ID); e != nil {
		t.Fatal(e)
	}
	if e = tr.Store.ConfirmGroup(ctx, i.ID, "chat"); e != nil {
		t.Fatal(e)
	}
	if e = tr.Store.BindConfirmedGroup(ctx, i.ID); e != nil {
		t.Fatal(e)
	}
}

func TestRequestTrackerProvesCurrentBirthFirstTurnWithoutRecordingSecrets(t *testing.T) {
	ctx := context.Background()
	tr, now, _ := durableTrackerFixture(t)
	birth, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": "thread", "createdAt": now.Add(-30 * time.Second).Unix()}})
	if e := tr.ObserveContext(ctx, codexclient.Event{Method: "thread/started", Generation: 1, Params: birth}); e != nil {
		t.Fatal(e)
	}
	if e := tr.ObserveContext(ctx, routedEvent("item/commandExecution/requestApproval", "19", 1)); e != nil {
		t.Fatal(e)
	}
	stageTrackerProgress(t, tr, *now)
	if e := tr.Hydrated(ctx, "thread", []codexclient.Turn{trackerTurn(*now)}); e != nil {
		t.Fatal(e)
	}
	if d := trackerDecision(t, tr, "19"); d.State != "eligible" {
		t.Fatalf("new first turn lost eligibility %+v", d)
	}
	*now = now.Add(time.Second)
	if e := tr.ObserveContext(ctx, routedEvent("item/tool/requestUserInput", "20", 1)); e != nil {
		t.Fatal(e)
	}
	if d := trackerDecision(t, tr, "20"); d.State != "eligible" {
		t.Fatalf("first-turn request after hydration lost eligibility %+v", d)
	}
	var persisted string
	if e := tr.Store.DB.QueryRow(`SELECT group_concat(request_key||thread_id||turn_id||method) FROM app_requests`).Scan(&persisted); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(persisted, "SECRET") {
		t.Fatal("native contents persisted")
	}
	if !tr.Waiting("thread", "turn") {
		t.Fatal("process wait state absent")
	}
	if e := tr.ObserveContext(ctx, routedEvent("serverRequest/resolved", "19", 1)); e != nil {
		t.Fatal(e)
	}
	if d := trackerDecision(t, tr, "19"); d.State != "suppressed" || d.Reason != "resolved" {
		t.Fatal(d)
	}
	if !tr.Waiting("thread", "turn") {
		t.Fatal("one resolution cleared other request")
	}
}
func TestRequestTrackerReconnectAndUnverifiedBirthNeverQualify(t *testing.T) {
	for _, variant := range []string{"resume", "mismatched_birth", "baseline", "old_birth_projection", "generation_second"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			tr, now, _ := durableTrackerFixture(t)
			if variant == "old_birth_projection" {
				tr.GenerationStartedAtMS = now.UnixMilli()
			}
			if variant == "generation_second" {
				tr.GenerationStartedAtMS = now.Add(-30 * time.Second).UnixMilli()
			}
			if variant != "resume" {
				created := now.Add(-30 * time.Second).Unix()
				if variant == "mismatched_birth" {
					created--
				}
				if variant == "baseline" {
					tr.Store.DB.Exec(`UPDATE thread_observations SET origin='baseline',proof='baseline'`)
				}
				b, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": "thread", "createdAt": created}})
				tr.Observe(codexclient.Event{Method: "thread/started", Generation: 2, Params: b})
			}
			if e := tr.ObserveContext(ctx, routedEvent("item/tool/requestUserInput", "19", 2)); e != nil {
				t.Fatal(e)
			}
			stageTrackerProgress(t, tr, *now)
			if e := tr.Hydrated(ctx, "thread", []codexclient.Turn{trackerTurn(*now)}); e != nil {
				t.Fatal(e)
			}
			if d := trackerDecision(t, tr, "19"); d.State != "suppressed" || d.Reason != "replay" {
				t.Fatal(d)
			}
			if !tr.Waiting("thread", "turn") {
				t.Fatal("replay must still display waiting")
			}
		})
	}
}
func TestRequestTrackerNewTurnAfterHydrationQualifiesAndTypedIDStaysDistinct(t *testing.T) {
	ctx := context.Background()
	tr, now, _ := durableTrackerFixture(t)
	if e := tr.Hydrated(ctx, "thread", nil); e != nil {
		t.Fatal(e)
	}
	*now = now.Add(3 * time.Second)
	start := now.Unix()
	b, _ := json.Marshal(map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "startedAt": start}})
	tr.Observe(codexclient.Event{Method: "turn/started", Generation: 1, Params: b})
	for _, id := range []string{"19", "\"19\""} {
		if e := tr.ObserveContext(ctx, routedEvent("item/tool/requestUserInput", id, 1)); e != nil {
			t.Fatal(e)
		}
		if d := trackerDecision(t, tr, id); d.State != "eligible" {
			t.Fatal(d)
		}
	}
	if requestRoutingKey("turn", json.RawMessage("19")) == requestRoutingKey("turn", json.RawMessage("\"19\"")) {
		t.Fatal("typed routing IDs collapsed")
	}
	terminal := []codexclient.Turn{{ID: "turn", Status: "interrupted"}}
	if e := tr.ReconcileContext(ctx, "thread", terminal); e != nil {
		t.Fatal(e)
	}
	if tr.Waiting("thread", "turn") {
		t.Fatal("terminal state retained wait")
	}
}

type requestGateway struct {
	calls              int
	parent, card, uuid string
	sendErr            error
	afterRPC           func()
}

func (g *requestGateway) ReplyCard(_ context.Context, parent, card, uuid string) (string, error) {
	g.calls++
	g.parent, g.card, g.uuid = parent, card, uuid
	if g.afterRPC != nil {
		g.afterRPC()
	}
	if g.sendErr != nil {
		return "", g.sendErr
	}
	return "remote-request", nil
}
func (*requestGateway) SendCard(context.Context, string, string, string, string) (string, error) {
	return "", errors.New("unexpected send")
}
func (*requestGateway) UpdateCard(context.Context, string, string) error {
	return errors.New("unexpected update")
}
func (*requestGateway) MemberIDs(context.Context, string) ([]string, error) {
	return []string{"ou_user"}, nil
}
func TestRequestTrackerDispatchFreezesExactReplyAndDoesNotRetryUnknown(t *testing.T) {
	for _, variant := range []string{"confirmed", "unknown", "rejected", "commit_failure"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			tr, now, _ := durableTrackerFixture(t)
			if e := tr.Hydrated(ctx, "thread", nil); e != nil {
				t.Fatal(e)
			}
			*now = now.Add(3 * time.Second)
			at := now.Unix()
			tr.started = map[string]map[string]int64{"thread": {"turn": at}}
			if e := tr.ObserveContext(ctx, routedEvent("item/tool/requestUserInput", "19", 1)); e != nil {
				t.Fatal(e)
			}
			stageTrackerProgress(t, tr, *now)
			if e := tr.Hydrated(ctx, "thread", []codexclient.Turn{{ID: "turn", Status: "inProgress", StartedAt: &at}}); e != nil {
				t.Fatal(e)
			}
			bindTracker(t, tr)
			g := &requestGateway{}
			switch variant {
			case "unknown":
				g.sendErr = errors.New("fixture timeout")
			case "rejected":
				g.sendErr = &feishu.Rejected{Code: 1}
			case "commit_failure":
				g.afterRPC = func() {
					tr.Store.DB.Exec(`CREATE TRIGGER fail_request_finish BEFORE UPDATE ON app_requests BEGIN SELECT RAISE(ABORT,'fixture'); END`)
				}
			}
			tr.Gateway, tr.Membership = g, g
			wrote, e := tr.DispatchOne(ctx, "thread")
			if !wrote || (e != nil) != (variant == "commit_failure") {
				t.Fatalf("dispatch %t %v", wrote, e)
			}
			if g.calls != 1 || g.parent != "original-input" || g.uuid == "" || !strings.Contains(g.card, `\u003cat id=`) || strings.Contains(g.card, "SECRET") {
				t.Fatalf("reply evidence %+v", g)
			}
			if variant == "commit_failure" {
				tr.Store.DB.Exec(`DROP TRIGGER fail_request_finish`)
			}
			if e := tr.Store.RecoverRequestReminders(ctx, "host"); e != nil {
				t.Fatal(e)
			}
			if wrote, e := tr.DispatchOne(ctx, "thread"); e != nil || wrote || g.calls != 1 {
				t.Fatalf("unsafe retry %t %v calls=%d", wrote, e, g.calls)
			}
			var frozen, uuid string
			if e := tr.Store.DB.QueryRow(`SELECT card_json,attempt_id FROM request_reminder_attempts`).Scan(&frozen, &uuid); e != nil || frozen != g.card || uuid != g.uuid {
				t.Fatal("payload journal mismatch", e)
			}
		})
	}
}
