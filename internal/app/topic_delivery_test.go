package app

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type topicFeedbackGateway struct {
	rootCalls                                  int
	rootErr                                    error
	rootDeleted, rootMissingDeleted            bool
	t                                          *testing.T
	s                                          *store.Store
	targets                                    []store.FollowAttemptTarget
	cards                                      []string
	err                                        error
	groupCalls, sendCalls, listCalls, getCalls int
	wrongRoot                                  bool
}

func (g *topicFeedbackGateway) SendCard(context.Context, string, string, string, string) (string, error) {
	g.sendCalls++
	return "", errors.New("unexpected group send")
}
func (g *topicFeedbackGateway) ReplyCard(context.Context, string, string, string) (string, error) {
	g.groupCalls++
	return "", errors.New("unexpected ordinary group reply")
}
func (g *topicFeedbackGateway) UpdateCard(context.Context, string, string) error {
	return errors.New("unexpected patch")
}
func (g *topicFeedbackGateway) MemberIDs(context.Context, string) ([]string, error) {
	return []string{"ou_user"}, nil
}
func (g *topicFeedbackGateway) ReplyTopicCard(ctx context.Context, parent, card, uuid string) (string, error) {
	target, err := g.s.FollowAttemptTarget(ctx, uuid)
	if err != nil || target.ParentMessageID != parent || target.FollowMode != "topic" {
		g.t.Fatalf("wire issued without exact frozen target: %+v %v", target, err)
	}
	g.targets = append(g.targets, target)
	g.cards = append(g.cards, card)
	return "topic-card-" + uuid, g.err
}
func (g *topicFeedbackGateway) record() feishu.CardMessage {
	target := g.targets[len(g.targets)-1]
	root, thread, parent := target.RootMessageID, target.PlatformThreadID, target.ParentMessageID
	deleted := false
	if thread == "" {
		thread = "platform-" + root
	}
	if root == "" {
		root = "platform-root-" + thread
	}
	if g.wrongRoot {
		root = "another-root"
	}
	return feishu.CardMessage{ID: "topic-card-" + target.AttemptID, ChatID: target.ChatID, Type: "interactive", SenderID: "app", SenderIDType: "app_id", SenderType: "app", ParentID: &parent, RootID: &root, ThreadID: &thread, Deleted: &deleted, Content: g.cards[len(g.cards)-1]}
}
func (g *topicFeedbackGateway) ReadCardMessage(ctx context.Context, id string) (feishu.CardMessage, error) {
	var chat, platform string
	err := g.s.DB.QueryRowContext(ctx, `SELECT chat_id,platform_thread_id FROM bindings WHERE host_id='host' AND root_message_id=? UNION ALL SELECT chat_id,platform_thread_id FROM command_source_targets WHERE host_id='host' AND root_message_id=? LIMIT 1`, id, id).Scan(&chat, &platform)
	if err == nil {
		g.rootCalls++
		if g.rootErr != nil {
			return feishu.CardMessage{}, g.rootErr
		}
		root := feishu.CardMessage{ID: id, ChatID: chat, RootID: &id, ThreadID: &platform, Deleted: &g.rootDeleted}
		if g.rootMissingDeleted {
			root.Deleted = nil
		}
		return root, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return feishu.CardMessage{}, err
	}
	if len(g.targets) == 0 {
		g.t.Fatalf("root GET had no durable target proof: %s", id)
	}
	g.getCalls++
	return g.record(), nil
}

func (g *topicFeedbackGateway) ListCardMessages(_ context.Context, kind, id, cursor string) (feishu.CardMessagePage, error) {
	g.listCalls++
	target := g.targets[len(g.targets)-1]
	if kind != "thread" || id != target.PlatformThreadID || cursor != "" {
		g.t.Fatal("readback lost frozen topic", kind, id, cursor)
	}
	return feishu.CardMessagePage{Items: []feishu.CardMessage{g.record()}}, nil
}
func appFollowTopic(t *testing.T, s *store.Store, root string) {
	t.Helper()
	if _, err := s.DB.Exec(`UPDATE bindings SET follow_mode='topic',root_message_id=?,platform_thread_id=? WHERE host_id='host' AND thread_id='thread'`, root, "platform-"+root); err != nil {
		t.Fatal(err)
	}
}

