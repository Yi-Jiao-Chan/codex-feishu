package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

type modelFormFixture struct {
	commands Commands
	actor    SessionSettings
	native   *settingsNativeFixture
	form     store.SessionSettingsForm
	receiver *Receiver
	outbound messages.Outbound
	gateway  *runtimeGateway
	remote   string
}

func openModelForm(t *testing.T) *modelFormFixture {
	t.Helper()
	c, actor, native, in := settingsFixture(t)
	ctx := context.Background()
	in.Text = "/model"
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	requests, err := c.Store.SessionSettingsRequests(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if err != nil || len(requests) != 1 {
		t.Fatal("opening replay created extra forms", err)
	}
	if err := actor.Tick(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	form, err := c.Store.SessionSettingsForm(ctx, requests[0].ID)
	if err != nil || form.Phase != "editing" || native.writes != 0 {
		t.Fatal("opening did not remain read-only", err)
	}
	if rs, err := c.Store.SessionSettingsRequests(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}); err != nil || len(rs) != 0 {
		t.Fatal("editing form remained in actor work queue", err)
	}
	if replies, err := c.Store.CommandReplyCandidates(ctx, "host", actor.nowMS(), 64); err != nil || len(replies) != 0 {
		t.Fatal("settings form also sent a command receipt", err)
	}
	gateway := &runtimeGateway{}
	outbound := messages.Outbound{Store: c.Store, Gateway: gateway}
	result, err := outbound.DispatchOne(ctx, "host", "thread", actor.nowMS())
	if err != nil || result.State != "confirmed" || result.MessageID == "" || len(gateway.writes) != 1 || gateway.writes[0].op != "reply" || gateway.writes[0].parent != in.MessageID {
		t.Fatal("form not delivered as one reply to command", err)
	}
	return &modelFormFixture{commands: c, actor: actor, native: native, form: form,
		receiver: &Receiver{Store: c.Store, Config: c.Config, ModelSettingsAction: c.ModelSettingsAction},
		outbound: outbound, gateway: gateway, remote: result.MessageID}
}

func modelFormValues(model, effort, fast string) map[string]any {
	return map[string]any{cards.ModelSettingsModelField: model, cards.ModelSettingsEffortField: effort, cards.ModelSettingsFastField: fast}
}

func (f *modelFormFixture) click(t *testing.T, action string, values map[string]any) string {
	t.Helper()
	event := receiverAction(t, "ou_operator", f.remote, f.form.Token, "", f.form.Revision)
	event.Event.Action.Value["action"] = action
	event.Event.Action.FormValue = values
	response, err := f.receiver.Action(context.Background(), event)
	if err != nil || response == nil || response.Toast == nil || response.Card != nil {
		t.Fatal("callback must only acknowledge the durable local claim", err)
	}
	return response.Toast.Content
}

func (f *modelFormFixture) mirror(t *testing.T) store.Mirror {
	t.Helper()
	m, err := f.commands.Store.Mirror(context.Background(), "host", "thread", store.SessionSettingsCardKey(f.form.RequestID))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestModelFormSaveAndCancelUpdateOriginalCard(t *testing.T) {
	for _, save := range []bool{true, false} {
		name := "cancel"
		if save {
			name = "save"
		}
		t.Run(name, func(t *testing.T) {
			f := openModelForm(t)
			action, want, writes := cards.ModelSettingsSaveAction, "fixture-two · max · fast", 1
			values := modelFormValues("fixture-two", "max", "on")
			if !save {
				action, want, writes = cards.ModelSettingsCancelAction, "fixture-two · medium", 0
				// Cancel must ignore even invalid drafts and show a fresh native read.
				values = modelFormValues("forged-model", "invalid", "forged-tier")
				f.native.current.Model = "fixture-two"
				f.native.current.ReasoningEffort = settingString("medium")
			}
			if got := f.click(t, action, values); !strings.Contains(got, "正在") || f.native.writes != 0 {
				t.Fatal("callback executed native write or did not acknowledge")
			}
			// An identical callback is harmless before and after completion.
			f.click(t, action, values)
			if err := f.actor.Tick(context.Background(), "thread"); err != nil {
				t.Fatal(err)
			}
			m := f.mirror(t)
			if f.native.writes != writes || !m.Terminal || m.RemoteID != f.remote || !strings.Contains(m.Card, want) || strings.Contains(m.Card, "form_action_type") {
				t.Fatalf("terminal card did not reflect actual settings: writes=%d card=%s", f.native.writes, m.Card)
			}
			if !save && !strings.Contains(m.Card, "已取消") || save && !strings.Contains(m.Card, "已保存") {
				t.Fatal("wrong terminal outcome")
			}
			result, err := f.outbound.DispatchOne(context.Background(), "host", "thread", f.actor.nowMS())
			if err != nil || result.State != "confirmed" || result.MessageID != f.remote || len(f.gateway.writes) != 2 || f.gateway.writes[1].op != "update" || f.gateway.writes[1].target != f.remote {
				t.Fatal("terminal created a replacement card instead of PATCH", err)
			}
			f.click(t, action, values)
			if err := f.actor.Tick(context.Background(), "thread"); err != nil || f.native.writes != writes {
				t.Fatal("duplicate callback repeated native mutation", err)
			}
			other := cards.ModelSettingsCancelAction
			if !save {
				other, values = cards.ModelSettingsSaveAction, modelFormValues("fixture-two", "max", "on")
			}
			if got := f.click(t, other, values); !strings.Contains(got, "已处理或失效") {
				t.Fatal("second action bypassed the first winner", got)
			}
			if save {
				var payload string
				if err := f.commands.Store.DB.QueryRow(`SELECT payload FROM session_settings_requests WHERE id=?`, f.form.RequestID).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if json.Unmarshal([]byte(payload), &fields) != nil || len(fields) != 4 {
					t.Fatal("save did not atomically submit the three selected settings")
				}
				for _, field := range []string{"threadId", "model", "effort", "serviceTier"} {
					if _, ok := fields[field]; !ok {
						t.Fatal("settings patch changed unrelated session policy")
					}
				}
			}
		})
	}
}

func TestModelFormValidationDoesNotConsumeDraftOrOverwriteChangedSettings(t *testing.T) {
	f := openModelForm(t)
	if got := f.click(t, cards.ModelSettingsSaveAction, modelFormValues("fixture-two", "low", "on")); !strings.Contains(got, "不支持此思考强度") {
		t.Fatal("unsupported model/effort combination was accepted", got)
	}
	form, err := f.commands.Store.SessionSettingsForm(context.Background(), f.form.RequestID)
	if err != nil || form.Phase != "editing" || form.Revision != f.form.Revision || f.native.writes != 0 {
		t.Fatal("invalid draft consumed the action", err)
	}
	f.click(t, cards.ModelSettingsSaveAction, modelFormValues("fixture-two", "max", "on"))
	// App changed the session after the card opened; do not overwrite it.
	f.native.current.ReasoningEffort = settingString("low")
	if err := f.actor.Tick(context.Background(), "thread"); err != nil {
		t.Fatal(err)
	}
	m := f.mirror(t)
	if f.native.writes != 0 || !m.Terminal || !strings.Contains(m.Card, "会话设置已变化") || !strings.Contains(m.Card, "fixture-one · low") {
		t.Fatal("stale form overwrote current settings")
	}
}

func TestModelFormCallbackAuthenticatesOpenerCardAndRevision(t *testing.T) {
	for _, variant := range []string{"other-operator", "forwarded-card", "bad-token", "old-revision", "expired", "offline"} {
		t.Run(variant, func(t *testing.T) {
			f := openModelForm(t)
			event := receiverAction(t, "ou_operator", f.remote, f.form.Token, "", f.form.Revision)
			event.Event.Action.Value["action"] = cards.ModelSettingsSaveAction
			event.Event.Action.FormValue = modelFormValues("fixture-two", "max", "on")
			switch variant {
			case "other-operator":
				f.commands.Config.Access.OperatorOpenIDs = append(f.commands.Config.Access.OperatorOpenIDs, "ou_other_operator")
				f.receiver.Config = f.commands.Config
				f.receiver.ModelSettingsAction = f.commands.ModelSettingsAction
				event.Event.Operator.OpenID = "ou_other_operator"
			case "forwarded-card":
				event.Event.Context.OpenMessageID = "forwarded-card"
			case "bad-token":
				event.Event.Action.Value["token"] = "wrong-token"
			case "old-revision":
				event.Event.Action.Value["revision"] = float64(f.form.Revision - 1)
			case "expired":
				f.commands.Now = func() time.Time { return time.UnixMilli(f.form.ExpiresAtMS) }
				f.receiver.ModelSettingsAction = f.commands.ModelSettingsAction
			case "offline":
				f.receiver.ActionAllowed = func(string) bool { return false }
			}
			response, err := f.receiver.Action(context.Background(), event)
			if err != nil || response == nil || response.Toast == nil || response.Toast.Content == "正在保存" {
				t.Fatal("invalid callback was accepted", err)
			}
			form, err := f.commands.Store.SessionSettingsForm(context.Background(), f.form.RequestID)
			if err != nil || form.Phase != "editing" || f.native.writes != 0 {
				t.Fatal("invalid callback changed the form", err)
			}
		})
	}
}

func TestModelFormUnknownSaveOnlyReconcilesAndPatchesSameCard(t *testing.T) {
	f := openModelForm(t)
	f.native.apply = false
	f.native.writeErr = &codexclient.UnknownError{Method: "thread/settings/update", Cause: errors.New("lost response")}
	f.click(t, cards.ModelSettingsSaveAction, modelFormValues("fixture-two", "max", "on"))
	ctx := context.Background()
	if err := f.actor.Tick(ctx, "thread"); !errors.Is(err, errSessionSettingsPending) {
		t.Fatal("unknown save not held for readback", err)
	}
	m := f.mirror(t)
	if m.Terminal || strings.Contains(m.Card, "已保存") || !strings.Contains(m.Card, "核验") || f.native.writes != 1 {
		t.Fatal("unknown save reported success")
	}
	// Progress updates obey the existing two-second card throttle.
	if result, err := f.outbound.DispatchOne(ctx, "host", "thread", f.actor.nowMS()+2000); err != nil || result.MessageID != f.remote {
		t.Fatal("unknown result not delivered on original card", err)
	}
	now := f.actor.Now().Add(31 * time.Second)
	f.actor.Now = func() time.Time { return now }
	if err := f.actor.Tick(ctx, "thread"); !errors.Is(err, errSessionSettingsPending) || f.native.writes != 1 {
		t.Fatal("unknown write was repeated", err)
	}
	f.native.current.Model = "fixture-two"
	f.native.current.ReasoningEffort = settingString("max")
	f.native.current.ServiceTier = settingString("fast")
	now = now.Add(31 * time.Second)
	if err := f.actor.Tick(ctx, "thread"); err != nil || f.native.writes != 1 {
		t.Fatal("readback confirmation repeated the save", err)
	}
	m = f.mirror(t)
	if !m.Terminal || m.RemoteID != f.remote || !strings.Contains(m.Card, "fixture-two · max · fast") || !strings.Contains(m.Card, "已保存") {
		t.Fatal("readback did not close original card")
	}
	result, err := f.outbound.DispatchOne(ctx, "host", "thread", now.UnixMilli())
	if err != nil || result.MessageID != f.remote || len(f.gateway.writes) != 3 || f.gateway.writes[2].op != "update" {
		t.Fatalf("reconciliation did not update original card: result=%+v writes=%d err=%v", result, len(f.gateway.writes), err)
	}
}
