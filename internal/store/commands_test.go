package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/rules"
)

func commandSettingsSections(t *testing.T, card string) (string, string, string) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(card), &doc); err != nil {
		t.Fatal(err)
	}
	els := doc["body"].(map[string]any)["elements"].([]any)
	if len(els) != 4 {
		t.Fatalf("settings hierarchy: %d elements", len(els))
	}
	panel := els[2].(map[string]any)
	if panel["tag"] != "collapsible_panel" || panel["expanded"] != false {
		t.Fatal("command details are not natively collapsed")
	}
	return els[0].(map[string]any)["content"].(string), els[1].(map[string]any)["content"].(string), panel["elements"].([]any)[0].(map[string]any)["content"].(string)
}

func TestCommandNotificationShowsCurrentPolicyRatherThanOnlyEnabledFlag(t *testing.T) {
	for _, variant := range []string{"allowed", "off_with_snooze", "snooze", "quiet", "expired_snooze_with_quiet", "on_keeps_quiet"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			s, req, now := commandStoreRequest(t)
			p, _ := rules.DefaultPolicy("Asia/Shanghai")
			want := "当前允许提醒你"
			action := rules.NotifyStatus
			switch variant {
			case "off_with_snooze":
				p.Enabled, p.SnoozeUntil, p.QuietStart, p.QuietEnd = false, now.Add(2*time.Hour), "17:00", "19:00"
				want = "长期关闭"
			case "snooze":
				p.SnoozeUntil = now.Add(2 * time.Hour)
				want = "临时暂停"
			case "quiet":
				p.QuietStart, p.QuietEnd = "17:00", "19:00"
				want = "每日免打扰"
			case "expired_snooze_with_quiet":
				p.SnoozeUntil, p.QuietStart, p.QuietEnd = now.Add(-time.Hour), "17:00", "19:00"
				want = "每日免打扰"
			case "on_keeps_quiet":
				p.Enabled, p.SnoozeUntil, p.QuietStart, p.QuietEnd = false, now.Add(2*time.Hour), "17:00", "19:00"
				action, want = rules.NotifyOn, "每日免打扰"
			}
			if _, err := s.InitNotificationPolicyAt(ctx, "app", "ou_notify", p, now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			req.Kind, req.UserID, req.NotificationTimezone, req.Notification = "notify", "ou_notify", p.Timezone, rules.NotificationCommand{Action: action}
			reply, _, err := s.ApplyLocalCommand(ctx, req, now)
			if err != nil {
				t.Fatal(err)
			}
			conclusion, summary, details := commandSettingsSections(t, reply.Card)
			if !strings.Contains(conclusion, want) || !strings.Contains(summary, "正文继续同步") {
				t.Fatalf("wrong current notification result: %q %q", conclusion, summary)
			}
			for _, text := range []string{"@ 提醒：", "临时暂停至：", "每日免打扰：", "时区：Asia/Shanghai", "本部署全部任务群", "仅修改你自己的设置", "恢复后仍遵守每日免打扰"} {
				if !strings.Contains(details, text) {
					t.Fatalf("missing folded policy detail %q", text)
				}
			}
			if variant == "snooze" && !strings.Contains(summary, "2026年10月12日 20:00（Asia/Shanghai）") {
				t.Fatalf("deadline is not a friendly local date: %q", summary)
			}
			if variant == "off_with_snooze" && !strings.Contains(details, "临时暂停结束后不会自动恢复") {
				t.Fatal("snooze falsely overrides permanent off")
			}
			stored, err := s.NotificationPolicy(ctx, "app", "ou_notify")
			if err != nil {
				t.Fatal(err)
			}
			if action == rules.NotifyStatus && !reflect.DeepEqual(stored, p) {
				t.Fatal("status rendering changed policy or revision")
			}
			if action == rules.NotifyOn && (!stored.Enabled || !stored.SnoozeUntil.IsZero() || stored.QuietStart != p.QuietStart || stored.QuietEnd != p.QuietEnd || stored.Revision != 1) {
				t.Fatalf("on changed its existing semantics: %+v", stored)
			}
		})
	}
}

