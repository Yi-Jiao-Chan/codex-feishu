package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type topicGateway struct {
	rootReads                       int
	rootErr                         error
	rootDeleted, rootMissingDeleted bool
	*outboundGateway
	parents []string
}

func (g *topicGateway) ReadCardMessage(ctx context.Context, id string) (feishu.CardMessage, error) {
	g.rootReads++
	if g.rootErr != nil {
		return feishu.CardMessage{}, g.rootErr
	}
	var chat, platform string
	if err := g.s.DB.QueryRowContext(ctx, `SELECT chat_id,platform_thread_id FROM bindings WHERE host_id='host' AND root_message_id=?`, id).Scan(&chat, &platform); err != nil {
		return feishu.CardMessage{}, err
	}
	root := feishu.CardMessage{ID: id, ChatID: chat, RootID: &id, ThreadID: &platform, Deleted: &g.rootDeleted}
	if g.rootMissingDeleted {
		root.Deleted = nil
	}
	return root, nil
}
func (g *topicGateway) ReplyTopicCard(ctx context.Context, parent, card, uuid string) (string, error) {
	g.parents = append(g.parents, parent)
	return g.dispatched(ctx, "reply", parent, card, uuid)
}
func makeTopic(t *testing.T, s *store.Store) {
	t.Helper()
	if _, err := s.DB.Exec(`UPDATE bindings SET follow_mode='topic',root_message_id='root',platform_thread_id='platform' WHERE host_id='host' AND thread_id='thread'`); err != nil {
		t.Fatal(err)
	}
}
func TestTopicOutboundUsesRootForEveryNewCardAndStopsOnRepair(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"human", "progress", "final"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := outboundFixture(t, kind)
			makeTopic(t, s)
			g := &topicGateway{outboundGateway: &outboundGateway{s: s, t: t, err: &feishu.Rejected{Code: 230011}}}
			o := Outbound{Store: s, Gateway: g}
			r, err := o.DispatchOne(ctx, "host", "thread", 1000)
			if err != nil || r.State != "rejected" || len(g.parents) != 1 || g.parents[0] != "root" {
				t.Fatal(r, err, g.parents)
			}
			a, err := s.MirrorAttempt(ctx, r.AttemptID)
			if err != nil || a.FollowMode != "topic" || a.Operation != "reply" || a.ParentMessageID != "root" || a.RootMessageID != "root" || a.PlatformThreadID != "platform" {
				t.Fatal("topic wire not journalled", a, err)
			}
			b, err := s.Binding(ctx, "host", "thread")
			if err != nil || !b.RepairRequired {
				t.Fatal("deleted root not quarantined", b, err)
			}
			if err = s.StageMirrors(ctx, "host", "thread", []store.Mirror{{Key: "another", TurnID: "turn", Kind: "human", Card: `{"body":"later"}`}}, store.Watermark{ObservedAtMS: 2000}); err != nil {
				t.Fatal(err)
			}
			if _, err = o.DispatchOne(ctx, "host", "thread", 3000); !errors.Is(err, store.ErrTargetRepair) || len(g.calls) != 1 {
				t.Fatal("repair target sent more content", err, g.calls)
			}
		})
	}
}
func TestTopicGatewayAbsenceCannotFallBackToGroup(t *testing.T) {
	s, _, _ := outboundFixture(t, "human")
	makeTopic(t, s)
	g := &outboundGateway{s: s, t: t}
	r, err := (Outbound{Store: s, Gateway: g}).DispatchOne(context.Background(), "host", "thread", 1000)
	if err == nil || r.AttemptID != "" || len(g.calls) != 0 {
		t.Fatal(r, err, g.calls)
	}
}
func TestTopicReadbackRequiresExactRootAndThread(t *testing.T) {
	root, thread, parent := "root", "platform", "root"
	deleted := false
	card := `{"body":{"elements":[{"tag":"div","element_id":"marker"}]}}`
	proof := CardReadbackProof{FollowMode: "topic", RootMessageID: root, PlatformThreadID: thread, AppID: "app", ChatID: "chat", ParentMessageID: parent, Card: card, Marker: "marker"}
	m := feishu.CardMessage{ID: "message", ChatID: "chat", Type: "interactive", SenderID: "app", SenderIDType: "app_id", SenderType: "app", ParentID: &parent, RootID: &root, ThreadID: &thread, Deleted: &deleted, Content: card}
	if !matchesCardReadback(proof, m) {
		t.Fatal("valid topic card rejected")
	}
	wrong := "other"
	m.RootID = &wrong
	if matchesCardReadback(proof, m) {
		t.Fatal("other root confirmed")
	}
	m.RootID = &root
	m.ThreadID = &wrong
	if matchesCardReadback(proof, m) {
		t.Fatal("other topic confirmed")
	}
	m.ThreadID = nil
	if matchesCardReadback(proof, m) {
		t.Fatal("missing topic evidence confirmed")
	}
}

