package store

import (
	"context"
	"testing"
	"time"

	"codex-feishu/internal/cards"
)

func settingsStoreRequest(t *testing.T) (*Store, string, CommandRequest) {
	t.Helper()
	s, path, _ := taskControlStore(t, true)
	return s, path, CommandRequest{HostID: "host", AppID: "app", ThreadID: "thread", ChatID: "chat", ChatType: "group", MessageID: "settings", UserID: "operator", Operators: []string{"operator"}, Kind: "session_settings", Setting: "fast", SettingValue: "on"}
}

func TestSessionSettingsRequestAtomicDedupAndSingleClaim(t *testing.T) {
	s, _, req := settingsStoreRequest(t)
	ctx := context.Background()
	now := time.Unix(200, 0)
	reply, fresh, err := s.ApplyLocalCommand(ctx, req, now)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	duplicate, fresh, err := s.ApplyLocalCommand(ctx, req, now.Add(time.Second))
	if err != nil || fresh || reply.ID != duplicate.ID {
		t.Fatal("command replay changed identity", err)
	}
	scope := SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	requests, err := s.SessionSettingsRequests(ctx, scope)
	if err != nil || len(requests) != 1 {
		t.Fatal("duplicate settings operation", err)
	}
	if _, claimed, err := s.ClaimCommandReply(ctx, "host", now.UnixMilli()); err != nil || claimed {
		t.Fatal("pending native operation delivered reply", err)
	}
	wrong := requests[0]
	wrong.ThreadID = "other"
	if claimed, err := s.ClaimSessionSettings(ctx, wrong, `{"threadId":"other","serviceTier":"priority"}`); err == nil || claimed {
		t.Fatal("wrong thread claimed request")
	}
	payload := `{"threadId":"thread","serviceTier":"priority"}`
	if claimed, err := s.ClaimSessionSettings(ctx, requests[0], payload); err != nil || !claimed {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimSessionSettings(ctx, requests[0], payload); err != nil || claimed {
		t.Fatal("request claimed twice", err)
	}
	req.MessageID = "rollback"
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_settings BEFORE INSERT ON session_settings_requests BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplyLocalCommand(ctx, req, now); err == nil {
		t.Fatal("failed settings insert accepted command")
	}
	if _, err := s.CommandReply(ctx, "host", "app", req.MessageID); err == nil {
		t.Fatal("partial command survived rollback")
	}
}

func TestSessionSettingsMigrationPreservesExistingRepliesAndSources(t *testing.T) {
	s, path, req := settingsStoreRequest(t)
	ctx := context.Background()
	now := time.Unix(200, 0)
	req.Kind, req.Feedback = "feedback", "existing reply"
	before, _, err := s.ApplyLocalCommand(ctx, req, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DROP TABLE session_settings_requests; DROP TABLE reply_settings_sources; PRAGMA user_version=31`); err != nil {
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
	after, err := next.CommandReply(ctx, "host", "app", req.MessageID)
	if err != nil || before != after {
		t.Fatal("schema migration changed existing command", err)
	}
	first, err := next.SaveReplySettingsSource(ctx, "host", "thread", "turn", "Codex · first settings")
	if err != nil {
		t.Fatal(err)
	}
	second, err := next.SaveReplySettingsSource(ctx, "host", "thread", "turn", "Codex · later settings")
	if err != nil || first != second {
		t.Fatal("snapshot changed after later settings", err)
	}
	sources, err := next.ReplySettingsSources(ctx, "host", "thread")
	if err != nil || sources["turn"] != first {
		t.Fatal("snapshot not persisted", err)
	}
}

func TestSessionSettingsUnknownRetainsFrozenPayloadAndDelaysReadback(t *testing.T) {
	s, _, req := settingsStoreRequest(t)
	ctx := context.Background()
	now := time.Unix(200, 0)
	if _, _, err := s.ApplyLocalCommand(ctx, req, now); err != nil {
		t.Fatal(err)
	}
	scope := SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	requests, _ := s.SessionSettingsRequests(ctx, scope)
	payload := `{"threadId":"thread","serviceTier":"priority"}`
	if claimed, err := s.ClaimSessionSettings(ctx, requests[0], payload); err != nil || !claimed {
		t.Fatal(err)
	}
	if err := s.FinishSessionSettings(ctx, requests[0], "unknown", cards.Message("设置", "Codex", "正在核验"), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	pending, err := s.SessionSettingsRequests(ctx, scope)
	if err != nil || len(pending) != 1 || pending[0].State != "unknown" || pending[0].Payload != payload || pending[0].NextCheckAtMS != now.UnixMilli()+30000 {
		t.Fatal("unknown lost frozen request or backoff", err)
	}
	if claimed, err := s.ClaimSessionSettings(ctx, pending[0], payload); err != nil || claimed {
		t.Fatal("unknown made writable again", err)
	}
	if _, claimed, err := s.ClaimCommandReply(ctx, "host", now.UnixMilli()); err != nil || !claimed {
		t.Fatal("unknown feedback not available", err)
	}
}