func TestTopicCommandAndReminderRejectionsNeverFallBack(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"command", "reminder"} {
		t.Run(kind, func(t *testing.T) {
			var s *store.Store
			var dispatch func() (bool, error)
			var g *topicFeedbackGateway
			if kind == "command" {
				c, db, in, _ := appCommandFixture(t)
				s = db
				appFollowTopic(t, s, "root")
				in.ChatID, in.ChatType, in.ThreadID, in.Text = "chat", "group", "thread", "/notify at status"
				if err := c.Handle(ctx, in); err != nil {
					t.Fatal(err)
				}
				dispatch = func() (bool, error) { return c.DispatchOne(ctx, g) }
			} else {
				tr, _, _ := liveRetryTracker(t)
				s = tr.Store
				appFollowTopic(t, s, "original-input")
				dispatch = func() (bool, error) {
					tr.Gateway, tr.Membership = g, g
					return tr.DispatchOne(ctx, "thread")
				}
			}
			g = &topicFeedbackGateway{t: t, s: s, err: &feishu.Rejected{Code: 230011}}
			wrote, err := dispatch()
			if err != nil || !wrote || len(g.targets) != 1 || g.groupCalls != 0 || g.sendCalls != 0 {
				t.Fatal(wrote, err, g)
			}
			b, err := s.Binding(ctx, "host", "thread")
			if err != nil || !b.RepairRequired {
				t.Fatal("invalid topic remained healthy", b, err)
			}
		})
	}
}

func TestTopicCommandUnknownReadbackConfirmsOnlyFrozenRoot(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := appCommandFixture(t)
	appFollowTopic(t, s, "root")
	in.ChatID, in.ChatType, in.ThreadID, in.Text = "chat", "group", "thread", "/notify at status"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	g := &topicFeedbackGateway{t: t, s: s, err: &feishu.Unknown{Operation: "fixture"}}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	g.wrongRoot = true
	if wrote, err := c.DispatchOne(ctx, g); err != nil || wrote || len(g.targets) != 1 {
		t.Fatal("wrong topic confirmed or caused retry", wrote, err, g)
	}
	g.wrongRoot = false
	if _, err := s.DB.Exec(`UPDATE card_request_markers SET next_check_at_ms=0`); err != nil {
		t.Fatal(err)
	}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote || len(g.targets) != 1 || g.groupCalls != 0 || g.sendCalls != 0 {
		t.Fatal("topic unknown resent", wrote, err, g)
	}
	saved, err := s.CommandReply(ctx, "host", "app", in.MessageID)
	if err != nil || saved.State != "confirmed" || saved.RemoteID != "topic-card-"+g.targets[0].AttemptID {
		t.Fatal(saved, err)
	}
}

func TestTwoTopicCommandsKeepOriginalParentsAndRoots(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := appCommandFixture(t)
	appFollowTopic(t, s, "root-one")
	if _, err := s.DB.Exec(`INSERT INTO thread_observations(host_id,thread_id,origin,state) VALUES('host','second','new','eligible');INSERT INTO create_intents(intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,created_at) VALUES('second-intent','host','second','owner','second','[]','confirmed','chat',100);`); err != nil {
		t.Fatal(err)
	}
	if err := s.BindFollowTarget(ctx, store.Binding{HostID: "host", ThreadID: "second", IntentID: "second-intent", ChatID: "chat", FollowMode: "topic", RootMessageID: "root-two", PlatformThreadID: "platform-root-two"}); err != nil {
		t.Fatal(err)
	}
	g := &topicFeedbackGateway{t: t, s: s}
	for _, thread := range []string{"thread", "second"} {
		in.ChatID, in.ChatType, in.ThreadID, in.MessageID, in.Text = "chat", "group", thread, "command-"+thread, "/notify at status"
		if err := c.Handle(ctx, in); err != nil {
			t.Fatal(err)
		}
		if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote {
			t.Fatal(wrote, err)
		}
	}
	if len(g.targets) != 2 || g.targets[0].RootMessageID != "root-one" || g.targets[1].RootMessageID != "root-two" || g.targets[0].ParentMessageID != "command-thread" || g.targets[1].ParentMessageID != "command-second" || g.groupCalls != 0 || g.sendCalls != 0 {
		t.Fatal("topics shared parents/roots", g.targets)
	}
}

