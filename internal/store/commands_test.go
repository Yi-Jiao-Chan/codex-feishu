package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"codex-feishu/internal/rules"
)

func commandStoreRequest(t *testing.T) (*Store, CommandRequest, time.Time) {
	t.Helper()
	s, project := directoryStoreFixture(t)
	parsed, err := rules.ParseDirectoryCommand("/mute-dir " + project)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := rules.ResolveScope(context.Background(), project, []string{project})
	if err != nil || !scope.Allowed {
		t.Fatalf("scope: %+v %v", scope, err)
	}
	return s, CommandRequest{HostID: "host", AppID: "app", MessageID: "command", ChatID: "private", ChatType: "p2p", UserID: "ou_operator", Kind: "directory", Operators: []string{"ou_operator"}, NotificationTargets: []string{"ou_notify"}, AllowedRoots: []string{project}, ScopeRoots: []string{project}, Directory: parsed, DirectoryScope: scope}, time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
}

func TestLocalCommandDirectoryEffectDedupAndReplyAreAtomic(t *testing.T) {
	ctx := context.Background()
	s, req, now := commandStoreRequest(t)
	first, fresh, err := s.ApplyLocalCommand(ctx, req, now)
	if err != nil || !fresh {
		t.Fatalf("first: %+v %t %v", first, fresh, err)
	}
	remove := req
	remove.MessageID, remove.Directory.Action = "remove", rules.DirectoryUnmute
	if _, _, err := s.ApplyLocalCommand(ctx, remove, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	replay, fresh, err := s.ApplyLocalCommand(ctx, req, now.Add(time.Hour))
	if err != nil || fresh || !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay changed reply: %+v %t %v", replay, fresh, err)
	}
	remaining, err := s.DirectoryRules(ctx, "host", "app")
	if err != nil || len(remaining) != 0 {
		t.Fatalf("old replay reapplied mute: %+v %v", remaining, err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_command_reply BEFORE INSERT ON command_replies BEGIN SELECT RAISE(ABORT,'fixture reply failure'); END`); err != nil {
		t.Fatal(err)
	}
	req.MessageID = "rollback"
	if _, _, err := s.ApplyLocalCommand(ctx, req, now.Add(2*time.Second)); err == nil {
		t.Fatal("reply failure accepted effect")
	}
	remaining, err = s.DirectoryRules(ctx, "host", "app")
	if err != nil || len(remaining) != 0 {
		t.Fatalf("partial command effect survived: %+v %v", remaining, err)
	}
}

func TestLocalCommandPolicyAndAuditRollbackWithReply(t *testing.T) {
	ctx := context.Background()
	s, req, now := commandStoreRequest(t)
	p, _ := rules.DefaultPolicy("UTC")
	if _, err := s.InitNotificationPolicyAt(ctx, "app", "ou_notify", p, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	req.Kind, req.UserID, req.NotificationTimezone, req.Notification = "notify", "ou_notify", "UTC", rules.NotificationCommand{Action: rules.NotifyOff}
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_command_reply BEFORE INSERT ON command_replies BEGIN SELECT RAISE(ABORT,'fixture reply failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplyLocalCommand(ctx, req, now); err == nil {
		t.Fatal("reply failure accepted policy update")
	}
	current, err := s.NotificationPolicy(ctx, "app", "ou_notify")
	if err != nil || !current.Enabled || current.Revision != 0 {
		t.Fatalf("partial notification effect survived: %+v %v", current, err)
	}
	var snapshots int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_policy_history`).Scan(&snapshots); err != nil || snapshots != 1 {
		t.Fatalf("partial audit survived: %d %v", snapshots, err)
	}
}

func TestLocalCommandRejectsScopeAndActorForgeries(t *testing.T) {
	ctx := context.Background()
	s, req, now := commandStoreRequest(t)
	for _, alter := range []func(*CommandRequest){
		func(r *CommandRequest) { r.AppID = "other" },
		func(r *CommandRequest) { r.HostID = "other" },
		func(r *CommandRequest) { r.UserID = "stranger" },
		func(r *CommandRequest) { r.UserID = "ou_notify" },
		func(r *CommandRequest) { r.DirectoryScope.Allowed = false },
		func(r *CommandRequest) { r.AllowedRoots = nil },
		func(r *CommandRequest) { r.ThreadID = "forged"; r.ChatType = "group" },
	} {
		invalid := req
		alter(&invalid)
		if _, _, err := s.ApplyLocalCommand(ctx, invalid, now); err == nil {
			t.Errorf("forged command accepted: %+v", invalid)
		}
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_replies`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("forged command journalled: %d %v", count, err)
	}
}

func TestCommandReplyUnknownAfterRestartIsNeverClaimed(t *testing.T) {
	ctx := context.Background()
	s, req, now := commandStoreRequest(t)
	req.Kind = "help"
	r, _, err := s.ApplyLocalCommand(ctx, req, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, fresh, err := s.ClaimCommandReply(ctx, "host", now.UnixMilli()+1000)
	if err != nil || !fresh || claimed.ID != r.ID || claimed.MessageID != req.MessageID || unmarkedWireCard(t, s, claimed.ID, claimed.Card) != r.Card {
		t.Fatalf("wrong frozen reply: %+v %t %v", claimed, fresh, err)
	}
	var sequence int
	var name, path string
	if err := s.DB.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverCommandDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := s.ClaimCommandReply(ctx, "host", now.UnixMilli()+2000); err != nil || fresh {
		t.Fatalf("ambiguous reply claimed again: %t %v", fresh, err)
	}
	if err := s.FinishCommandReply(ctx, r.ID, "confirmed", "remote"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCommandReply(ctx, r.ID, "confirmed", "different"); !errors.Is(err, ErrConflict) {
		t.Fatalf("contradictory confirmation accepted: %v", err)
	}
}
