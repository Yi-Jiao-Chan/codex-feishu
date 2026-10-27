package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"codex-feishu/internal/cards"
)

func TestTaskCardHasControlRequiresUniqueButtonInNativeColumns(t *testing.T) {
	button := map[string]any{
		"tag": "button",
		"behaviors": []any{map[string]any{
			"type": "callback", "value": map[string]any{"action": "stop", "token": "opaque", "revision": 7},
		}},
	}
	columns := func(tag string, elements ...any) map[string]any {
		return map[string]any{"tag": "column_set", "columns": []any{map[string]any{"tag": tag, "elements": elements}}}
	}
	for _, tc := range []struct {
		name     string
		elements []any
		want     bool
	}{
		{"legacy top level", []any{button}, true},
		{"title row", []any{columns("column", button)}, true},
		{"duplicate across levels", []any{button, columns("column", button)}, false},
		{"duplicate within column", []any{columns("column", button, button)}, false},
		{"invalid column tag", []any{columns("markdown", button)}, false},
		{"non-container elements", []any{map[string]any{"tag": "markdown", "elements": []any{button}}}, false},
		{"non-container columns", []any{map[string]any{"tag": "markdown", "columns": []any{map[string]any{"tag": "column", "elements": []any{button}}}}}, false},
		{"callback on container", []any{map[string]any{"tag": "column_set", "behaviors": button["behaviors"]}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(map[string]any{"body": map[string]any{"elements": tc.elements}})
			if err != nil {
				t.Fatal(err)
			}
			card := string(encoded)
			if got := taskCardHasControl(card, "stop", "opaque", 7); got != tc.want {
				t.Fatalf("control accepted=%v, want %v", got, tc.want)
			}
			if taskCardHasControl(card, "continue", "opaque", 7) || taskCardHasControl(card, "stop", "other", 7) || taskCardHasControl(card, "stop", "opaque", 6) {
				t.Fatal("column traversal relaxed the action, token or revision match")
			}
		})
	}
	for _, state := range []struct{ status, kind string }{{"inProgress", "stop"}, {"interrupted", "continue"}} {
		card := cards.Task(cards.TaskView{Status: state.status, Kind: state.kind, State: "available", Token: "opaque", Revision: 7, QueueCount: 1, Body: "公开动作"})
		if !taskCardHasControl(card, state.kind, "opaque", 7) {
			t.Fatalf("actual %s renderer produced an unclaimable control", state.kind)
		}
	}
	if taskCardHasControl(`{"body":`, "stop", "opaque", 7) {
		t.Fatal("malformed card authorized a control")
	}
}

func taskControlStore(t *testing.T, bound bool) (*Store, string, SubmissionScope) {
	t.Helper()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	scope := SubmissionScope{"host", "app", "thread"}
	if bound {
		ctx := context.Background()
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
	}
	return s, path, scope
}

func taskControlView(t *testing.T, s *Store, scope SubmissionScope) TaskControl {
	t.Helper()
	list, err := s.TaskControls(context.Background(), scope)
	if err != nil || len(list) != 1 {
		t.Fatalf("task control unavailable: %d %v", len(list), err)
	}
	return list[0]
}