func TestUnboundTopicCommandKeepsSourceButWorkgroupEntryUsesGroupReply(t *testing.T) {
	ctx := context.Background()
	for _, source := range []string{"entry", "topic", "thread_only"} {
		t.Run(source, func(t *testing.T) {
			c, s, in, _ := appCommandFixture(t)
			appFollowTopic(t, s, "bound-root")
			in.ChatID, in.ChatType, in.ThreadID, in.Text = "chat", "group", "", "/notify at status"
			if source == "topic" {
				in.SourceRootID, in.SourceThreadID = "unknown-root", "unknown-platform"
			}
			if source == "thread_only" {
				in.SourceThreadID = "unknown-platform"
			}
			if err := c.Handle(ctx, in); err != nil {
				t.Fatal(err)
			}
			if source == "entry" {
				g := &requestGateway{}
				if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote || g.calls != 1 || g.parent != in.MessageID {
					t.Fatal("main-stream entry rejected", wrote, err, g)
				}
				return
			}
			if source == "thread_only" {
				g := &topicFeedbackGateway{t: t, s: s}
				if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote || len(g.targets) != 0 || g.groupCalls != 0 || g.sendCalls != 0 || g.rootCalls != 0 {
					t.Fatal("rootless topic was written", wrote, err, g)
				}
				return
			}
			g := &topicFeedbackGateway{t: t, s: s, err: &feishu.Rejected{Code: 230011}}
			if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote || len(g.targets) != 1 || g.groupCalls != 0 || g.sendCalls != 0 {
				t.Fatal(wrote, err, g)
			}
			target := g.targets[0]
			if target.ThreadID != "" || target.RootMessageID != in.SourceRootID || target.PlatformThreadID != in.SourceThreadID || target.ParentMessageID != in.MessageID {
				t.Fatal("unbound topic source not frozen", target)
			}
			b, err := s.Binding(ctx, "host", "thread")
			if err != nil || b.RepairRequired {
				t.Fatal("unknown topic changed an unrelated binding", b, err)
			}
		})
	}
}

func TestTopicReminderUnknownReadbackDoesNotResendOrLeaveThread(t *testing.T) {
	ctx := context.Background()
	tr, _, _ := liveRetryTracker(t)
	appFollowTopic(t, tr.Store, "original-input")
	g := &topicFeedbackGateway{t: t, s: tr.Store, err: &feishu.Unknown{Operation: "fixture"}}
	tr.Gateway, tr.Membership = g, g
	if wrote, err := tr.DispatchOne(ctx, "thread"); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	if wrote, err := tr.DispatchOne(ctx, "thread"); err != nil || !wrote || len(g.targets) != 1 || g.getCalls != 1 || g.groupCalls != 0 || g.sendCalls != 0 {
		t.Fatal("unknown topic reminder resent", wrote, err, g)
	}
	var state, remote string
	if err := tr.Store.DB.QueryRow(`SELECT state,remote_id FROM request_reminder_attempts WHERE attempt_id=?`, g.targets[0].AttemptID).Scan(&state, &remote); err != nil || state != "confirmed" || remote == "" {
		t.Fatal(state, remote, err)
	}
}