func TestTopicRateLimitKeepsTargetHealthyAndRetryInsideTopic(t *testing.T) {
	ctx := context.Background()
	s, _, _ := outboundFixture(t, "human")
	makeTopic(t, s)
	g := &topicGateway{outboundGateway: &outboundGateway{s: s, t: t, err: &feishu.RateLimited{Rejection: &feishu.Rejected{Code: 230020}}}}
	o := Outbound{Store: s, Gateway: g, Now: func() time.Time { return time.UnixMilli(1000) }}
	first, err := o.DispatchOne(ctx, "host", "thread", 1000)
	if err != nil || first.State != "deferred" {
		t.Fatal(first, err)
	}
	b, err := s.Binding(ctx, "host", "thread")
	if err != nil || b.RepairRequired {
		t.Fatal("limit quarantined healthy target", b, err)
	}
	g.err = nil
	next, err := o.DispatchOne(ctx, "host", "thread", 3000)
	if err != nil || next.State != "confirmed" || len(g.parents) != 2 || g.parents[1] != "root" {
		t.Fatal(next, err, g.parents)
	}
	if first.AttemptID == next.AttemptID {
		t.Fatal("rejected attempt reused")
	}
}

type topicResultGateway struct {
	*topicGateway
	threadID string
}

func (g *topicResultGateway) ReplyTopicCardResult(ctx context.Context, parent, card, uuid string) (feishu.TopicCardResult, error) {
	id, err := g.ReplyTopicCard(ctx, parent, card, uuid)
	return feishu.TopicCardResult{MessageID: id, ThreadID: g.threadID}, err
}
func TestTopicResponsePersistsFirstPlatformThreadIdentity(t *testing.T) {
	ctx := context.Background()
	s, _, _ := outboundFixture(t, "human")
	makeTopic(t, s)
	if _, err := s.DB.Exec(`UPDATE bindings SET platform_thread_id=''`); err != nil {
		t.Fatal(err)
	}
	g := &topicResultGateway{topicGateway: &topicGateway{outboundGateway: &outboundGateway{s: s, t: t}}, threadID: "platform-returned"}
	result, err := (Outbound{Store: s, Gateway: g}).DispatchOne(ctx, "host", "thread", 1000)
	if err != nil || result.State != "confirmed" {
		t.Fatal(result, err)
	}
	b, err := s.Binding(ctx, "host", "thread")
	if err != nil || b.PlatformThreadID != "platform-returned" {
		t.Fatal("platform topic was discarded", b, err)
	}
}
func TestTopicResponseConflictingIdentityStopsTarget(t *testing.T) {
	ctx := context.Background()
	s, _, _ := outboundFixture(t, "human")
	makeTopic(t, s)
	g := &topicResultGateway{topicGateway: &topicGateway{outboundGateway: &outboundGateway{s: s, t: t}}, threadID: "other-platform"}
	result, err := (Outbound{Store: s, Gateway: g}).DispatchOne(ctx, "host", "thread", 1000)
	if !errors.Is(err, store.ErrConflict) || result.State != "unknown" {
		t.Fatal(result, err)
	}
	b, err := s.Binding(ctx, "host", "thread")
	if err != nil || !b.RepairRequired || b.PlatformThreadID != "platform" {
		t.Fatal("platform topic was changed", b, err)
	}
}

func TestTwoTopicsInSameWorkgroupKeepDistinctOutboundRoots(t *testing.T) {
	ctx := context.Background()
	s, _, _ := outboundFixture(t, "human")
	makeTopic(t, s)
	if _, err := s.DB.Exec(`INSERT INTO thread_observations(host_id,thread_id,origin,state) VALUES('host','second','new','eligible');
INSERT INTO create_intents(intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,created_at) VALUES('second-intent','host','second','owner','second','[]','confirmed','chat',100);`); err != nil {
		t.Fatal(err)
	}
	if err := s.BindFollowTarget(ctx, store.Binding{HostID: "host", ThreadID: "second", IntentID: "second-intent", ChatID: "chat", FollowMode: "topic", RootMessageID: "second-root", PlatformThreadID: "second-platform"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StageMirrors(ctx, "host", "second", []store.Mirror{{Key: "second-document", TurnID: "second-turn", Kind: "human", Card: `{"body":"second"}`}}, store.Watermark{ObservedAtMS: 102}); err != nil {
		t.Fatal(err)
	}
	g := &topicGateway{outboundGateway: &outboundGateway{s: s, t: t}}
	o := Outbound{Store: s, Gateway: g}
	for _, thread := range []string{"thread", "second"} {
		result, err := o.DispatchOne(ctx, "host", thread, 1000)
		if err != nil || result.State != "confirmed" {
			t.Fatal(result, err)
		}
	}
	if len(g.parents) != 2 || g.parents[0] != "root" || g.parents[1] != "second-root" {
		t.Fatal("workgroup topics crossed", g.parents)
	}
}