func stageTaskCard(t *testing.T, s *Store, scope SubmissionScope, v TaskControl, delivered bool, at int64) {
	t.Helper()
	ctx := context.Background()
	card := cards.Task(cards.TaskView{Status: v.NativeStatus, Token: v.Token, Revision: v.Revision, Kind: v.Kind, State: v.State, QueueCount: v.QueueCount})
	err := s.StageMirrors(ctx, scope.HostID, scope.ThreadID, []Mirror{{Key: "task-progress", TurnID: v.TurnID, Kind: "progress", Card: card, Terminal: v.NativeStatus != "inProgress", ParentMessageID: "original-input"}}, Watermark{TurnID: v.TurnID, ObservedAtMS: at})
	if err != nil {
		t.Fatal(err)
	}
	if delivered {
		m, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, "task-progress")
		if err != nil {
			t.Fatal(err)
		}
		a, claimed, err := s.ClaimMirror(ctx, scope.HostID, scope.ThreadID, m.Key, m.Revision, at*3000)
		if err != nil || !claimed {
			t.Fatalf("progress delivery was not claimed: %v %v", claimed, err)
		}
		if err = s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "actual-progress"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTaskControlPreparesEligibleUnboundButClaimRequiresConfirmedCard(t *testing.T) {
	s, _, scope := taskControlStore(t, false)
	ctx := context.Background()
	v := TaskControl{TurnID: "original-turn", NativeStatus: "inProgress", Kind: "stop", State: "available", QueueCount: -1, Fingerprint: "public"}
	if err := s.StageTaskControls(ctx, scope, []TaskControl{v}); err != nil {
		t.Fatal("eligible unbound turn cannot prepare", err)
	}
	got := taskControlView(t, s, scope)
	if got.Token == "" || got.Revision != 1 {
		t.Fatal("control lacks opaque stable identity")
	}
	if _, err := s.ClaimTaskControl(ctx, "host", "app", "chat", "actual-progress", "stop", got.Token, got.Revision); !errors.Is(err, ErrSubmissionStale) {
		t.Fatal("unbound callback was accepted", err)
	}
	for _, foreign := range []SubmissionScope{{"host", "foreign-app", "thread"}, {"foreign-host", "app", "thread"}, {"host", "app", "foreign-thread"}} {
		if err := s.StageTaskControls(ctx, foreign, []TaskControl{v}); !errors.Is(err, ErrConflict) {
			t.Fatal("foreign actor prepared controls", err)
		}
	}
	if err := s.StageTaskControls(ctx, scope, []TaskControl{v}); err != nil {
		t.Fatal(err)
	}
	if same := taskControlView(t, s, scope); same.Token != got.Token || same.Revision != got.Revision {
		t.Fatal("identical preparation changed card identity")
	}
}

func TestTaskControlClaimRequiresCurrentDeliveredProgressPayload(t *testing.T) {
	s, _, scope := taskControlStore(t, true)
	ctx := context.Background()
	v := TaskControl{TurnID: "original-turn", NativeStatus: "inProgress", Kind: "stop", State: "available", QueueCount: -1, Fingerprint: "first"}
	if err := s.StageTaskControls(ctx, scope, []TaskControl{v}); err != nil {
		t.Fatal(err)
	}
	first := taskControlView(t, s, scope)
	stageTaskCard(t, s, scope, first, true, 1)
	for _, args := range []struct{ host, app, chat, message, kind, token string }{
		{"other-host", "app", "chat", "actual-progress", "stop", first.Token},
		{"host", "other-app", "chat", "actual-progress", "stop", first.Token},
		{"host", "app", "other-chat", "actual-progress", "stop", first.Token},
		{"host", "app", "chat", "original-input", "stop", first.Token},
		{"host", "app", "chat", "other-progress", "stop", first.Token},
		{"host", "app", "chat", "actual-progress", "continue", first.Token},
		{"host", "app", "chat", "actual-progress", "stop", "fabricated"},
	} {
		if _, err := s.ClaimTaskControl(ctx, args.host, args.app, args.chat, args.message, args.kind, args.token, first.Revision); !errors.Is(err, ErrSubmissionStale) {
			t.Fatal("foreign callback scope or action accepted", err)
		}
	}
	v.Fingerprint = "second"
	if err := s.StageTaskControls(ctx, scope, []TaskControl{v}); err != nil {
		t.Fatal(err)
	}
	second := taskControlView(t, s, scope)
	// Prepare committed before Engine.Apply: the persisted authorization must
	// not authorize a revision absent from the actual delivered card.
	if _, err := s.ClaimTaskControl(ctx, "host", "app", "chat", "actual-progress", "stop", second.Token, second.Revision); !errors.Is(err, ErrSubmissionStale) {
		t.Fatal("unrendered revision was accepted", err)
	}
	stageTaskCard(t, s, scope, second, false, 2)
	if _, err := s.ClaimTaskControl(ctx, "host", "app", "chat", "actual-progress", "stop", second.Token, second.Revision); !errors.Is(err, ErrSubmissionStale) {
		t.Fatal("undelivered revision was accepted", err)
	}
	stageTaskCard(t, s, scope, second, true, 2)
	if _, err := s.ClaimTaskControl(ctx, "host", "app", "chat", "actual-progress", "stop", first.Token, first.Revision); !errors.Is(err, ErrSubmissionStale) {
		t.Fatal("old card revision was accepted", err)
	}
	if _, err := s.ClaimTaskControl(ctx, "host", "app", "chat", "actual-progress", "stop", second.Token, second.Revision); err != nil {
		t.Fatal("delivered matching progress callback was rejected", err)
	}
}

func TestTaskControlClaimAcceptsLegacyDeliveredCardBeforeCompaction(t *testing.T) {
	s, _, scope := taskControlStore(t, true)
	ctx := context.Background()
	v := TaskControl{TurnID: "original-turn", NativeStatus: "inProgress", Kind: "stop", State: "available", QueueCount: -1, Fingerprint: "legacy"}
	if err := s.StageTaskControls(ctx, scope, []TaskControl{v}); err != nil {
		t.Fatal(err)
	}
	shown := taskControlView(t, s, scope)
	stageTaskCard(t, s, scope, shown, true, 1)
	card := cards.Task(cards.TaskView{Status: shown.NativeStatus, Token: shown.Token, Revision: shown.Revision, Kind: shown.Kind, State: shown.State, QueueCount: shown.QueueCount})
	if _, err := s.DB.ExecContext(ctx, `UPDATE mirrors SET card_json=?,callback_proofs='' WHERE host_id=? AND thread_id=? AND mirror_key='task-progress'`, card, scope.HostID, scope.ThreadID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTaskControl(ctx, scope.HostID, scope.AppID, "chat", "actual-progress", shown.Kind, shown.Token, shown.Revision); err != nil {
		t.Fatal("legacy delivered task control was rejected before background compaction", err)
	}
}

func TestTaskControlConcurrentReplayAndLatePreparationCannotUndoClaim(t *testing.T) {
	s, path, scope := taskControlStore(t, true)
	ctx := context.Background()
	v := TaskControl{TurnID: "original-turn", NativeStatus: "inProgress", Kind: "stop", State: "available", QueueCount: -1, Fingerprint: "first"}
	if err := s.StageTaskControls(ctx, scope, []TaskControl{v}); err != nil {
		t.Fatal(err)
	}
	shown := taskControlView(t, s, scope)
	stageTaskCard(t, s, scope, shown, true, 1)
	var wg sync.WaitGroup
	claims := make(chan TaskControlClaim, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.ClaimTaskControl(ctx, "host", "app", "chat", "actual-progress", "stop", shown.Token, shown.Revision)
			claims <- c
			errs <- err
		}()
	}
	wg.Wait()
	close(claims)
	close(errs)
	var original TaskControlClaim
	for err := range errs {
		if err != nil {
			t.Fatal("concurrent replay was rejected", err)
		}
	}
	for c := range claims {
		if original.ExternalID == "" {
			original = c
		}
		if c != original {
			t.Fatal("concurrent callback produced another claim")
		}
	}
	if err := s.StageTaskControls(ctx, scope, []TaskControl{v}); err != nil {
		t.Fatal(err)
	}
	if got := taskControlView(t, s, scope); got.State != "pending" || got.Token != shown.Token {
		t.Fatal("late preparation made a claimed action clickable again")
	}
	a, err := s.ClaimThreadSubmissionActionExpected(ctx, scope, "stop", original.ExternalID, original.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveSubmissionAction(ctx, scope, a.ID, "rejected", "ended"); err != nil {
		t.Fatal(err)
	}
	a, err = s.SubmissionAction(ctx, scope, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishTaskControlClaim(ctx, scope, original, a); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, err := reopened.ClaimTaskControl(ctx, "host", "app", "chat", "actual-progress", "stop", shown.Token, shown.Revision)
	if err != nil || replay.ExternalID != original.ExternalID || replay.NativeActionID != a.ID || replay.State != "rejected" || replay.Result != "ended" || replay.TurnID != "original-turn" {
		t.Fatal("terminal callback replay lost its durable original result", err)
	}
}

func TestTaskControlRejectsInvalidNativeActionState(t *testing.T) {
	s, _, scope := taskControlStore(t, true)
	for _, v := range []TaskControl{
		{TurnID: "turn", NativeStatus: "completed", Kind: "stop", State: "available", QueueCount: -1},
		{TurnID: "turn", NativeStatus: "interrupted", Kind: "continue", State: "available", QueueCount: -1},
		{TurnID: "turn", NativeStatus: "interrupted", Kind: "continue", State: "available", QueueCount: 0},
		{TurnID: "turn", NativeStatus: "unproven", State: "unavailable", QueueCount: -1},
	} {
		if err := s.StageTaskControls(context.Background(), scope, []TaskControl{v}); !errors.Is(err, ErrConflict) {
			t.Fatal("unsupported native status/action proof was persisted", err)
		}
	}
}

func TestTaskControlUnknownNativeWorkBlocksUnrefreshedCardClaim(t *testing.T) {
	s, _, scope := taskControlStore(t, true)
	ctx := context.Background()
	v := TaskControl{TurnID: "original-turn", NativeStatus: "inProgress", Kind: "stop", State: "available", QueueCount: -1}
	if err := s.StageTaskControls(ctx, scope, []TaskControl{v}); err != nil {
		t.Fatal(err)
	}
	shown := taskControlView(t, s, scope)
	stageTaskCard(t, s, scope, shown, true, 1)
	a, err := s.ClaimThreadSubmissionAction(ctx, scope, "continue", "other-command")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PrepareSubmissionAction(ctx, scope, a.ID, "start", "", "queue", ""); err != nil {
		t.Fatal(err)
	}
	attempt, claimed, err := s.ClaimSubmissionAttempt(ctx, scope, "", a.ID, "start")
	if err != nil || !claimed {
		t.Fatal("native dispatch not claimed", err)
	}
	if err = s.FinishSubmissionAttempt(ctx, scope, attempt.ID, "unknown", "", -1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimTaskControl(ctx, "host", "app", "chat", "actual-progress", "stop", shown.Token, shown.Revision); !errors.Is(err, ErrSubmissionBusy) {
		t.Fatal("unrefreshed task card bypassed uncertain native work", err)
	}
	claims, err := s.TaskControlClaims(ctx, scope)
	if err != nil || len(claims) != 0 {
		t.Fatal("rejected busy card silently scheduled a deferred action", err)
	}
}