func TestTwoTopicRemindersKeepOriginalInputParents(t *testing.T) {
	ctx := context.Background()
	tr, now, _ := liveRetryTracker(t)
	s := tr.Store
	appFollowTopic(t, s, "first-root")
	if _, _, err := s.Claim(ctx, store.Receipt{AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: "original-input"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO thread_observations(host_id,thread_id,origin,state) VALUES('host','second','new','eligible');INSERT INTO create_intents(intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,created_at) VALUES('second-intent','host','second','owner','second','[]','confirmed','chat',100);`); err != nil {
		t.Fatal(err)
	}
	if err := s.BindFollowTarget(ctx, store.Binding{HostID: "host", ThreadID: "second", IntentID: "second-intent", ChatID: "chat", FollowMode: "topic", RootMessageID: "second-root", PlatformThreadID: "second-platform"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Claim(ctx, store.Receipt{AppID: "app", ThreadID: "second", ChatID: "chat", MessageID: "second-input"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StageMirrors(ctx, "host", "second", []store.Mirror{{Key: "second-progress", TurnID: "second-turn", Kind: "progress", ParentMessageID: "second-input", Card: `{"body":"waiting"}`}}, store.Watermark{ObservedAtMS: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveAppRequest(ctx, "host", "app", store.PendingRequest{Key: "second-request", ThreadID: "second", TurnID: "second-turn", Method: "item/tool/requestUserInput", EventAt: now.Unix()}, tr.Options, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.StageRequestReminders(ctx, "host", "app", "second"); err != nil {
		t.Fatal(err)
	}
	g := &topicFeedbackGateway{t: t, s: s}
	tr.Gateway, tr.Membership = g, g
	for _, thread := range []string{"thread", "second"} {
		if wrote, err := tr.DispatchOne(ctx, thread); err != nil || !wrote {
			t.Fatal(wrote, err)
		}
	}
	if len(g.targets) != 2 || g.targets[0].RootMessageID != "first-root" || g.targets[1].RootMessageID != "second-root" || g.targets[0].ParentMessageID != "original-input" || g.targets[1].ParentMessageID != "second-input" || g.groupCalls != 0 || g.sendCalls != 0 {
		t.Fatal("topic reminders shared parents", g.targets)
	}
}

func TestTopicCommandAndReminderRateLimitRetryKeepsFrozenParent(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"command", "reminder"} {
		t.Run(kind, func(t *testing.T) {
			var s *store.Store
			var dispatch func() (bool, error)
			var advance func()
			var g *topicFeedbackGateway
			if kind == "command" {
				c, db, in, now := appCommandFixture(t)
				s = db
				c.Now = func() time.Time { return now }
				appFollowTopic(t, s, "root")
				in.ChatID, in.ChatType, in.ThreadID, in.Text = "chat", "group", "thread", "/notify at status"
				if err := c.Handle(ctx, in); err != nil {
					t.Fatal(err)
				}
				dispatch = func() (bool, error) { return c.DispatchOne(ctx, g) }
				advance = func() { now = now.Add(2 * time.Second) }
			} else {
				tr, now, _ := liveRetryTracker(t)
				s = tr.Store
				appFollowTopic(t, s, "original-input")
				dispatch = func() (bool, error) { tr.Gateway, tr.Membership = g, g; return tr.DispatchOne(ctx, "thread") }
				advance = func() { *now = now.Add(2 * time.Second) }
			}
			g = &topicFeedbackGateway{t: t, s: s, err: &feishu.RateLimited{Rejection: &feishu.Rejected{Code: 230020}, RetryAfter: time.Second}}
			if wrote, err := dispatch(); err != nil || !wrote {
				t.Fatal(wrote, err)
			}
			b, err := s.Binding(ctx, "host", "thread")
			if err != nil || b.RepairRequired {
				t.Fatal("transient limit marked repair", b, err)
			}
			advance()
			g.err = nil
			if wrote, err := dispatch(); err != nil || !wrote || len(g.targets) != 2 || g.groupCalls != 0 || g.sendCalls != 0 {
				t.Fatal(wrote, err, g)
			}
			if g.targets[0].AttemptID == g.targets[1].AttemptID || g.targets[0].ParentMessageID != g.targets[1].ParentMessageID || g.targets[0].RootMessageID != g.targets[1].RootMessageID {
				t.Fatal("retry changed frozen target", g.targets)
			}
		})
	}
}

func TestUnboundTopicFeedbackWithoutRootPausesBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := appCommandFixture(t)
	appFollowTopic(t, s, "other-binding-root")
	in.ChatID, in.ChatType, in.ThreadID, in.Text, in.SourceThreadID = "chat", "group", "", "/notify at status", "unknown-platform"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	g := &topicFeedbackGateway{t: t, s: s}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote || len(g.targets) != 0 || g.getCalls != 0 || g.groupCalls != 0 || g.sendCalls != 0 {
		t.Fatal("rootless source wrote content", wrote, err, g)
	}
	b, err := s.Binding(ctx, "host", "thread")
	if err != nil || b.RepairRequired {
		t.Fatal("unbound source touched unrelated target", b, err)
	}
	saved, err := s.CommandReply(ctx, "host", "app", in.MessageID)
	if err != nil || saved.State != "ready" {
		t.Fatal("unverified target was not paused", saved, err)
	}
}

type detailedTopicFeedbackGateway struct {
	*topicFeedbackGateway
	platform string
}

func (g *detailedTopicFeedbackGateway) ReplyTopicCardResult(ctx context.Context, parent, card, uuid string) (feishu.TopicCardResult, error) {
	id, err := g.ReplyTopicCard(ctx, parent, card, uuid)
	return feishu.TopicCardResult{MessageID: id, ThreadID: g.platform}, err
}
func TestUnboundTopicFeedbackConflictingResponseStaysUnknown(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := appCommandFixture(t)
	in.ChatID, in.ChatType, in.Text, in.SourceRootID, in.SourceThreadID = "chat", "group", "/notify at status", "source-root", "source-platform"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	g := &detailedTopicFeedbackGateway{topicFeedbackGateway: &topicFeedbackGateway{t: t, s: s}, platform: "other-platform"}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	saved, err := s.CommandReply(ctx, "host", "app", in.MessageID)
	if err != nil || saved.State != "unknown" || saved.RemoteID != "" || len(g.targets) != 1 || g.groupCalls != 0 || g.sendCalls != 0 {
		t.Fatal("unverified source response confirmed", saved, err)
	}
}

func TestTopicDeletedRootStopsCommandsAndRemindersBeforePlatformWrite(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"command", "reminder"} {
		t.Run(kind, func(t *testing.T) {
			var s *store.Store
			var dispatch func() (bool, error)
			var g *topicFeedbackGateway
			if kind == "command" {
				c, db, in, _ := appCommandFixture(t)
				s = db
				appFollowTopic(t, s, "root")
				in.ChatID, in.ChatType, in.ThreadID, in.Text = "chat", "group", "thread", "/notify at status"
				if err := c.Handle(ctx, in); err != nil {
					t.Fatal(err)
				}
				dispatch = func() (bool, error) { return c.DispatchOne(ctx, g) }
			} else {
				tr, _, _ := liveRetryTracker(t)
				s = tr.Store
				appFollowTopic(t, s, "original-input")
				dispatch = func() (bool, error) { tr.Gateway, tr.Membership = g, g; return tr.DispatchOne(ctx, "thread") }
			}
			// The write gateway would accept a reply; only the root GET rejects it.
			g = &topicFeedbackGateway{t: t, s: s, rootDeleted: true}
			if wrote, err := dispatch(); err != nil || !wrote || g.rootCalls != 1 || len(g.targets) != 0 || g.sendCalls != 0 || g.groupCalls != 0 {
				t.Fatal("deleted root still written", wrote, err, g)
			}
			b, err := s.Binding(ctx, "host", "thread")
			if err != nil || !b.RepairRequired {
				t.Fatal("deleted root not persisted as repair", b, err)
			}
			var state string
			table := "command_reply_attempts"
			if kind == "reminder" {
				table = "request_reminder_attempts"
			}
			if err = s.DB.QueryRow(`SELECT state FROM ` + table).Scan(&state); err != nil || state != "rejected" {
				t.Fatal("local root rejection misclassified", state, err)
			}
		})
	}
}

func TestTopicRootReadFailureDefersCommandsAndRemindersWithoutRepair(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"command", "reminder"} {
		for _, variant := range []string{"unknown", "missing_deleted"} {
			t.Run(kind+"/"+variant, func(t *testing.T) {
				var s *store.Store
				var dispatch func() (bool, error)
				var advance func()
				var g *topicFeedbackGateway
				if kind == "command" {
					c, db, in, now := appCommandFixture(t)
					s = db
					c.Now = func() time.Time { return now }
					appFollowTopic(t, s, "root")
					in.ChatID, in.ChatType, in.ThreadID, in.Text = "chat", "group", "thread", "/notify at status"
					if err := c.Handle(ctx, in); err != nil {
						t.Fatal(err)
					}
					dispatch = func() (bool, error) { return c.DispatchOne(ctx, g) }
					advance = func() { now = now.Add(2 * time.Second) }
				} else {
					tr, now, _ := liveRetryTracker(t)
					s = tr.Store
					appFollowTopic(t, s, "original-input")
					dispatch = func() (bool, error) { tr.Gateway, tr.Membership = g, g; return tr.DispatchOne(ctx, "thread") }
					advance = func() { *now = now.Add(2 * time.Second) }
				}
				g = &topicFeedbackGateway{t: t, s: s}
				if variant == "unknown" {
					g.rootErr = &feishu.Unknown{Operation: "GET root"}
				} else {
					g.rootMissingDeleted = true
				}
				if wrote, err := dispatch(); err != nil || !wrote || len(g.targets) != 0 || g.sendCalls != 0 || g.groupCalls != 0 {
					t.Fatal("incomplete root wrote content", wrote, err, g)
				}
				b, err := s.Binding(ctx, "host", "thread")
				if err != nil || b.RepairRequired {
					t.Fatal("temporary root read marked repair", b, err)
				}
				advance()
				g.rootErr, g.rootMissingDeleted = nil, false
				if wrote, err := dispatch(); err != nil || !wrote || len(g.targets) != 1 || g.rootCalls != 2 {
					t.Fatal("read failure consumed unsent work", wrote, err, g)
				}
			})
		}
	}
}

func TestUnboundTopicRootDeletionStopsFeedbackWithoutRepairingOtherBinding(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := appCommandFixture(t)
	appFollowTopic(t, s, "other-binding-root")
	in.ChatID, in.ChatType, in.Text, in.SourceRootID, in.SourceThreadID = "chat", "group", "/notify at status", "source-root", "source-platform"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	g := &topicFeedbackGateway{t: t, s: s, rootDeleted: true}
	if wrote, err := c.DispatchOne(ctx, g); err != nil || !wrote || g.rootCalls != 1 || len(g.targets) != 0 || g.sendCalls != 0 || g.groupCalls != 0 {
		t.Fatal(wrote, err, g)
	}
	b, err := s.Binding(ctx, "host", "thread")
	if err != nil || b.RepairRequired {
		t.Fatal("unbound root marked unrelated binding", b, err)
	}
}
