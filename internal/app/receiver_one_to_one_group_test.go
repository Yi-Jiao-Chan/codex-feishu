package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/creation"
	"codex-feishu/internal/store"
)

func TestReceiverOneToOneGroupPersistsTextAndPostOnce(t *testing.T) {
	for _, tc := range []struct {
		name, kind, content string
		attachments         bool
	}{
		{"text", "text", `{"text":"continue this task"}`, false},
		{"text with attachments enabled", "text", `{"text":"continue this task"}`, true},
		{"post", "post", `{"title":"","content":[[{"tag":"text","text":"continue this task"}]]}`, false},
		{"post with attachments enabled", "post", `{"title":"","content":[[{"tag":"text","text":"continue this task"}]]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, db := receiverFixture(t)
			r.Config.Attachments.Enabled = tc.attachments
			checks, wakes := 0, 0
			r.IsOneToOneGroup = func(ctx context.Context, chat, user string) (bool, error) {
				checks++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Second || chat != "chat" || user != "ou_operator" {
					t.Fatal("membership check lost its deadline or identity")
				}
				return true, nil
			}
			r.Wake = func(thread string) {
				if thread != "thread" {
					t.Fatal("input woke a different conversation")
				}
				wakes++
			}
			e := receiverMessage(t, "chat", "ou_operator", "no-at-input", "group", false)
			e.Event.Message.MessageType, e.Event.Message.Content = &tc.kind, &tc.content
			for i := 0; i < 2; i++ {
				if err := r.Message(context.Background(), e); err != nil {
					t.Fatal(err)
				}
			}
			receipts, err := db.Receipts(context.Background(), "thread")
			if err != nil || len(receipts) != 1 || receipts[0].Body != "continue this task" || checks != 2 || wakes != 2 {
				t.Fatalf("ordinary input was lost, changed or duplicated: receipts=%+v checks=%d wakes=%d err=%v", receipts, checks, wakes, err)
			}
		})
	}
}

func TestReceiverOneToOneGroupRejectsUnverifiedOrUnauthorizedInput(t *testing.T) {
	for _, tc := range []struct {
		name, chat, user string
		allowed, noCheck bool
		readErr          error
		wantChecks       int
	}{
		{name: "more members", chat: "chat", user: "ou_operator", wantChecks: 1},
		{name: "permission error", chat: "chat", user: "ou_operator", allowed: true, readErr: errors.New("fixture permission denied"), wantChecks: 1},
		{name: "timeout", chat: "chat", user: "ou_operator", allowed: true, readErr: context.DeadlineExceeded, wantChecks: 1},
		{name: "missing checker", chat: "chat", user: "ou_operator", noCheck: true},
		{name: "unbound group", chat: "unbound", user: "ou_operator", allowed: true},
		{name: "unauthorized sender", chat: "chat", user: "ou_outsider", allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, db := receiverFixture(t)
			checks, wakes, commands, creates := 0, 0, 0, 0
			if !tc.noCheck {
				r.IsOneToOneGroup = func(context.Context, string, string) (bool, error) {
					checks++
					return tc.allowed, tc.readErr
				}
			}
			r.Wake = func(string) { wakes++ }
			r.Command = func(context.Context, InboundCommand) error { commands++; return nil }
			r.Create = func(context.Context, creation.Inbound) (bool, error) { creates++; return true, nil }
			for _, body := range []string{`{"text":"continue this task"}`, `{"text":"/new another task"}`, `{"text":"/help"}`} {
				e := receiverMessage(t, tc.chat, tc.user, body, "group", false)
				e.Event.Message.Content = &body
				if err := r.Message(context.Background(), e); err != nil {
					t.Fatal(err)
				}
			}
			receipts, err := db.Receipts(context.Background(), "thread")
			if err != nil || len(receipts) != 0 || wakes != 0 || commands != 0 || creates != 0 || checks != 3*tc.wantChecks {
				t.Fatalf("ineligible input reached a handler: receipts=%d checks=%d wakes=%d commands=%d creates=%d err=%v", len(receipts), checks, wakes, commands, creates, err)
			}
		})
	}
}

func TestReceiverOneToOneGroupRechecksEveryMessageAndKeepsMentions(t *testing.T) {
	r, db := receiverFixture(t)
	checks, commands := 0, 0
	allowed := true
	r.IsOneToOneGroup = func(context.Context, string, string) (bool, error) {
		checks++
		return allowed, nil
	}
	r.Command = func(_ context.Context, c InboundCommand) error {
		commands++
		if c.Text != "/help" || c.ThreadID != "thread" {
			t.Fatal("command lost its bound target", c)
		}
		return nil
	}
	for i, step := range []struct {
		body               string
		allowed, mentioned bool
	}{
		{`{"text":"/help"}`, true, false},
		{`{"text":"first input"}`, true, false},
		{`{"text":"another member has joined"}`, false, false},
		{`{"text":"@_user_1 explicit input"}`, false, true},
		{`{"text":"alone again"}`, true, false},
	} {
		allowed = step.allowed
		e := receiverMessage(t, "chat", "ou_operator", step.body, "group", step.mentioned)
		e.Event.Message.Content = &step.body
		if err := r.Message(context.Background(), e); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	receipts, err := db.Receipts(context.Background(), "thread")
	if err != nil || len(receipts) != 3 || commands != 1 || checks != 4 {
		t.Fatalf("membership change or mention handling failed: receipts=%d commands=%d checks=%d err=%v", len(receipts), commands, checks, err)
	}
}

func TestReceiverOneToOneGroupPreservesBoundAuthorizationAndCancellation(t *testing.T) {
	for _, cancelDuringRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "binding guard", true: "canceled read"}[cancelDuringRead], func(t *testing.T) {
			r, db := receiverFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.IsOneToOneGroup = func(context.Context, string, string) (bool, error) {
				if cancelDuringRead {
					cancel()
				}
				return true, nil
			}
			r.BeginBound = func(context.Context, store.Binding, string) (context.Context, func(), error) {
				return nil, nil, store.ErrIneligible
			}
			e := receiverMessage(t, "chat", "ou_operator", "denied-input", "group", false)
			e.Event.Message.Content = str(`{"text":"continue this task"}`)
			err := r.Message(ctx, e)
			if !cancelDuringRead && !errors.Is(err, store.ErrIneligible) || cancelDuringRead && err != nil {
				t.Fatal("incorrect rejection", err)
			}
			receipts, err := db.Receipts(context.Background(), "thread")
			if err != nil || len(receipts) != 0 {
				t.Fatal("rejected input reached receipt queue", err)
			}
		})
	}
}

func TestReceiverOneToOneGroupPreservesTopicScope(t *testing.T) {
	r, db := receiverFixture(t)
	if _, err := db.DB.Exec(`UPDATE bindings SET follow_mode='topic',root_message_id='root',platform_thread_id='platform-root' WHERE host_id='host' AND thread_id='thread'`); err != nil {
		t.Fatal(err)
	}
	checks := 0
	r.IsOneToOneGroup = func(context.Context, string, string) (bool, error) { checks++; return true, nil }
	for _, root := range []string{"", "other-root", "root"} {
		e := receiverMessage(t, "chat", "ou_operator", "topic-input-"+root, "group", false)
		e.Event.Message.Content = str(`{"text":"continue this task"}`)
		e.Event.Message.RootId = &root
		if err := r.Message(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	receipts, err := db.Receipts(context.Background(), "thread")
	if err != nil || len(receipts) != 1 || receipts[0].MessageID != "topic-input-root" || checks != 1 {
		t.Fatal("unmentioned input escaped its topic", receipts, checks, err)
	}
}

func TestReceiverOneToOneGroupKeepsStandaloneAttachmentFlag(t *testing.T) {
	r, db := receiverFixture(t)
	r.Config.Attachments.Enabled = true
	r.IsOneToOneGroup = func(context.Context, string, string) (bool, error) {
		t.Fatal("standalone attachment bypassed its receive flag")
		return true, nil
	}
	e := attachmentEvent(t, "standalone-image", "ou_operator", "image", map[string]string{"image_key": "fixture-image"}, false)
	if err := r.Message(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	receipts, err := db.Receipts(context.Background(), "thread")
	if err != nil || len(receipts) != 0 {
		t.Fatal("disabled standalone reception created a receipt", err)
	}
}

type oneToOneServiceGateway struct {
	*serviceGateway
	checks atomic.Int32
}

func (g *oneToOneServiceGateway) IsOneToOneGroup(_ context.Context, chat, user string) (bool, error) {
	g.checks.Add(1)
	return chat == "chat" && user == "ou_operator", nil
}

func TestServiceOneToOneGroupUsesExistingQueue(t *testing.T) {
	db, cfg, root := serviceFixture(t)
	native := serviceTestNative(root)
	g := &oneToOneServiceGateway{serviceGateway: &serviceGateway{runtimeGateway: &runtimeGateway{}, listening: make(chan struct{})}}
	s := &Service{Config: cfg, Store: db, Gateway: g, BotID: "ou_bot", Connect: func(context.Context) (ServiceNative, error) { return native, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("service did not stop")
		}
	})
	select {
	case <-g.listening:
	case <-time.After(3 * time.Second):
		t.Fatal("receiver missing")
	}
	e := receiverMessage(t, "chat", "ou_operator", "one-to-one-input", "group", false)
	e.Event.Message.Content = str(`{"text":"continue this task"}`)
	for i := 0; i < 2; i++ {
		if err := g.message(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	runtimeWait(t, func() bool {
		items, err := db.Submissions(ctx, store.SubmissionScope{HostID: "host", AppID: cfg.Feishu.AppID, ThreadID: "thread"})
		return err == nil && len(items) == 1 && items[0].State == "queued" && native.adds.Load() == 1
	})
	if g.checks.Load() != 2 || g.listens.Load() != 1 {
		t.Fatal("service lost the membership guard or duplicated its receiver")
	}
}
