package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/rules"
)

const CommandSchema = `
CREATE TABLE command_replies (
 app_id TEXT NOT NULL, message_id TEXT NOT NULL, host_id TEXT NOT NULL REFERENCES deployment(host_id),
 chat_id TEXT NOT NULL, chat_type TEXT NOT NULL, user_id TEXT NOT NULL, thread_id TEXT NOT NULL,
 kind TEXT NOT NULL, attempt_id TEXT NOT NULL UNIQUE, card_json TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('ready','dispatching','unknown','rejected','confirmed')),
 remote_id TEXT NOT NULL DEFAULT '', created_at_ms INTEGER NOT NULL, dispatched_at_ms INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(app_id,message_id)
);
`

// CommandRequest contains trusted configuration and parsed values, never raw
// inbound text. DirectoryScope must come from ResolveScope before this call.
type CommandRequest struct {
	SourceRootID, SourceThreadID                                 string
	HostID, AppID, MessageID, ChatID, ChatType, UserID, ThreadID string
	Kind, Feedback, NotificationTimezone                         string
	CreationEnabled                                              bool
	OwnerOpenID                                                  string
	FilePreviewEnabled                                           bool
	PreviewAccessCode                                            string `json:"-"`
	Operators, NotificationTargets, AllowedRoots                 []string
	ScopeRoots                                                   []string // Canonical roots used by ResolveScope.
	Directory                                                    rules.DirectoryCommand
	DirectoryScope                                               rules.Scope
	Notification                                                 rules.NotificationCommand
}

type CommandReply struct {
	ID, HostID, AppID, MessageID, ChatID, ChatType, UserID, ThreadID string
	Kind, Card, State, RemoteID                                      string
	CreatedAtMS, DispatchedAtMS                                      int64
}

const commandColumns = `attempt_id,host_id,app_id,message_id,chat_id,chat_type,user_id,thread_id,kind,card_json,state,remote_id,created_at_ms,dispatched_at_ms`

func scanCommandReply(row rowScanner) (r CommandReply, err error) {
	err = row.Scan(&r.ID, &r.HostID, &r.AppID, &r.MessageID, &r.ChatID, &r.ChatType, &r.UserID, &r.ThreadID, &r.Kind, &r.Card, &r.State, &r.RemoteID, &r.CreatedAtMS, &r.DispatchedAtMS)
	return
}

func (s *Store) CommandReply(ctx context.Context, host, app, message string) (CommandReply, error) {
	return scanCommandReply(s.DB.QueryRowContext(ctx, `SELECT `+commandColumns+` FROM command_replies WHERE host_id=? AND app_id=? AND message_id=?`, host, app, message))
}

