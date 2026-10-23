package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func settingString(s string) *string { return &s }

type settingsNativeFixture struct {
	current                    codexclient.ThreadSettings
	models                     []codexclient.Model
	writes                     int
	readErr, listErr, writeErr error
	apply                      bool
	beforeWrite                func(codexclient.SettingsPatch)
}

func (n *settingsNativeFixture) ReadThreadSettings(context.Context, string) (codexclient.ThreadSettings, error) {
	return n.current, n.readErr
}
func (n *settingsNativeFixture) ListModels(context.Context) ([]codexclient.Model, error) {
	return n.models, n.listErr
}
func (n *settingsNativeFixture) UpdateThreadSettings(_ context.Context, thread string, p codexclient.SettingsPatch) error {
	if n.beforeWrite != nil {
		n.beforeWrite(p)
	}
	n.writes++
	if n.apply {
		if p.Model != nil {
			n.current.Model = *p.Model
		}
		if p.ReasoningEffort != nil {
			n.current.ReasoningEffort = p.ReasoningEffort
		}
		if p.ServiceTier != nil {
			n.current.ServiceTier, n.current.ServiceTierKnown = p.ServiceTier.Value, true
		}
	}
	return n.writeErr
}

func settingsFixture(t *testing.T) (Commands, SessionSettings, *settingsNativeFixture, InboundCommand) {
	t.Helper()
	c, s, in, now := appCommandFixture(t)
	in.UserID, in.ChatID, in.ChatType, in.ThreadID = "ou_operator", "chat", "group", "thread"
	n := &settingsNativeFixture{apply: true, current: codexclient.ThreadSettings{ThreadID: "thread", Model: "fixture-one", ReasoningEffort: settingString("high"), ServiceTier: settingString("default"), ServiceTierKnown: true},
		models: []codexclient.Model{
			{ID: "fixture-one", Model: "fixture-one", DefaultReasoningEffort: "high", SupportedReasoningEfforts: []codexclient.ReasoningEffortOption{{ReasoningEffort: "low"}, {ReasoningEffort: "high"}}, ServiceTiers: []codexclient.ModelServiceTier{{ID: "priority"}}},
			{ID: "fixture-two", Model: "fixture-two", DefaultReasoningEffort: "medium", SupportedReasoningEfforts: []codexclient.ReasoningEffortOption{{ReasoningEffort: "medium"}, {ReasoningEffort: "max"}}, ServiceTiers: []codexclient.ModelServiceTier{{ID: "fast"}}},
		}}
	controller := SessionSettings{Store: s, Native: n, HostID: "host", AppID: "app", Operators: c.Config.Access.OperatorOpenIDs, Now: func() time.Time { return now }}
	n.beforeWrite = func(p codexclient.SettingsPatch) {
		t.Helper()
		rs, err := s.SessionSettingsRequests(context.Background(), store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
		if err != nil || len(rs) == 0 {
			t.Fatalf("missing pre-write journal: %v", err)
		}
		want, _ := codexclient.SettingsRequestJSON("thread", p)
		found := false
		for _, r := range rs {
			if r.State == "dispatching" && r.Payload == want {
				found = true
			}
		}
		if !found {
			t.Fatal("native settings write was not durably frozen")
		}
	}
	return c, controller, n, in
}

// Seed the schema-32 command journal directly: these entries are retained
// only for upgrade/restart recovery; the removed commands are not public APIs.
func enqueueLegacySettings(c Commands, ctx context.Context, in InboundCommand) error {
	fields := strings.Fields(in.Text)
	_, _, err := c.Store.ApplyLocalCommand(ctx, store.CommandRequest{
		HostID: c.Config.Host.ID, AppID: c.Config.Feishu.AppID,
		MessageID: in.MessageID, ChatID: in.ChatID, ChatType: in.ChatType,
		UserID: in.UserID, ThreadID: in.ThreadID, Operators: c.Config.Access.OperatorOpenIDs,
		Kind: "session_settings", Setting: strings.TrimPrefix(fields[0], "/"), SettingValue: strings.Join(fields[1:], " "),
	}, c.now())
	return err
}

func TestLegacySessionSettingsApplyOnceAndKeepOtherFields(t *testing.T) {
	for _, tc := range []struct{ command, model, effort, tier string }{
		{"/reasoning low", "fixture-one", "low", "default"},
		{"/fast on", "fixture-one", "high", "priority"},
		{"/model fixture-two", "fixture-two", "medium", "default"},
		{"/model fixture-two max", "fixture-two", "max", "default"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			c, s, n, in := settingsFixture(t)
			ctx := context.Background()
			in.Text = tc.command
			if err := enqueueLegacySettings(c, ctx, in); err != nil {
				t.Fatal(err)
			}
			if candidates, err := c.Store.CommandReplyCandidates(ctx, "host", s.nowMS(), 64); err != nil || len(candidates) != 0 {
				t.Fatal("pending command sent premature success", err)
			}
			if err := s.Tick(ctx, "thread"); err != nil {
				t.Fatal(err)
			}
			if n.writes != 1 || n.current.Model != tc.model || *n.current.ReasoningEffort != tc.effort || *n.current.ServiceTier != tc.tier {
				t.Fatalf("settings not applied: %+v, writes %d", n.current, n.writes)
			}
			reply, err := c.Store.CommandReply(ctx, "host", "app", in.MessageID)
			if err != nil || !strings.Contains(reply.Card, "会话设置已更新") || !strings.Contains(reply.Card, tc.model) {
				t.Fatal("missing confirmed settings", err)
			}
			if err := enqueueLegacySettings(c, ctx, in); err != nil {
				t.Fatal(err)
			}
			if err := s.Tick(ctx, "thread"); err != nil {
				t.Fatal(err)
			}
			if n.writes != 1 {
				t.Fatal("replayed command changed settings again")
			}
			var payload string
			if err := c.Store.DB.QueryRow(`SELECT payload FROM session_settings_requests WHERE message_id=?`, in.MessageID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal([]byte(payload), &fields); err != nil {
				t.Fatal(err)
			}
			for key := range fields {
				if key != "threadId" && key != "model" && key != "effort" && key != "serviceTier" {
					t.Fatal("unrelated configuration was overwritten")
				}
			}
		})
	}
}

func TestLegacySessionSettingsCatalogValidationAndReadOnly(t *testing.T) {
	for _, command := range []string{"/model", "/reasoning", "/fast", "/reasoning max", "/model missing", "/model fixture-two low"} {
		t.Run(command, func(t *testing.T) {
			c, s, n, in := settingsFixture(t)
			in.Text = command
			if err := enqueueLegacySettings(c, context.Background(), in); err != nil {
				t.Fatal(err)
			}
			if err := s.Tick(context.Background(), "thread"); err != nil {
				t.Fatal(err)
			}
			if n.writes != 0 {
				t.Fatal("query or unsupported choice mutated thread")
			}
			reply, _ := c.Store.CommandReply(context.Background(), "host", "app", in.MessageID)
			if !strings.Contains(reply.Card, "fixture-one") {
				t.Fatal("current model missing")
			}
		})
	}
	c, s, n, in := settingsFixture(t)
	in.Text = "/fast off"
	n.current.ServiceTier = settingString("priority")
	n.listErr = errors.New("unavailable")
	if err := enqueueLegacySettings(c, context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background(), "thread"); err != nil {
		t.Fatal(err)
	}
	if n.writes != 1 || *n.current.ServiceTier != "default" {
		t.Fatal("fast off must explicitly select standard tier")
	}
}

func TestLegacySessionSettingsUnknownRecoveryNeverResendsAndAllowsQueries(t *testing.T) {
	c, s, n, in := settingsFixture(t)
	ctx := context.Background()
	n.apply = false
	n.writeErr = &codexclient.UnknownError{Method: "thread/settings/update", Cause: errors.New("lost")}
	in.Text = "/reasoning low"
	if err := enqueueLegacySettings(c, ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx, "thread"); !errors.Is(err, errSessionSettingsPending) {
		t.Fatalf("unknown not pending: %v", err)
	}
	original, _ := c.Store.CommandReply(ctx, "host", "app", in.MessageID)
	if !strings.Contains(original.Card, "尚未确认") {
		t.Fatal("unknown reported success")
	}
	reply, claimed, err := c.Store.ClaimCommandReply(ctx, "host", s.nowMS())
	if err != nil || !claimed {
		t.Fatal("unknown result not deliverable", err)
	}
	if err := c.Store.FinishCommandReply(ctx, reply.ID, "confirmed", "settings-response"); err != nil {
		t.Fatal(err)
	}
	now := s.Now().Add(31 * time.Second)
	s.Now = func() time.Time { return now }
	if err := s.Tick(ctx, "thread"); !errors.Is(err, errSessionSettingsPending) {
		t.Fatal(err)
	}
	if n.writes != 1 {
		t.Fatal("recovery repeated unknown write")
	}
	in.MessageID, in.Text = "show-current", "/model"
	if err := enqueueLegacySettings(c, ctx, in); err != nil {
		t.Fatal(err)
	}
	in.MessageID, in.Text = "blocked-change", "/fast on"
	if err := enqueueLegacySettings(c, ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx, "thread"); !errors.Is(err, errSessionSettingsPending) {
		t.Fatal(err)
	}
	shown, _ := c.Store.CommandReply(ctx, "host", "app", "show-current")
	if !strings.Contains(shown.Card, "仍在核验") || !strings.Contains(shown.Card, "fixture-one") {
		t.Fatal("read-only query blocked by unknown")
	}
	blocked, _ := c.Store.CommandReply(ctx, "host", "app", "blocked-change")
	if !strings.Contains(blocked.Card, "未发送新的修改") || n.writes != 1 {
		t.Fatal("new mutation bypassed unknown")
	}
	n.current.ReasoningEffort = settingString("low")
	now = now.Add(31 * time.Second)
	if err := s.Tick(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	if n.writes != 1 {
		t.Fatal("readback confirmation reissued mutation")
	}
	after, _ := c.Store.CommandReply(ctx, "host", "app", "command")
	if after.Card != original.Card {
		t.Fatal("already dispatched reply was rewritten")
	}
	pending, err := c.Store.SessionSettingsRequests(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if err != nil || len(pending) != 0 {
		t.Fatal("confirmed readback left pending operation", err)
	}
}

func TestLegacySessionSettingsLostAckCanConfirmByReadback(t *testing.T) {
	c, s, n, in := settingsFixture(t)
	in.Text = "/fast on"
	n.writeErr = &codexclient.UnknownError{Method: "thread/settings/update", Cause: errors.New("lost")}
	if err := enqueueLegacySettings(c, context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background(), "thread"); err != nil {
		t.Fatal(err)
	}
	if n.writes != 1 {
		t.Fatal("unexpected writes")
	}
	reply, _ := c.Store.CommandReply(context.Background(), "host", "app", in.MessageID)
	if !strings.Contains(reply.Card, "已核验") || !strings.Contains(reply.Card, "fast") {
		t.Fatal("matching readback not reflected")
	}
}

func TestSessionSettingsRejectsUnauthorizedWrongScopeAndInvalidSyntax(t *testing.T) {
	for _, variant := range []string{"private", "recipient", "stranger", "bad-model", "removed-fast", "removed-reasoning", "revoked", "guard"} {
		t.Run(variant, func(t *testing.T) {
			c, s, n, in := settingsFixture(t)
			in.Text = "/model"
			switch variant {
			case "private":
				in.ChatType, in.ChatID, in.ThreadID = "p2p", "private", ""
			case "recipient":
				in.UserID = "ou_notify"
			case "stranger":
				in.UserID = "ou_stranger"
			case "bad-model":
				in.Text = "/model fixture-two"
			case "removed-fast":
				in.Text = "/fast on"
			case "removed-reasoning":
				in.Text = "/reasoning high unexpected"
			case "revoked":
				s.Operators = nil
			case "guard":
				s.BeginWrite = func(context.Context, string) (context.Context, func(), error) { return nil, nil, errors.New("denied") }
			}
			if err := c.Handle(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			if err := s.Tick(context.Background(), "thread"); err != nil && !errors.Is(err, errSessionSettingsPending) {
				t.Fatal(err)
			}
			if n.writes != 0 {
				t.Fatal("unauthorized settings mutation")
			}
		})
	}
}

func TestLegacySessionSettingsCrashAfterClaimOnlyReadsOriginalPayload(t *testing.T) {
	c, s, n, in := settingsFixture(t)
	in.Text = "/reasoning low"
	ctx := context.Background()
	if err := enqueueLegacySettings(c, ctx, in); err != nil {
		t.Fatal(err)
	}
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	requests, _ := c.Store.SessionSettingsRequests(ctx, scope)
	payload, _ := codexclient.SettingsRequestJSON("thread", codexclient.SettingsPatch{ReasoningEffort: settingString("low")})
	if claimed, err := c.Store.ClaimSessionSettings(ctx, requests[0], payload); err != nil || !claimed {
		t.Fatal(err)
	}
	n.current.ReasoningEffort = settingString("low")
	if err := s.Tick(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	if n.writes != 0 {
		t.Fatal("dispatching request was replayed after crash")
	}
}

type slowSettingsServiceNative struct {
	*serviceNative
	settingsReads, interrupts int
}

func (n *slowSettingsServiceNative) ReadThreadSettings(ctx context.Context, _ string) (codexclient.ThreadSettings, error) {
	n.settingsReads++
	<-ctx.Done()
	return codexclient.ThreadSettings{}, ctx.Err()
}
func (*slowSettingsServiceNative) ListModels(context.Context) ([]codexclient.Model, error) {
	return nil, errors.New("unexpected catalog read")
}
func (*slowSettingsServiceNative) UpdateThreadSettings(context.Context, string, codexclient.SettingsPatch) error {
	return errors.New("unexpected settings write")
}
func (n *slowSettingsServiceNative) Interrupt(ctx context.Context, thread, turn string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n.interrupts++
	n.mu.Lock()
	defer n.mu.Unlock()
	n.turns[thread][0].Status = "interrupted"
	return nil
}

func TestServiceSettingsTimeoutStillProcessesStopAndPersistsFailure(t *testing.T) {
	db, cfg, root := serviceFixture(t)
	native := &slowSettingsServiceNative{serviceNative: serviceTestNative(root)}
	service := &Service{Store: db, Config: cfg, Gateway: &serviceGateway{runtimeGateway: &runtimeGateway{}}}
	commands := Commands{Store: db, Config: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	in := InboundCommand{MessageID: "slow-settings", ChatID: "chat", ChatType: "group", UserID: "ou_operator", ThreadID: "thread", Text: "/model"}
	if err := commands.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.MessageID, in.Text = "stop-during-settings", "/stop"
	if err := commands.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	runtime := service.generation(native)
	if err := runtime.ActorTick(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	if native.interrupts != 1 || ctx.Err() != nil {
		t.Fatal("settings timeout starved stop")
	}
	var requestID string
	if err := db.DB.QueryRowContext(ctx, `SELECT id FROM session_settings_requests WHERE message_id='slow-settings'`).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	mirror, err := db.Mirror(ctx, cfg.Host.ID, "thread", store.SessionSettingsCardKey(requestID))
	if err != nil || !mirror.Terminal || !strings.Contains(mirror.Card, "读取失败") {
		t.Fatal("timeout result not persisted on settings card", err)
	}
	if err := runtime.ActorTick(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	if native.settingsReads != 1 {
		t.Fatal("failed read repeatedly consumed actor budget")
	}
}