func TestCommandDirectorySeparatesConfirmedRemovalFromRemainingRules(t *testing.T) {
	ctx := context.Background()
	s, req, now := commandStoreRequest(t)
	if _, _, err := s.ApplyLocalCommand(ctx, req, now); err != nil {
		t.Fatal(err)
	}
	recursive := req
	recursive.MessageID, recursive.Directory.Rule.Recursive = "recursive", true
	if _, _, err := s.ApplyLocalCommand(ctx, recursive, now); err != nil {
		t.Fatal(err)
	}
	remove := req
	remove.MessageID, remove.Directory.Action = "remove-exact", rules.DirectoryUnmute
	reply, _, err := s.ApplyLocalCommand(ctx, remove, now)
	if err != nil {
		t.Fatal(err)
	}
	conclusion, summary, details := commandSettingsSections(t, reply.Card)
	if conclusion != "对应目录规则已移除。" || !strings.Contains(summary, "仍命中其他 mute 规则") || !strings.Contains(details, "当前仍命中") || !strings.Contains(details, req.Directory.Rule.Path+"（递归）") || !strings.Contains(details, req.Directory.Rule.Path+"（精确）") {
		t.Fatalf("unmute lost remaining rule evidence: %q %q %q", conclusion, summary, details)
	}
	remove.MessageID = "remove-absent"
	absent, _, err := s.ApplyLocalCommand(ctx, remove, now)
	if err != nil {
		t.Fatal(err)
	}
	conclusion, summary, _ = commandSettingsSections(t, absent.Card)
	if strings.Contains(conclusion, "已移除") || !strings.Contains(conclusion, "不存在") || !strings.Contains(summary, "新会话不会自动建群") {
		t.Fatalf("absent rule falsely claimed removal or resumed groups: %q %q", conclusion, summary)
	}
	if !strings.Contains(summary, "已绑定任务继续同步") || !strings.Contains(summary, "不补建历史任务群") {
		t.Fatal("visible directory scope caveats were lost")
	}
	list := req
	list.MessageID, list.Directory = "list", rules.DirectoryCommand{Action: rules.DirectoryList}
	listed, _, err := s.ApplyLocalCommand(ctx, list, now)
	if err != nil {
		t.Fatal(err)
	}
	conclusion, _, details = commandSettingsSections(t, listed.Card)
	if conclusion != "当前有 1 条目录 mute 规则。" || !strings.Contains(details, req.Directory.Rule.Path+"（递归）") {
		t.Fatal("directory listing lost its rule count or folded full path")
	}
	remove.MessageID, remove.Directory.Rule.Recursive = "remove-recursive", true
	last, _, err := s.ApplyLocalCommand(ctx, remove, now)
	if err != nil {
		t.Fatal(err)
	}
	_, summary, details = commandSettingsSections(t, last.Card)
	if strings.Contains(summary, "仍命中") || !strings.Contains(details, "当前未命中其他 mute 规则") {
		t.Fatal("last rule removal retained a stale matching claim")
	}
	list.MessageID = "list-empty"
	listed, _, err = s.ApplyLocalCommand(ctx, list, now)
	if err != nil {
		t.Fatal(err)
	}
	conclusion, _, details = commandSettingsSections(t, listed.Card)
	if conclusion != "当前没有目录 mute 规则。" || !strings.Contains(details, "目录规则：无") {
		t.Fatal("empty listing lost its confirmed result")
	}
	claimed, fresh, err := s.ClaimCommandReply(ctx, req.HostID, now.UnixMilli()+1000)
	if err != nil || !fresh {
		t.Fatalf("claim: %t %v", fresh, err)
	}
	saved, err := s.CommandReply(ctx, req.HostID, req.AppID, claimed.MessageID)
	if err != nil || unmarkedWireCard(t, s, claimed.ID, claimed.Card) != saved.Card {
		t.Fatal("nested settings were not frozen consistently with the command receipt", err)
	}
}

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
