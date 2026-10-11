package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

func appCommandFixture(t *testing.T) (Commands, *store.Store, InboundCommand, time.Time) {
	t.Helper()
	r, s := receiverFixture(t)
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	r.Config.Notifications.TargetOpenIDs = []string{"ou_notify", "ou_other"}
	r.Config.Discovery.AllowedRoots = []string{t.TempDir()}
	for _, user := range r.Config.Notifications.TargetOpenIDs {
		p, _ := rules.DefaultPolicy("Asia/Shanghai")
		if _, err := s.InitNotificationPolicyAt(context.Background(), "app", user, p, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	c := Commands{Store: s, Config: r.Config, Now: func() time.Time { return now }}
	var seq int
	var name string
	if err := s.DB.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &c.Config.Storage.DatabaseFile); err != nil {
		t.Fatal(err)
	}
	return c, s, InboundCommand{MessageID: "command", ChatID: "private", ChatType: "p2p", UserID: "ou_notify"}, now
}

func TestReceiverUnsupportedContentRepliesOnceWithoutNativeReceipt(t *testing.T) {
	ctx := context.Background()
	c, s, _, _ := appCommandFixture(t)
	r := &Receiver{Store: s, Config: c.Config, BotID: "ou_bot", Command: c.Handle}
	e := receiverMessage(t, "chat", "ou_operator", "attachment", "group", true)
	kind, content := "image", `{"image_key":"sensitive-image-key"}`
	e.Event.Message.MessageType, e.Event.Message.Content = &kind, &content
	for i := 0; i < 2; i++ {
		if err := r.Message(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := s.CommandReply(ctx, "host", "app", "attachment")
	if err != nil || !strings.Contains(saved.Card, "仅支持文字") || strings.Contains(saved.Card, "sensitive-image-key") || saved.MessageID != "attachment" {
		t.Fatal("unsupported input feedback lost its parent or exposed content", err)
	}
	receipts, err := s.Receipts(ctx, "thread")
	if err != nil || len(receipts) != 0 {
		t.Fatal("unsupported input entered native receipt queue", err)
	}
	stranger := receiverMessage(t, "chat", "ou_stranger", "stranger-attachment", "group", true)
	stranger.Event.Message.MessageType, stranger.Event.Message.Content = &kind, &content
	if err := r.Message(ctx, stranger); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommandReply(ctx, "host", "app", "stranger-attachment"); err == nil {
		t.Fatal("stranger received a feedback message")
	}
}

func TestCommandsNotifyOwnOnlyAndOldOnReplayCannotUndoOff(t *testing.T) {
	ctx := context.Background()
	c, s, in, now := appCommandFixture(t)
	in.Text = "/notify at on"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	original, err := s.CommandReply(ctx, "host", "app", in.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	off := in
	off.MessageID, off.Text = "later-off", "/notify at off"
	c.Now = func() time.Time { return now.Add(time.Second) }
	if err := c.Handle(ctx, off); err != nil {
		t.Fatal(err)
	}
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	p, err := s.NotificationPolicy(ctx, "app", "ou_notify")
	if err != nil || p.Enabled || p.Revision != 2 {
		t.Fatalf("old on replay changed current off: %+v %v", p, err)
	}
	again, err := s.CommandReply(ctx, "host", "app", in.MessageID)
	if err != nil || again.ID != original.ID || again.Card != original.Card {
		t.Fatal("replay lost saved result")
	}
	in.MessageID, in.Text = "target-other", "/notify at off ou_other"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	other, err := s.NotificationPolicy(ctx, "app", "ou_other")
	if err != nil || !other.Enabled || other.Revision != 0 {
		t.Fatalf("caller changed another recipient: %+v %v", other, err)
	}
	in.MessageID, in.UserID, in.Text = "operator-not-recipient", "ou_operator", "/notify at off"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	p, _ = s.NotificationPolicy(ctx, "app", "ou_notify")
	if p.Revision != 2 {
		t.Fatal("nonrecipient modified notification policy")
	}
}

func TestCommandsDirectoryScopeOverlapAndDeletedPathReplay(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := appCommandFixture(t)
	in.UserID = "ou_operator"
	project := c.Config.Discovery.AllowedRoots[0]
	in.Text = "/mute-dir " + project + " --recursive"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	exact := in
	exact.MessageID, exact.Text = "exact", "/mute-dir "+project
	if err := c.Handle(ctx, exact); err != nil {
		t.Fatal(err)
	}
	remove := in
	remove.MessageID, remove.Text = "remove", "/unmute-dir "+project
	if err := c.Handle(ctx, remove); err != nil {
		t.Fatal(err)
	}
	rulesNow, err := s.DirectoryRules(ctx, "host", "app")
	if err != nil || len(rulesNow) != 1 || !rulesNow[0].Recursive {
		t.Fatalf("exact/recursive overlap: %+v %v", rulesNow, err)
	}
	reply, err := s.CommandReply(ctx, "host", "app", remove.MessageID)
	if err != nil || !strings.Contains(reply.Card, "当前仍命中") {
		t.Fatalf("unmute falsely restored automatic groups: %+v %v", reply, err)
	}
	if err := c.Handle(ctx, exact); err != nil {
		t.Fatal(err)
	}
	rulesNow, _ = s.DirectoryRules(ctx, "host", "app")
	if len(rulesNow) != 1 {
		t.Fatal("replay reapplied removed exact mute")
	}
	if err := os.Remove(project); err != nil {
		t.Fatal(err)
	}
	if err := c.Handle(ctx, in); err != nil {
		t.Fatalf("days-later deleted path replay failed: %v", err)
	}
}

func TestCommandsAuthorizeRegisteredWorktreeAgainstConfiguredWorktree(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := appCommandFixture(t)
	base := t.TempDir()
	original := filepath.Join(base, "original")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", original}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, output)
		}
	}
	git("init", "-q")
	git("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "fixture")
	configured, candidate := filepath.Join(base, "configured"), filepath.Join(base, "candidate")
	git("worktree", "add", "--detach", configured)
	git("worktree", "add", "--detach", candidate)
	for _, root := range []string{original, configured, candidate} {
		if err := os.Mkdir(filepath.Join(root, "subdir"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	c.Config.Discovery.AllowedRoots = []string{filepath.Join(configured, "subdir")}
	in.UserID, in.Text = "ou_operator", "/mute-dir "+filepath.Join(candidate, "subdir")
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	stored, err := s.DirectoryRules(ctx, "host", "app")
	if err != nil || len(stored) != 1 || stored[0].Path != filepath.Join(candidate, "subdir") || stored[0].Recursive {
		t.Fatalf("registered worktree authorization changed path/scope: %+v %v", stored, err)
	}
}

func TestCommandsStrangersSilentAndHelpDoesNotCreateTasks(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := appCommandFixture(t)
	in.UserID, in.Text = "stranger", "/help"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_replies`).Scan(&count); err != nil || count != 0 {
		t.Fatal("stranger received spam response")
	}
	in.UserID, in.Text = "ou_operator", "ordinary task text"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	r, err := s.CommandReply(ctx, "host", "app", in.MessageID)
	if err != nil || !strings.Contains(r.Card, "Codex App") || !strings.Contains(r.Card, "暂不创建新任务") {
		t.Fatalf("unbound guidance missing: %+v %v", r, err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM submission_actions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("help created task control")
	}
}

func TestCommandsStopContinuePersistOnlyAndWakeAfterCommit(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := appCommandFixture(t)
	wakes := 0
	c.Wake = func(thread string) {
		if thread != "thread" {
			t.Fatal("wrong thread wake")
		}
		var count int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_replies`).Scan(&count); err != nil || count == 0 {
			t.Fatal("wake before command commit")
		}
		wakes++
	}
	in.UserID, in.ThreadID, in.ChatID, in.ChatType, in.Text = "ou_operator", "thread", "chat", "group", "/stop"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	// SDK replay after binding discovery changed must retain the original action.
	replay := in
	replay.ThreadID = ""
	if err := c.Handle(ctx, replay); err != nil {
		t.Fatal(err)
	}
	in.MessageID, in.Text = "continue", "/continue"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	var actions int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM submission_actions WHERE stage='prepare' AND state='ready'`).Scan(&actions); err != nil || actions != 2 || wakes != 3 {
		t.Fatalf("control actions/wake: %d %d %v", actions, wakes, err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_command_reply BEFORE INSERT ON command_replies BEGIN SELECT RAISE(ABORT,'fixture reply failure'); END`); err != nil {
		t.Fatal(err)
	}
	in.MessageID, in.Text = "rollback", "/stop"
	if err := c.Handle(ctx, in); err == nil {
		t.Fatal("failed reply accepted stop")
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM submission_actions`).Scan(&actions); err != nil || actions != 2 || wakes != 3 {
		t.Fatal("partial control action or premature wake survived rollback")
	}
}

type commandTestGateway struct {
	t     *testing.T
	s     *store.Store
	want  store.CommandReply
	calls int
	err   error
	close bool
}

func (g *commandTestGateway) ReplyCard(ctx context.Context, parent, card, uuid string) (string, error) {
	g.t.Helper()
	r, err := g.s.CommandReply(ctx, g.want.HostID, g.want.AppID, g.want.MessageID)
	if err != nil || r.State != "dispatching" || parent != g.want.MessageID || card != g.want.Card || uuid != g.want.ID {
		g.t.Fatalf("reply before journal or wrong parent/payload: %+v %v", r, err)
	}
	g.calls++
	if g.close {
		g.s.Close()
	}
	return "reply-message", g.err
}
func (g *commandTestGateway) SendCard(context.Context, string, string, string, string) (string, error) {
	g.t.Fatal("command reply downgraded to send")
	return "", nil
}
func (g *commandTestGateway) UpdateCard(context.Context, string, string) error {
	g.t.Fatal("command reply used PATCH")
	return nil
}

func TestCommandRepliesFreezeOriginalParentAndAmbiguousOutcomes(t *testing.T) {
	ctx := context.Background()
	for _, variant := range []string{"confirmed", "unknown", "rejected", "confirmation_failure"} {
		t.Run(variant, func(t *testing.T) {
			c, s, in, _ := appCommandFixture(t)
			in.Text = "/help"
			if err := c.Handle(ctx, in); err != nil {
				t.Fatal(err)
			}
			r, err := s.CommandReply(ctx, "host", "app", in.MessageID)
			if err != nil {
				t.Fatal(err)
			}
			g := &commandTestGateway{t: t, s: s, want: r}
			switch variant {
			case "unknown":
				g.err = errors.New("transport failed")
			case "rejected":
				g.err = &feishu.Rejected{Code: 230002}
			case "confirmation_failure":
				g.close = true
			}
			processed, err := c.DispatchOne(ctx, g)
			if !processed || g.calls != 1 || (variant == "confirmation_failure") != (err != nil) {
				t.Fatalf("delivery: %t %v", processed, err)
			}
			if variant == "confirmation_failure" {
				reopened, err := store.Open(c.Config.Storage.DatabaseFile)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				s, c.Store, g.s = reopened, reopened, reopened
			}
			if err := s.RecoverCommandDispatches(ctx, "host"); err != nil {
				t.Fatal(err)
			}
			if processed, err := c.DispatchOne(ctx, g); err != nil || processed || g.calls != 1 {
				t.Fatalf("command reply resent: %t %v", processed, err)
			}
			current, err := s.CommandReply(ctx, "host", "app", in.MessageID)
			want := variant
			if variant == "confirmation_failure" {
				want = "unknown"
			}
			if err != nil || current.State != want {
				t.Fatalf("outcome not frozen: %+v %v", current, err)
			}
		})
	}
}
