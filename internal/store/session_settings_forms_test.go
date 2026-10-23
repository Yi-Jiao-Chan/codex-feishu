package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"codex-feishu/internal/cards"
)

func settingsFormCard(phase string, f SessionSettingsForm) string {
	elements := []any{map[string]any{"tag": "markdown", "content": phase}}
	if phase == "editing" {
		elements = append(elements, map[string]any{"tag": "button", "text": map[string]any{"tag": "plain_text", "content": "save"}, "behaviors": []any{map[string]any{"type": "callback", "value": map[string]any{"action": "model_settings_save", "token": f.Token, "revision": f.Revision}}}})
	}
	b, _ := json.Marshal(map[string]any{"schema": "2.0", "config": map[string]any{"update_multi": true}, "body": map[string]any{"elements": elements}})
	return string(b)
}

func settingsFormRequest(t *testing.T, s *Store, id string) SessionSettingsRequest {
	t.Helper()
	r, err := scanSessionSettings(s.DB.QueryRow(`SELECT `+sessionSettingsColumns+` FROM session_settings_requests WHERE id=?`, id))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func openingSettingsForm(t *testing.T) (*Store, string, SubmissionScope, CommandRequest, SessionSettingsRequest, SessionSettingsForm, int64) {
	t.Helper()
	s, path, scope := taskControlStore(t, true)
	now := time.Unix(200, 0)
	command := CommandRequest{HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID, ChatID: "chat", ChatType: "group", MessageID: "open-settings", UserID: "operator", Operators: []string{"operator"}, Kind: "model_settings"}
	reply, fresh, err := s.ApplyLocalCommand(context.Background(), command, now)
	if err != nil || !fresh || reply.State != "confirmed" || reply.Card != "" {
		t.Fatalf("open: %+v %v %v", reply, fresh, err)
	}
	requests, err := s.SessionSettingsRequests(context.Background(), scope)
	if err != nil || len(requests) != 1 {
		t.Fatalf("loading request: %+v %v", requests, err)
	}
	f, err := s.SessionSettingsForm(context.Background(), requests[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, path, scope, command, requests[0], f, now.UnixMilli()
}

func editSettingsForm(t *testing.T, s *Store, r SessionSettingsRequest, f SessionSettingsForm) SessionSettingsForm {
	t.Helper()
	next := f
	next.Phase = "editing"
	next.Revision++
	next.Snapshot = `{"ThreadID":"thread","Model":"model-one"}`
	next.Catalog = `[{"id":"model-one","model":"model-one"}]`
	next.Selection = `{"model":"model-one","effort":"high","fast":"keep"}`
	if err := s.StageSessionSettingsForm(context.Background(), r, f, next, settingsFormCard("editing", next)); err != nil {
		t.Fatal(err)
	}
	return next
}

func deliverSettingsForm(t *testing.T, s *Store, r SessionSettingsRequest, at int64) MirrorAttempt {
	t.Helper()
	ctx := context.Background()
	m, err := s.Mirror(ctx, r.HostID, r.ThreadID, SessionSettingsCardKey(r.ID))
	if err != nil {
		t.Fatal(err)
	}
	a, claimed, err := s.ClaimMirror(ctx, r.HostID, r.ThreadID, m.Key, m.Revision, at)
	if err != nil || !claimed {
		t.Fatalf("mirror not claimed: %v %v", claimed, err)
	}
	remote := "settings-card"
	if a.Operation == "update" && a.TargetMessageID != remote {
		t.Fatal("update lost original remote ID")
	}
	if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", remote); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestModelSettingsOpeningIsSilentAtomicAndDeduplicated(t *testing.T) {
	s, _, scope, command, r, f, now := openingSettingsForm(t)
	ctx := context.Background()
	if f.Phase != "loading" || f.Revision != 1 || f.ActionRevision != 0 || f.Token == "" || f.RequestID != r.ID || f.ExpiresAtMS != now+3600000 || f.Snapshot != "{}" || f.Catalog != "[]" || f.Selection != "{}" {
		t.Fatalf("wrong initial form: %+v", f)
	}
	before, err := s.CommandReply(ctx, scope.HostID, scope.AppID, command.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	again, fresh, err := s.ApplyLocalCommand(ctx, command, time.UnixMilli(now+1000))
	if err != nil || fresh || again != before {
		t.Fatal("opening replay changed receipt", err)
	}
	for _, table := range []string{"command_replies", "session_settings_requests", "session_settings_forms", "mirrors", "outbound"} {
		var count int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count %d: %v", table, count, err)
		}
	}
	if _, ok, err := s.ClaimCommandReply(ctx, scope.HostID, now); err != nil || ok {
		t.Fatal("silent opening entered send-only outbox", err)
	}
	m, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, SessionSettingsCardKey(r.ID))
	if err != nil || m.RemoteID != "" || m.ParentMessageID != command.MessageID || m.TurnID != m.Key || m.Terminal {
		t.Fatalf("wrong loading mirror %+v %v", m, err)
	}
	if _, err := s.Watermark(ctx, scope.HostID, scope.ThreadID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("opening advanced history watermark", err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_settings_form BEFORE INSERT ON session_settings_forms BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	command.MessageID = "rolled-back-opening"
	if _, _, err := s.ApplyLocalCommand(ctx, command, time.UnixMilli(now)); err == nil {
		t.Fatal("failed form creation accepted command")
	}
	if _, err := s.CommandReply(ctx, scope.HostID, scope.AppID, command.MessageID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("partial form opening survived", err)
	}
}

func TestModelSettingsEditingIsExcludedAndProjectionUsesCAS(t *testing.T) {
	s, _, scope, _, r, f, _ := openingSettingsForm(t)
	next := editSettingsForm(t, s, r, f)
	if requests, err := s.SessionSettingsRequests(context.Background(), scope); err != nil || len(requests) != 0 {
		t.Fatal("editing form busy loops worker", err)
	}
	if err := s.StageSessionSettingsForm(context.Background(), r, f, next, settingsFormCard("stale", next)); !errors.Is(err, ErrConflict) {
		t.Fatal("stale render restored old form", err)
	}
	actual, err := s.SessionSettingsFormByToken(context.Background(), scope, f.Token)
	if err != nil || actual != next {
		t.Fatal("CAS damaged stored projection", err)
	}
	foreign := scope
	foreign.ThreadID = "other"
	if _, err := s.SessionSettingsFormByToken(context.Background(), foreign, f.Token); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("token lookup crossed thread", err)
	}
	before, err := s.Mirror(context.Background(), scope.HostID, scope.ThreadID, SessionSettingsCardKey(r.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StageDisconnectedCards(context.Background(), scope, "offline"); err != nil {
		t.Fatal(err)
	}
	after, err := s.Mirror(context.Background(), scope.HostID, scope.ThreadID, before.Key)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("generic offline rendering overwrote settings form", err)
	}
}

func TestModelSettingsActionRejectsWrongScopeIdentityAndDelivery(t *testing.T) {
	for _, variant := range []string{"host", "app", "thread", "opener", "chat", "forwarded", "token", "revision", "expiry", "undelivered"} {
		t.Run(variant, func(t *testing.T) {
			s, _, scope, _, r, f, now := openingSettingsForm(t)
			f = editSettingsForm(t, s, r, f)
			if variant != "undelivered" {
				deliverSettingsForm(t, s, r, now+3000)
			}
			user, chat, message, token, revision := "operator", "chat", "settings-card", f.Token, f.Revision
			switch variant {
			case "host":
				scope.HostID = "other"
			case "app":
				scope.AppID = "other"
			case "thread":
				scope.ThreadID = "other"
			case "opener":
				user = "other-operator"
			case "chat":
				chat = "other-chat"
			case "forwarded":
				message = "forwarded-card"
			case "token":
				token = "other-token"
			case "revision":
				revision++
			case "expiry":
				now = f.ExpiresAtMS
			}
			if _, ok, err := s.ClaimSessionSettingsFormAction(context.Background(), scope, user, chat, message, token, revision, "save", `{"model":"model-two"}`, settingsFormCard("saving", f), now); err == nil || ok {
				t.Fatalf("%s action accepted: %v %v", variant, ok, err)
			}
			actual, err := s.SessionSettingsForm(context.Background(), r.ID)
			if err != nil || actual != f || settingsFormRequest(t, s, r.ID).Value != "" {
				t.Fatal("denied callback changed durable state", err)
			}
		})
	}
}

func TestModelSettingsFormRequiresLatestDeliveredRevision(t *testing.T) {
	s, _, scope, _, r, f, now := openingSettingsForm(t)
	deliverSettingsForm(t, s, r, now)
	f = editSettingsForm(t, s, r, f)
	if _, ok, err := s.ClaimSessionSettingsFormAction(context.Background(), scope, "operator", "chat", "settings-card", f.Token, f.Revision, "save", `{}`, settingsFormCard("saving", f), now+3000); err == nil || ok {
		t.Fatal("undelivered form revision accepted callback")
	}
	deliverSettingsForm(t, s, r, now+3000)
	if _, ok, err := s.ClaimSessionSettingsFormAction(context.Background(), scope, "operator", "chat", "settings-card", f.Token, f.Revision, "save", `{}`, settingsFormCard("saving", f), now+4000); err != nil || !ok {
		t.Fatal("delivered form not operable", err)
	}
}

func TestModelSettingsSaveAndCancelHaveOneConcurrentWinner(t *testing.T) {
	s, _, scope, _, r, f, now := openingSettingsForm(t)
	f = editSettingsForm(t, s, r, f)
	deliverSettingsForm(t, s, r, now)
	type result struct {
		f   SessionSettingsForm
		ok  bool
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, action := range []string{"save", "cancel"} {
		wg.Add(1)
		go func(action string) {
			defer wg.Done()
			<-start
			got, ok, err := s.ClaimSessionSettingsFormAction(context.Background(), scope, "operator", "chat", "settings-card", f.Token, f.Revision, action, `{"model":"model-two"}`, settingsFormCard(action, f), now+3000)
			results <- result{got, ok, err}
		}(action)
	}
	close(start)
	wg.Wait()
	close(results)
	winners, stale := 0, 0
	for result := range results {
		if result.ok && result.err == nil {
			winners++
		} else if errors.Is(result.err, ErrSubmissionStale) {
			stale++
		} else {
			t.Fatalf("unexpected result %+v", result)
		}
	}
	if winners != 1 || stale != 1 {
		t.Fatalf("save/cancel winners %d stale %d", winners, stale)
	}
	got, err := s.SessionSettingsForm(context.Background(), r.ID)
	if err != nil || got.Revision != f.Revision+1 || got.ActionRevision != f.Revision || got.Phase != "saving" && got.Phase != "canceling" {
		t.Fatalf("winning action not frozen: %+v %v", got, err)
	}
	m, err := s.Mirror(context.Background(), r.HostID, r.ThreadID, SessionSettingsCardKey(r.ID))
	if err != nil || m.Revision != 3 || m.RemoteID != "settings-card" {
		t.Fatal("competing action staged twice", err)
	}
}

func TestModelSettingsDuplicateActionsAreImmutableThroughCompletion(t *testing.T) {
	for _, action := range []string{"save", "cancel"} {
		t.Run(action, func(t *testing.T) {
			s, _, scope, _, r, f, now := openingSettingsForm(t)
			f = editSettingsForm(t, s, r, f)
			deliverSettingsForm(t, s, r, now)
			selection := `{"model":"model-two","effort":"high","fast":"off"}`
			pending := settingsFormCard("pending", f)
			claimed, ok, err := s.ClaimSessionSettingsFormAction(context.Background(), scope, "operator", "chat", "settings-card", f.Token, f.Revision, action, selection, pending, now+3000)
			if err != nil || !ok {
				t.Fatal(err)
			}
			duplicate, ok, err := s.ClaimSessionSettingsFormAction(context.Background(), scope, "operator", "chat", "settings-card", f.Token, f.Revision, action, selection, pending, f.ExpiresAtMS+1)
			if err != nil || ok || duplicate != claimed {
				t.Fatal("duplicate changed first action", err)
			}
			if action == "save" {
				if _, ok, err := s.ClaimSessionSettingsFormAction(context.Background(), scope, "operator", "chat", "settings-card", f.Token, f.Revision, action, `{"model":"other"}`, pending, now+3000); !errors.Is(err, ErrSubmissionStale) || ok {
					t.Fatal("conflicting duplicate accepted", err)
				}
			}
			r = settingsFormRequest(t, s, r.ID)
			if action == "cancel" {
				if r.Value != SessionSettingsCancelValue || claimed.Selection != "{}" {
					t.Fatal("cancel accepted form inputs")
				}
				if ok, err := s.ClaimSessionSettings(context.Background(), r, `{"threadId":"thread","model":"model-two"}`); err == nil || ok {
					t.Fatal("cancel marker became native write")
				}
			}
			next := claimed
			next.Phase = "closed"
			next.Revision++
			terminal := settingsFormCard("closed", next)
			if err := s.FinishSessionSettingsForm(context.Background(), r, claimed, next, "confirmed", terminal, now+4000); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishSessionSettingsForm(context.Background(), r, claimed, next, "confirmed", terminal, now+4001); err != nil {
				t.Fatal("completion not idempotent", err)
			}
			duplicate, ok, err = s.ClaimSessionSettingsFormAction(context.Background(), scope, "operator", "chat", "settings-card", f.Token, f.Revision, action, selection, pending, f.ExpiresAtMS+1)
			if err != nil || ok || duplicate != next {
				t.Fatal("late duplicate reopened terminal form", err)
			}
			if requests, err := s.SessionSettingsRequests(context.Background(), scope); err != nil || len(requests) != 0 {
				t.Fatal("closed form remained worker candidate", err)
			}
		})
	}
}

func TestModelSettingsNativeCrashRecoveryNeverReclaimsFrozenMutation(t *testing.T) {
	s, path, scope, _, r, f, now := openingSettingsForm(t)
	f = editSettingsForm(t, s, r, f)
	deliverSettingsForm(t, s, r, now)
	f, ok, err := s.ClaimSessionSettingsFormAction(context.Background(), scope, "operator", "chat", "settings-card", f.Token, f.Revision, "save", `{"model":"model-two"}`, settingsFormCard("saving", f), now+3000)
	if err != nil || !ok {
		t.Fatal(err)
	}
	r = settingsFormRequest(t, s, r.ID)
	payload := `{"threadId":"thread","model":"model-two"}`
	if ok, err := s.ClaimSessionSettings(context.Background(), r, payload); err != nil || !ok {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	requests, err := reopened.SessionSettingsRequests(context.Background(), scope)
	if err != nil || len(requests) != 1 || requests[0].State != "dispatching" || requests[0].Payload != payload {
		t.Fatalf("crash lost native journal: %+v %v", requests, err)
	}
	if ok, err := reopened.ClaimSessionSettings(context.Background(), requests[0], payload); err != nil || ok {
		t.Fatal("recovered mutation reissued", err)
	}
	next := f
	next.Revision++
	next.Snapshot = `{"Model":"model-one"}`
	if err := reopened.FinishSessionSettingsForm(context.Background(), r, f, next, "unknown", settingsFormCard("unknown", next), now+4000); err != nil {
		t.Fatal(err)
	}
	frozen := settingsFormRequest(t, reopened, r.ID)
	if frozen.State != "unknown" || frozen.Payload != payload || frozen.NextCheckAtMS != now+34000 {
		t.Fatal("unknown lost payload or deadline")
	}
	if ok, err := reopened.ClaimSessionSettings(context.Background(), frozen, payload); err != nil || ok {
		t.Fatal("unknown request made writable", err)
	}
}

func TestModelSettingsTerminalProjectionQueuesBehindFrozenCardAttempt(t *testing.T) {
	for _, outcome := range []string{"confirmed", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			s, _, scope, _, r, f, now := openingSettingsForm(t)
			f = editSettingsForm(t, s, r, f)
			deliverSettingsForm(t, s, r, now)
			f, ok, err := s.ClaimSessionSettingsFormAction(context.Background(), scope, "operator", "chat", "settings-card", f.Token, f.Revision, "cancel", `{}`, settingsFormCard("canceling", f), now+3000)
			if err != nil || !ok {
				t.Fatal(err)
			}
			r = settingsFormRequest(t, s, r.ID)
			m, err := s.Mirror(context.Background(), r.HostID, r.ThreadID, SessionSettingsCardKey(r.ID))
			if err != nil {
				t.Fatal(err)
			}
			a, ok, err := s.ClaimMirror(context.Background(), r.HostID, r.ThreadID, m.Key, m.Revision, now+3000)
			if err != nil || !ok {
				t.Fatal(err)
			}
			if a.Operation != "update" || a.TargetMessageID != "settings-card" {
				t.Fatal("pending projection created another message")
			}
			if outcome == "unknown" {
				if err := s.FinishMirrorAttempt(context.Background(), a.ID, "unknown", ""); err != nil {
					t.Fatal(err)
				}
			}
			next := f
			next.Phase = "closed"
			next.Revision++
			if err := s.FinishSessionSettingsForm(context.Background(), r, f, next, "confirmed", settingsFormCard("closed", next), now+4000); err != nil {
				t.Fatal(err)
			}
			latest, err := s.Mirror(context.Background(), r.HostID, r.ThreadID, m.Key)
			if err != nil {
				t.Fatal(err)
			}
			wantState := "dispatching"
			if outcome == "unknown" {
				wantState = "unknown"
			}
			if latest.State != wantState || latest.RemoteID != "settings-card" || latest.Revision <= a.Revision || !latest.Terminal {
				t.Fatalf("newer projection reopened frozen attempt %+v", latest)
			}
			if _, ok, err := s.ClaimMirror(context.Background(), r.HostID, r.ThreadID, m.Key, latest.Revision, now+10000); err != nil || ok {
				t.Fatal("frozen PATCH blindly retried", err)
			}
			frozen, err := s.MirrorAttempt(context.Background(), a.ID)
			if err != nil || frozen.Card != a.Card {
				t.Fatal("frozen card payload rewritten", err)
			}
			// This is the existing readback/ack confirmation path, not a resend.
			if err := s.FinishMirrorAttempt(context.Background(), a.ID, "confirmed", "settings-card"); err != nil {
				t.Fatal(err)
			}
			latest, err = s.Mirror(context.Background(), r.HostID, r.ThreadID, m.Key)
			if err != nil || latest.State != "ready" {
				t.Fatal("newer terminal projection not queued", err)
			}
			final, ok, err := s.ClaimMirror(context.Background(), r.HostID, r.ThreadID, m.Key, latest.Revision, now+10000)
			if err != nil || !ok || final.Operation != "update" || final.TargetMessageID != "settings-card" {
				t.Fatalf("terminal identity lost: %+v %v", final, err)
			}
		})
	}
}

func TestModelSettingsSchema32UpgradePreservesLegacyJournals(t *testing.T) {
	s, path, command := settingsStoreRequest(t)
	ctx := context.Background()
	now := time.Unix(200, 0)
	if _, _, err := s.ApplyLocalCommand(ctx, command, now); err != nil {
		t.Fatal(err)
	}
	scope := SubmissionScope{command.HostID, command.AppID, command.ThreadID}
	requests, err := s.SessionSettingsRequests(ctx, scope)
	if err != nil || len(requests) != 1 {
		t.Fatal(err)
	}
	r := requests[0]
	payload := `{"threadId":"thread","serviceTier":"priority"}`
	if ok, err := s.ClaimSessionSettings(ctx, r, payload); err != nil || !ok {
		t.Fatal(err)
	}
	if err := s.FinishSessionSettings(ctx, r, "unknown", cards.Message("settings", "Codex", "unknown"), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	r = settingsFormRequest(t, s, r.ID)
	beforeCommand, err := s.CommandReply(ctx, scope.HostID, scope.AppID, command.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StageMirrors(ctx, scope.HostID, scope.ThreadID, []Mirror{{Key: "legacy-progress", TurnID: "turn", Kind: "progress", Card: cards.Message("legacy", "Codex", "public"), ParentMessageID: command.MessageID}}, Watermark{TurnID: "turn", ObservedAtMS: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	m, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, "legacy-progress")
	if err != nil {
		t.Fatal(err)
	}
	a, ok, err := s.ClaimMirror(ctx, scope.HostID, scope.ThreadID, m.Key, m.Revision, now.UnixMilli())
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err := s.FinishMirrorAttempt(ctx, a.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	beforeMirror, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, m.Key)
	if err != nil {
		t.Fatal(err)
	}
	beforeAttempt, err := s.MirrorAttempt(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DROP TABLE session_settings_forms; PRAGMA user_version=32`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if got := settingsFormRequest(t, next, r.ID); got != r {
		t.Fatal("schema33 rewrote legacy native request")
	}
	afterCommand, err := next.CommandReply(ctx, scope.HostID, scope.AppID, command.MessageID)
	if err != nil || afterCommand != beforeCommand {
		t.Fatal("schema33 changed existing command reply", err)
	}
	afterMirror, err := next.Mirror(ctx, scope.HostID, scope.ThreadID, m.Key)
	if err != nil || !reflect.DeepEqual(afterMirror, beforeMirror) {
		t.Fatal("schema33 changed existing mirror", err)
	}
	afterAttempt, err := next.MirrorAttempt(ctx, a.ID)
	if err != nil || afterAttempt != beforeAttempt {
		t.Fatal("schema33 changed frozen outbound attempt", err)
	}
	var version, count int
	if err := next.DB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal("schema upgrade missing", err)
	}
	if err := next.DB.QueryRow(`SELECT COUNT(*) FROM session_settings_forms`).Scan(&count); err != nil || count != 0 {
		t.Fatal("legacy request became an invented form", err)
	}
	if got, err := next.SessionSettingsRequests(ctx, scope); err != nil || len(got) != 1 || got[0] != r {
		t.Fatal("legacy unknown no longer recoverable", err)
	}
}
