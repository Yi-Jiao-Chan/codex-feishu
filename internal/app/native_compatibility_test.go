package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type compatibilityGateway struct {
	retryCommandGateway
	attempts int
	owner    string
	feature  string
}

func (g *compatibilityGateway) SendCard(ctx context.Context, kind, target, card, id string) (string, error) {
	g.t.Helper()
	g.attempts++
	feature := g.feature
	if feature == "" {
		feature = "任务队列读取"
	}
	if kind != "open_id" || target != g.owner || !strings.Contains(card, feature) || !strings.Contains(card, "0.169.1") {
		g.t.Fatal("notice reached the wrong target or omitted the affected feature")
	}
	var state, frozen, owner string
	if err := g.db.DB.QueryRowContext(ctx, "SELECT state,card_json,user_id FROM command_reply_attempts WHERE attempt_id=?", id).Scan(&state, &frozen, &owner); err != nil ||
		state != "dispatching" || frozen != card || owner != target {
		g.t.Fatal("private send lacked its frozen delivery attempt", err)
	}
	if g.err != nil {
		return "", g.err
	}
	return "om_notified", nil
}

type compatibilityNative struct {
	serviceNative
	observe func(string)
}

func (*compatibilityNative) AppServerVersion() string { return "0.169.1" }
func (n *compatibilityNative) SetCompatibilityObserver(observe func(string)) {
	n.observe = observe
}

func TestCompatibilityNoticeRequiresObservedFailure(t *testing.T) {
	ctx := context.Background()
	commands, db, _, now := appCommandFixture(t)
	cfg := commands.Config
	cfg.Access.OwnerOpenID = "ou_operator"
	cfg.Creation.Enabled = true
	commands.Config = cfg
	g := &compatibilityGateway{retryCommandGateway: retryCommandGateway{t: t, db: db}, owner: cfg.Access.OwnerOpenID, feature: "飞书新建任务"}
	s := &Service{Config: cfg, Store: db, Now: commands.Now}
	native := &compatibilityNative{}
	s.observeCompatibility(ctx, native)
	if wrote, err := commands.DispatchOne(ctx, g); wrote || err != nil {
		t.Fatal("a newly connected version produced a compatibility notice without a failure", err)
	}
	legacy := cards.Message("Codex 兼容性提醒", "功能暂不可用", "当前 Codex App Server 为 0.169.1。\n\n「飞书新建任务」所需协议尚未通过检查，暂时不可用。")
	if err := db.StageCompatibilityNotice(ctx, cfg.Host.ID, cfg.Feishu.AppID, cfg.Access.OwnerOpenID, "0.169.1", g.feature, legacy, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := commands.DispatchOne(ctx, g); err != nil || g.attempts != 0 {
		t.Fatal("legacy version advisory was sent", err)
	}
	native.observe("thread/start")
	native.observe("thread/start")
	if wrote, err := commands.DispatchOne(ctx, g); !wrote || err != nil || g.attempts != 1 {
		t.Fatal("legacy advisory suppressed the real failure", err)
	}
	if wrote, err := commands.DispatchOne(ctx, g); wrote || err != nil {
		t.Fatal("repeated failure produced a duplicate notice", err)
	}
}

func TestCompatibilityNoticePersistsAcrossServiceRestarts(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"confirmed", nil},
		{"unknown", &feishu.Unknown{}},
		{"rate limited", &feishu.RateLimited{RetryAfter: 5 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			commands, db, _, now := appCommandFixture(t)
			commands.Config.Access.OwnerOpenID = "ou_operator"
			g := &compatibilityGateway{retryCommandGateway: retryCommandGateway{t: t, db: db, err: tc.err}, owner: commands.Config.Access.OwnerOpenID}
			s := &Service{Config: commands.Config, Store: db, Now: commands.Now}
			s.stageCompatibilityNotice(ctx, "0.169.1", "任务队列读取")
			if wrote, err := commands.DispatchOne(ctx, g); !wrote || err != nil {
				t.Fatal("initial notice not dispatched", err)
			}
			reopened, err := store.Open(commands.Config.Storage.DatabaseFile)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			commands.Store, g.db = reopened, reopened
			commands.Now = func() time.Time { return now.Add(4 * time.Second) }
			restarted := &Service{Config: commands.Config, Store: reopened, Now: commands.Now}
			if err := reopened.RecoverCommandDispatches(ctx, restarted.Config.Host.ID); err != nil {
				t.Fatal(err)
			}
			restarted.stageCompatibilityNotice(ctx, "0.169.1", "任务队列读取")
			if wrote, err := commands.DispatchOne(ctx, g); wrote || err != nil || g.attempts != 1 {
				t.Fatal("unexpected repeat private send", err)
			}
			if tc.name == "rate limited" {
				commands.Now = func() time.Time { return now.Add(5 * time.Second) }
				g.err = nil
				if wrote, err := commands.DispatchOne(ctx, g); !wrote || err != nil || g.attempts != 2 {
					t.Fatal("definitely rejected notice did not retry", err)
				}
			}
		})
	}
}