// ApplyLocalCommand commits the command effect, dedup marker and rendered reply
// together. Duplicate messages return their original result without reapplying.
func (s *Store) ApplyLocalCommand(ctx context.Context, req CommandRequest, now time.Time) (CommandReply, bool, error) {
	if req.MessageID == "" || req.ChatID == "" || req.UserID == "" || req.ChatType != "group" && req.ChatType != "p2p" || now.UnixMilli() <= 0 {
		return CommandReply{}, false, errors.New("invalid command identity or time")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return CommandReply{}, false, err
	}
	defer tx.Rollback()
	if err := directoryDeployment(ctx, tx, req.HostID, req.AppID); err != nil {
		return CommandReply{}, false, err
	}
	saved, err := scanCommandReply(tx.QueryRowContext(ctx, `SELECT `+commandColumns+` FROM command_replies WHERE app_id=? AND message_id=?`, req.AppID, req.MessageID))
	if err == nil {
		if saved.HostID != req.HostID || saved.UserID != req.UserID || saved.ChatID != req.ChatID || saved.ChatType != req.ChatType {
			return CommandReply{}, false, ErrConflict
		}
		return saved, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CommandReply{}, false, err
	}
	err = nil
	operator := containsMentionID(req.Operators, req.UserID)
	recipient := containsMentionID(req.NotificationTargets, req.UserID)
	if !operator && !recipient {
		return CommandReply{}, false, ErrConflict
	}
	if req.ThreadID != "" {
		if req.ChatType != "group" {
			return CommandReply{}, false, ErrConflict
		}
		chat, err := submissionBinding(ctx, tx, SubmissionScope{req.HostID, req.AppID, req.ThreadID})
		if err != nil {
			return CommandReply{}, false, err
		}
		if chat != req.ChatID {
			return CommandReply{}, false, ErrConflict
		}
	}
	var body, settingsCard string
	silentReply := false
	switch req.Kind {
	case "preview":
		if !operator || req.OwnerOpenID == "" || req.UserID != req.OwnerOpenID || req.ChatType != "p2p" ||
			req.ThreadID != "" || !req.FilePreviewEnabled || req.PreviewAccessCode == "" {
			return CommandReply{}, false, ErrConflict
		}
		fence := "```"
		for strings.Contains(req.PreviewAccessCode, fence) {
			fence += "`"
		}
		settingsCard = cards.Message("文件预览", "仅本人私聊", "浏览器访问口令：\n"+fence+"\n"+req.PreviewAccessCode+"\n"+fence+"\n打开文件预览链接后，在登录页输入此口令。")
	case "directory":
		if !operator {
			return CommandReply{}, false, ErrConflict
		}
		settingsCard, err = applyCommandDirectory(ctx, tx, req)
	case "notify":
		if !recipient {
			return CommandReply{}, false, ErrConflict
		}
		p, readErr := scanNotificationPolicy(tx.QueryRowContext(ctx, `SELECT enabled,snooze_until,quiet_start,quiet_end,timezone,revision FROM notification_rules WHERE app_id=? AND open_id=?`, req.AppID, req.UserID))
		if readErr != nil {
			return CommandReply{}, false, readErr
		}
		if p.Timezone != req.NotificationTimezone {
			return CommandReply{}, false, ErrConflict
		}
		next, applyErr := rules.Apply(p, req.Notification, now)
		if applyErr != nil {
			return CommandReply{}, false, applyErr
		}
		if req.Notification.Action != rules.NotifyStatus {
			err = setNotificationPolicyTx(ctx, tx, req.AppID, req.UserID, p.Revision, next, now)
		}
		if err == nil {
			settingsCard = commandNotificationCard(next, now)
		}
	case "stop", "continue":
		if !operator || req.ThreadID == "" {
			return CommandReply{}, false, ErrConflict
		}
		scope := SubmissionScope{req.HostID, req.AppID, req.ThreadID}
		blocked, blockErr := submissionBlocked(ctx, tx, scope)
		if blockErr != nil {
			return CommandReply{}, false, blockErr
		}
		if blocked {
			body = "当前操作正在核验，未提交新的控制请求。请等待任务状态更新。"
		} else {
			expectedTurn := ""
			if req.Kind == "stop" {
				var hasCOT, unambiguous bool
				expectedTurn, hasCOT, unambiguous, err = cotStopTarget(ctx, tx, scope)
				if err != nil {
					return CommandReply{}, false, err
				}
				if hasCOT && !unambiguous {
					body = "当前执行状态尚未核验，未提交停止请求。请在 Codex App 停止，或等待状态更新。"
					break
				}
			}
			digest := sha256.Sum256([]byte(req.AppID + "\x00" + req.MessageID))
			token := "command-" + hex.EncodeToString(digest[:])
			_, err = tx.ExecContext(ctx, `INSERT INTO submission_actions(action_id,host_id,app_id,thread_id,token,revision,kind,stage,state,created_at_ms,expected_turn_id) VALUES(?,?,?,?,?,0,?,'prepare','ready',?,?)`, ID(), req.HostID, req.AppID, req.ThreadID, token, req.Kind, now.UnixMilli(), expectedTurn)
			if req.Kind == "stop" {
				body = "已收到停止请求，正在处理；结果以任务状态为准。"
				// CoT already carries the task state. Keep the command dedup
				// receipt and stop intent, without an extra success-receipt card.
				silentReply = expectedTurn != ""
			} else {
				body = "已收到继续处理队列的请求，正在核验；不会在没有队列时启动新任务。"
			}
		}
	case "help", "status":
		body = "一期从 Codex App 发起任务，第一条实际输入后自动建专属群；在已绑定群中普通消息进入同一会话的队列。\n这里暂不创建新任务。支持 /mute-dir、/unmute-dir、/notify at；已绑定群的操作者可用 /stop 和 /continue。"
		if req.CreationEnabled {
			body = "私聊机器人发送任务描述或 /new，即可选择目录并创建 Codex 会话。授权入口群可 @机器人发起；已绑定任务群用 /new 新建，普通 @ 消息继续原会话。\n支持 /mute-dir、/unmute-dir、/notify at；任务群或对应话题可用 /stop 和 /continue。"
		}
		if req.FilePreviewEnabled {
			body += "\n部署所有者可私聊发送 /preview 获取文件预览口令。"
		}
	case "feedback":
		body = req.Feedback
		if body == "" {
			return CommandReply{}, false, errors.New("command feedback required")
		}
	default:
		return CommandReply{}, false, errors.New("unsupported local command")
	}
	if err != nil {
		return CommandReply{}, false, err
	}
	card := settingsCard
	if card == "" && !silentReply {
		card = cards.Message("机器人设置", "本部署", body)
	}
	r := CommandReply{ID: ID(), HostID: req.HostID, AppID: req.AppID, MessageID: req.MessageID, ChatID: req.ChatID, ChatType: req.ChatType, UserID: req.UserID, ThreadID: req.ThreadID, Kind: req.Kind, Card: card, State: "ready", CreatedAtMS: now.UnixMilli()}
	if silentReply {
		// This completes only the local no-op reply delivery, not the native
		// stop. Its independently journaled action still has to be confirmed.
		r.State = "confirmed"
	}
	if len(r.Card) > cards.DefaultMaxCardBytes {
		return CommandReply{}, false, errors.New("command result exceeds card limit")
	}
	err = commandWriteOne(ctx, tx, `INSERT INTO command_replies(app_id,message_id,host_id,chat_id,chat_type,user_id,thread_id,kind,attempt_id,card_json,state,created_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, r.AppID, r.MessageID, r.HostID, r.ChatID, r.ChatType, r.UserID, r.ThreadID, r.Kind, r.ID, r.Card, r.State, r.CreatedAtMS)
	if err != nil {
		return CommandReply{}, false, err
	}
	if err := recordCommandSourceTargetTx(ctx, tx, req); err != nil {
		return CommandReply{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return CommandReply{}, false, err
	}
	return r, true, nil
}

func applyCommandDirectory(ctx context.Context, tx *sql.Tx, req CommandRequest) (string, error) {
	c := req.Directory
	removed := false
	if c.Action != rules.DirectoryList {
		if c.Action != rules.DirectoryMute && c.Action != rules.DirectoryUnmute || !req.DirectoryScope.Allowed || req.DirectoryScope.CWD != c.Rule.Path {
			return "", ErrConflict
		}
		canonical, err := rules.CanonicalDirectory(c.Rule.Path)
		if err != nil {
			return "", err
		}
		if canonical != c.Rule.Path {
			return "", ErrConflict
		}
		if len(req.AllowedRoots) == 0 || len(req.ScopeRoots) != len(req.AllowedRoots) {
			return "", ErrConflict
		}
		for index, configured := range req.AllowedRoots {
			root, err := rules.CanonicalDirectory(configured)
			if err != nil {
				return "", err
			}
			if root != req.ScopeRoots[index] {
				return "", ErrConflict
			}
		}
		if c.Action == rules.DirectoryMute {
			_, err = tx.ExecContext(ctx, `INSERT INTO directory_rules(host_id,app_id,canonical_path,recursive) VALUES(?,?,?,?) ON CONFLICT(host_id,app_id,canonical_path,recursive) DO NOTHING`, req.HostID, req.AppID, c.Rule.Path, c.Rule.Recursive)
		} else {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `DELETE FROM directory_rules WHERE host_id=? AND app_id=? AND canonical_path=? AND recursive=?`, req.HostID, req.AppID, c.Rule.Path, c.Rule.Recursive)
			if err == nil {
				var affected int64
				affected, err = result.RowsAffected()
				removed = affected > 0
			}
		}
		if err != nil {
			return "", err
		}
	} else if c.Rule != (rules.DirectoryRule{}) {
		return "", ErrConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT canonical_path,recursive FROM directory_rules WHERE host_id=? AND app_id=? ORDER BY canonical_path,recursive`, req.HostID, req.AppID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var rule rules.DirectoryRule
		if err := rows.Scan(&rule.Path, &rule.Recursive); err != nil {
			return "", err
		}
		if c.Action == rules.DirectoryList {
			lines = append(lines, commandDirectoryLabel(rule))
		} else if _, hit := rules.Muted(req.DirectoryScope, []rules.DirectoryRule{rule}); hit {
			lines = append(lines, commandDirectoryLabel(rule))
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	summary := "仅影响未来自动建群；已绑定任务继续同步，解除后不补建历史任务群。"
	body := "远程主机：" + req.HostID + "\n" + summary + "\n"
	view := cards.SettingsView{Summary: summary, DetailsTitle: "目录规则详情", Source: "机器人设置 · 本部署"}
	if c.Action == rules.DirectoryList {
		if len(lines) == 0 {
			view.Conclusion, view.Details = "当前没有目录 mute 规则。", body+"目录规则：无"
		} else {
			view.Conclusion, view.Details = fmt.Sprintf("当前有 %d 条目录 mute 规则。", len(lines)), body+"目录规则：\n"+strings.Join(lines, "\n")
		}
		return cards.Settings(view), nil
	}
	if c.Action == rules.DirectoryMute {
		view.Conclusion = "此目录的新会话不再自动建群。"
		body += "已保存 mute："
	} else {
		if removed {
			view.Conclusion = "对应目录规则已移除。"
			body += "已移除对应规则："
		} else {
			view.Conclusion = "对应目录规则不存在，无需移除。"
			body += "未找到对应规则："
		}
	}
	body += commandDirectoryLabel(c.Rule)
	if len(lines) == 0 {
		body += "\n该目录当前未命中其他 mute 规则。"
	} else {
		body += "\n当前仍命中：\n" + strings.Join(lines, "\n")
		if c.Action == rules.DirectoryUnmute {
			view.Summary = "此目录仍命中其他 mute 规则，新会话不会自动建群。\n" + summary
		}
	}
	view.Details = body
	return cards.Settings(view), nil
}

func commandDirectoryLabel(rule rules.DirectoryRule) string {
	mode := "精确"
	if rule.Recursive {
		mode = "递归"
	}
	return rule.Path + "（" + mode + "）"
}

func commandNotificationCard(p rules.Policy, now time.Time) string {
	state := "开启"
	if !p.Enabled {
		state = "长期关闭"
	}
	snooze, quiet := "无", "关闭"
	loc, _ := time.LoadLocation(p.Timezone)
	if !p.SnoozeUntil.IsZero() {
		snooze = p.SnoozeUntil.In(loc).Format("2006年1月2日 15:04") + "（" + p.Timezone + "）"
	}
	if p.QuietStart != "" {
		quiet = p.QuietStart + "–" + p.QuietEnd
	}
	reason, _ := p.Suppression(now)
	reasons := map[rules.SuppressionReason]string{rules.NotificationAllowed: "允许提醒", rules.SuppressedOff: "长期关闭", rules.SuppressedSnooze: "临时暂停", rules.SuppressedQuiet: "每日免打扰"}
	conclusions := map[rules.SuppressionReason]string{
		rules.NotificationAllowed: "@ 提醒已开启，当前允许提醒你。",
		rules.SuppressedOff:       "@ 提醒已长期关闭，当前不会提醒你。",
		rules.SuppressedSnooze:    "@ 提醒已临时暂停，当前不会提醒你。",
		rules.SuppressedQuiet:     "当前处于每日免打扰时段，不会 @ 提醒你。",
	}
	summary := "正文继续同步。"
	if reason == rules.SuppressedSnooze {
		summary = "暂停至 " + snooze + "；正文继续同步。"
	}
	text := fmt.Sprintf("@ 提醒：%s\n临时暂停至：%s\n每日免打扰：%s\n时区：%s\n当前：%s\n范围：本部署全部任务群，仅修改你自己的设置。正文继续同步；飞书普通群消息提醒仍由客户端设置控制。", state, snooze, quiet, p.Timezone, reasons[reason])
	text += "\n恢复后仍遵守每日免打扰；休息期间被抑制的 @ 不会积压补发。"
	if !p.Enabled && !p.SnoozeUntil.IsZero() {
		text += "\n仍长期关闭，临时暂停结束后不会自动恢复。"
	}
	return cards.Settings(cards.SettingsView{Conclusion: conclusions[reason], Summary: summary, DetailsTitle: "完整通知设置", Details: text, Source: "机器人设置 · 本部署"})
}

const commandReplyReady = `host_id=? AND state='ready' AND NOT EXISTS(SELECT 1 FROM command_reply_throttles t WHERE t.app_id=command_replies.app_id AND t.message_id=command_replies.message_id AND t.next_retry_at_ms>?) AND NOT EXISTS(SELECT 1 FROM command_reply_attempts a WHERE a.app_id=command_replies.app_id AND a.message_id=command_replies.message_id AND a.state IN ('dispatching','unknown'))`

// CommandReplyCandidates reads a bounded snapshot without claiming, changing
// attempt identities, or freezing a wire card. It shares dispatch filtering with
// ClaimCommandReply, including due throttles and unresolved prior attempts.
func (s *Store) CommandReplyCandidates(ctx context.Context, host string, nowMS int64, limit int) ([]CommandReply, error) {
	if host == "" || nowMS <= 0 || limit < 1 || limit > 64 {
		return nil, errors.New("invalid command candidate scope, time or limit")
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+commandColumns+` FROM command_replies WHERE `+commandReplyReady+` ORDER BY rowid LIMIT ?`, host, nowMS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CommandReply
	for rows.Next() {
		r, err := scanCommandReply(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ClaimCommandReplyCandidate can claim only the exact snapshot authorized by
// its caller. A changed, deferred, or competing claim never selects a new route.
func (s *Store) ClaimCommandReplyCandidate(ctx context.Context, candidate CommandReply, nowMS int64) (CommandReply, bool, error) {
	if candidate.HostID == "" || candidate.ID == "" || nowMS <= 0 {
		return CommandReply{}, false, errors.New("invalid command candidate or time")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return CommandReply{}, false, err
	}
	defer tx.Rollback()
	r, err := scanCommandReply(tx.QueryRowContext(ctx, `SELECT `+commandColumns+` FROM command_replies WHERE `+commandReplyReady+` AND attempt_id=?`, candidate.HostID, nowMS, candidate.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return CommandReply{}, false, nil
	}
	if err != nil {
		return CommandReply{}, false, err
	}
	if r != candidate {
		return CommandReply{}, false, nil
	}
	return claimCommandReplyTx(ctx, tx, r, nowMS)
}

func (s *Store) ClaimCommandReply(ctx context.Context, host string, nowMS int64) (CommandReply, bool, error) {
	if host == "" || nowMS <= 0 {
		return CommandReply{}, false, errors.New("invalid command dispatch scope or time")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return CommandReply{}, false, err
	}
	defer tx.Rollback()
	r, err := scanCommandReply(tx.QueryRowContext(ctx, `SELECT `+commandColumns+` FROM command_replies WHERE `+commandReplyReady+` ORDER BY rowid LIMIT 1`, host, nowMS))
	if errors.Is(err, sql.ErrNoRows) {
		return CommandReply{}, false, nil
	}
	if err != nil {
		return CommandReply{}, false, err
	}
	return claimCommandReplyTx(ctx, tx, r, nowMS)
}

func claimCommandReplyTx(ctx context.Context, tx *sql.Tx, r CommandReply, nowMS int64) (CommandReply, bool, error) {
	if err := directoryDeployment(ctx, tx, r.HostID, r.AppID); err != nil {
		return CommandReply{}, false, err
	}
	err := commandWriteOne(ctx, tx, `UPDATE command_replies SET state='dispatching',dispatched_at_ms=? WHERE attempt_id=? AND state='ready'`, nowMS, r.ID)
	if err != nil {
		return CommandReply{}, false, err
	}
	if err := commandWriteOne(ctx, tx, `INSERT INTO command_reply_attempts(attempt_id,host_id,app_id,message_id,chat_id,chat_type,user_id,thread_id,kind,card_json,created_at_ms,dispatched_at_ms,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'dispatching')`, r.ID, r.HostID, r.AppID, r.MessageID, r.ChatID, r.ChatType, r.UserID, r.ThreadID, r.Kind, r.Card, r.CreatedAtMS, nowMS); err != nil {
		return CommandReply{}, false, err
	}
	if err := freezeFollowAttemptTx(ctx, tx, r.ID, r.HostID, r.ThreadID, r.ChatID, r.MessageID); err != nil {
		return CommandReply{}, false, err
	}
	wire, _, err := freezeCardRequestMarker(ctx, tx, "command", r.ID, r.Card)
	if err != nil {
		return CommandReply{}, false, err
	}
	if err := commandWriteOne(ctx, tx, `UPDATE command_reply_attempts SET card_json=? WHERE attempt_id=?`, wire, r.ID); err != nil {
		return CommandReply{}, false, err
	}
	r.Card = wire
	if err := tx.Commit(); err != nil {
		return CommandReply{}, false, err
	}
	r.DispatchedAtMS = nowMS
	r.State = "dispatching"
	return r, true, nil
}

func (s *Store) FinishCommandReply(ctx context.Context, attemptID, state, remoteID string) error {
	if attemptID == "" || state != "confirmed" && state != "unknown" && state != "rejected" || state == "confirmed" && remoteID == "" || state != "confirmed" && remoteID != "" {
		return errors.New("invalid command reply evidence")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := s.commandReplyAttempt(ctx, tx, attemptID)
	legacy := errors.Is(err, sql.ErrNoRows)
	if err != nil && !legacy {
		return err
	}
	if !legacy && (a.State == "confirmed" || a.State == "rejected") {
		if a.State == state && a.RemoteID == remoteID {
			return nil
		}
		return ErrConflict
	}
	r, err := scanCommandReply(tx.QueryRowContext(ctx, `SELECT `+commandColumns+` FROM command_replies WHERE attempt_id=?`, attemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if r.State == "confirmed" || r.State == "rejected" {
		if r.State == state && r.RemoteID == remoteID {
			return nil
		}
		return ErrConflict
	}
	if r.State != "dispatching" && r.State != "unknown" {
		return ErrConflict
	}
	if !legacy {
		if a.AppID != r.AppID || a.MessageID != r.MessageID || a.HostID != r.HostID || a.State != "dispatching" && a.State != "unknown" {
			return ErrConflict
		}
		if err := commandWriteOne(ctx, tx, `UPDATE command_reply_attempts SET state=?,remote_id=? WHERE attempt_id=?`, state, remoteID, attemptID); err != nil {
			return err
		}
	}
	if err := commandWriteOne(ctx, tx, `UPDATE command_replies SET state=?,remote_id=? WHERE attempt_id=?`, state, remoteID, attemptID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM command_reply_throttles WHERE app_id=? AND message_id=?`, r.AppID, r.MessageID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecoverCommandDispatches(ctx context.Context, host string) error {
	if host == "" {
		return errors.New("command deployment required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE command_reply_attempts SET state='unknown' WHERE host_id=? AND state='dispatching'`, host); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE command_replies SET state='unknown' WHERE host_id=? AND state='dispatching'`, host); err != nil {
		return err
	}
	return tx.Commit()
}
