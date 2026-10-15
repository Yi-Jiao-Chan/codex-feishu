package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"codex-feishu/internal/cards"
)

// PendingRequestsSchema contains routing identities and fixed bridge feedback,
// never native commands, questions, permissions or answers.
const PendingRequestsSchema = `
CREATE TABLE app_requests (
 host_id TEXT NOT NULL, app_id TEXT NOT NULL, thread_id TEXT NOT NULL, request_key TEXT NOT NULL,
 turn_id TEXT NOT NULL, method TEXT NOT NULL, event_at INTEGER NOT NULL, replay INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','resolved')), confirmed_pending INTEGER NOT NULL,
 reminder_state TEXT NOT NULL CHECK(reminder_state IN ('none','ready','suppressed','dispatching','confirmed','unknown','rejected')),
 parent_key TEXT NOT NULL DEFAULT '', parent_message_id TEXT NOT NULL DEFAULT '',
 created_at_ms INTEGER NOT NULL, PRIMARY KEY(host_id,app_id,thread_id,request_key)
);
CREATE TABLE request_mentions (
 host_id TEXT NOT NULL, app_id TEXT NOT NULL, thread_id TEXT NOT NULL, request_key TEXT NOT NULL, open_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('eligible','suppressed','dispatching','confirmed','unknown','rejected')),
 reason TEXT NOT NULL, policy_revision INTEGER NOT NULL, attempt_id TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(host_id,app_id,thread_id,request_key,open_id),
 FOREIGN KEY(host_id,app_id,thread_id,request_key) REFERENCES app_requests(host_id,app_id,thread_id,request_key)
);
CREATE TABLE request_reminder_attempts (
 attempt_id TEXT PRIMARY KEY, host_id TEXT NOT NULL, app_id TEXT NOT NULL, thread_id TEXT NOT NULL, request_key TEXT NOT NULL,
 chat_id TEXT NOT NULL, parent_message_id TEXT NOT NULL, card_json TEXT NOT NULL, created_at_ms INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('dispatching','confirmed','unknown','rejected')), remote_id TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(host_id,app_id,thread_id,request_key) REFERENCES app_requests(host_id,app_id,thread_id,request_key)
);
`

type PendingRequest struct {
	Key, ThreadID, TurnID, Method string
	EventAt                       int64 // Unix seconds of first live observation; zero when provenance is unknown.
	Replay                        bool
}

func requestScope(ctx context.Context, tx *sql.Tx, host, app string) error {
	var actual string
	if err := tx.QueryRowContext(ctx, `SELECT app_id FROM deployment WHERE host_id=?`, host).Scan(&actual); err != nil {
		return err
	}
	if host == "" || app == "" || app != actual {
		return ErrConflict
	}
	return nil
}

// ObserveAppRequest freezes first eligibility before network delivery. Only the
// caller can prove live provenance; an unknown/replayed request never qualifies.
func (s *Store) ObserveAppRequest(ctx context.Context, host, app string, r PendingRequest, options MentionOptions, nowMS int64) error {
	if r.Key == "" || r.ThreadID == "" || r.TurnID == "" || nowMS <= 0 || r.EventAt < 0 || r.EventAt > nowMS/1000 {
		return errors.New("invalid App request routing")
	}
	switch r.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput":
	default:
		return errors.New("unsupported App request")
	}
	for _, id := range options.Targets {
		if !cards.ValidMentionID(id) {
			return errors.New("invalid notification target")
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = requestScope(ctx, tx, host, app); err != nil {
		return err
	}
	var authorized int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM thread_observations o WHERE host_id=? AND thread_id=? AND (state='eligible' OR EXISTS(SELECT 1 FROM bindings b WHERE b.host_id=o.host_id AND b.thread_id=o.thread_id))`, host, r.ThreadID).Scan(&authorized); err != nil {
		return err
	}
	if authorized != 1 {
		return ErrIneligible
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO app_requests(host_id,app_id,thread_id,request_key,turn_id,method,event_at,replay,state,confirmed_pending,reminder_state,created_at_ms) VALUES(?,?,?,?,?,?,?,?,'pending',1,'none',?) ON CONFLICT DO NOTHING`, host, app, r.ThreadID, r.Key, r.TurnID, r.Method, r.EventAt, r.Replay, nowMS)
	if err != nil {
		return err
	}
	fresh, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if fresh == 0 {
		var turn, method string
		if err = tx.QueryRowContext(ctx, `SELECT turn_id,method FROM app_requests WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=?`, host, app, r.ThreadID, r.Key).Scan(&turn, &method); err != nil {
			return err
		}
		if turn != r.TurnID || method != r.Method {
			return ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE app_requests SET confirmed_pending=1 WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=? AND state='pending'`, host, app, r.ThreadID, r.Key)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	seen := map[string]bool{}
	for _, id := range options.Targets {
		if seen[id] {
			continue
		}
		seen[id] = true
		reason := ""
		var revision int64
		switch {
		case !options.Enabled:
			reason = "disabled"
		case options.MentionOn != nil && !containsMentionID(options.MentionOn, "needs_user"):
			reason = "event_disabled"
		case r.Replay:
			reason = "replay"
		case r.EventAt <= 0:
			reason = "event_time_unknown"
		}
		if reason == "" {
			reason, revision, err = mentionEventPolicyReason(ctx, tx, app, id, r.EventAt)
			if err != nil {
				return err
			}
		}
		state := "eligible"
		if reason != "" {
			state = "suppressed"
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO request_mentions(host_id,app_id,thread_id,request_key,open_id,state,reason,policy_revision) VALUES(?,?,?,?,?,?,?,?)`, host, app, r.ThreadID, r.Key, id, state, reason, revision); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ResolveAppRequest cancels unattempted reminders; in-flight evidence remains
// frozen. A later resume cannot reopen an already resolved identity.
func (s *Store) ResolveAppRequest(ctx context.Context, host, app, thread, key string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = requestScope(ctx, tx, host, app); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE app_requests SET state='resolved',confirmed_pending=0,reminder_state=CASE WHEN reminder_state IN ('none','ready') THEN 'suppressed' ELSE reminder_state END WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=?`, host, app, thread, key); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE request_mentions SET state='suppressed',reason='resolved' WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=? AND state='eligible'`, host, app, thread, key); err != nil {
		return err
	}
	return tx.Commit()
}

// ReconcileAppRequests also closes durable requests that were not replayed in
// this connection. Terminal history is local evidence; no native reply is made.
func (s *Store) ReconcileAppRequests(ctx context.Context, host, app, thread string, terminalTurns []string) error {
	if len(terminalTurns) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = requestScope(ctx, tx, host, app); err != nil {
		return err
	}
	for _, turn := range terminalTurns {
		if turn == "" {
			return errors.New("invalid request turn routing")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE request_mentions SET state='suppressed',reason='resolved' WHERE host_id=? AND app_id=? AND thread_id=? AND state='eligible' AND request_key IN (SELECT request_key FROM app_requests WHERE host_id=? AND app_id=? AND thread_id=? AND turn_id=?)`, host, app, thread, host, app, thread, turn); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE app_requests SET state='resolved',confirmed_pending=0,reminder_state=CASE WHEN reminder_state IN ('none','ready') THEN 'suppressed' ELSE reminder_state END WHERE host_id=? AND app_id=? AND thread_id=? AND turn_id=?`, host, app, thread, turn); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// StageRequestReminders binds only to the initiating input already verified by
// complete-history reconciliation. The parent need not yet be delivered.
func (s *Store) StageRequestReminders(ctx context.Context, host, app, thread string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = requestScope(ctx, tx, host, app); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE app_requests SET parent_key=COALESCE((SELECT parent_key FROM mirrors m WHERE m.host_id=app_requests.host_id AND m.thread_id=app_requests.thread_id AND m.turn_id=app_requests.turn_id AND m.kind IN ('progress','final') ORDER BY m.rowid LIMIT 1),''),parent_message_id=COALESCE((SELECT parent_message_id FROM mirrors m WHERE m.host_id=app_requests.host_id AND m.thread_id=app_requests.thread_id AND m.turn_id=app_requests.turn_id AND m.kind IN ('progress','final') ORDER BY m.rowid LIMIT 1),''),reminder_state='ready' WHERE host_id=? AND app_id=? AND thread_id=? AND state='pending' AND confirmed_pending=1 AND reminder_state='none' AND EXISTS(SELECT 1 FROM request_mentions n WHERE n.host_id=app_requests.host_id AND n.app_id=app_requests.app_id AND n.thread_id=app_requests.thread_id AND n.request_key=app_requests.request_key AND n.state='eligible') AND EXISTS(SELECT 1 FROM mirrors m WHERE m.host_id=app_requests.host_id AND m.thread_id=app_requests.thread_id AND m.turn_id=app_requests.turn_id AND m.kind IN ('progress','final'))`, host, app, thread)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type RequestReminderAttempt struct {
	ID, HostID, AppID, ThreadID, Key, ChatID, ParentMessageID, Card, State, RemoteID string
	CreatedAtMS                                                                      int64
}

func (s *Store) RequestMentionDecisions(ctx context.Context, host, app, thread, key string) ([]MentionDecision, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT open_id,state,reason,policy_revision,attempt_id FROM request_mentions WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=? ORDER BY open_id`, host, app, thread, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MentionDecision
	for rows.Next() {
		var d MentionDecision
		if err := rows.Scan(&d.OpenID, &d.State, &d.Reason, &d.PolicyRevision, &d.AttemptID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ClaimRequestReminder checks live pending state, membership and current policy
// in one transaction, then freezes exactly one rendered reply before its RPC.
func (s *Store) ClaimRequestReminder(ctx context.Context, host, app, thread string, nowMS int64, options MentionOptions) (a RequestReminderAttempt, claimed bool, err error) {
	if nowMS <= 0 {
		return a, false, errors.New("invalid reminder dispatch time")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return a, false, err
	}
	defer tx.Rollback()
	if err = requestScope(ctx, tx, host, app); err != nil {
		return a, false, err
	}
	if err := pruneRequestReminderEligibility(ctx, tx, host, app, thread, nowMS, options); err != nil {
		return a, false, err
	}

	var busy int
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM app_requests WHERE host_id=? AND thread_id=? AND reminder_state='dispatching')+(SELECT COUNT(*) FROM outbound WHERE host_id=? AND thread_id=? AND state='dispatching')`, host, thread, host, thread).Scan(&busy); err != nil {
		return a, false, err
	}
	if busy != 0 {
		return RequestReminderAttempt{}, false, tx.Commit()
	}
	var parentKey string
	var eventAt int64
	var replay bool
	err = tx.QueryRowContext(ctx, `SELECT request_key,parent_key,parent_message_id,event_at,replay FROM app_requests WHERE host_id=? AND app_id=? AND thread_id=? AND state='pending' AND confirmed_pending=1 AND reminder_state='ready' AND (parent_key='' AND parent_message_id<>'' OR EXISTS(SELECT 1 FROM mirrors m WHERE m.host_id=app_requests.host_id AND m.thread_id=app_requests.thread_id AND m.mirror_key=app_requests.parent_key AND m.kind='human' AND m.remote_id<>'')) AND NOT EXISTS(SELECT 1 FROM request_reminder_throttles t WHERE t.host_id=app_requests.host_id AND t.app_id=app_requests.app_id AND t.thread_id=app_requests.thread_id AND t.request_key=app_requests.request_key AND t.next_retry_at_ms>?) AND NOT EXISTS(SELECT 1 FROM request_reminder_attempts old WHERE old.host_id=app_requests.host_id AND old.app_id=app_requests.app_id AND old.thread_id=app_requests.thread_id AND old.request_key=app_requests.request_key AND old.state IN ('dispatching','unknown')) ORDER BY created_at_ms,request_key LIMIT 1`, host, app, thread, nowMS).Scan(&a.Key, &parentKey, &a.ParentMessageID, &eventAt, &replay)
	if errors.Is(err, sql.ErrNoRows) {
		return RequestReminderAttempt{}, false, tx.Commit()
	}
	if err != nil {
		return a, false, err
	}
	err = tx.QueryRowContext(ctx, `SELECT chat_id FROM bindings WHERE host_id=? AND thread_id=?`, host, thread).Scan(&a.ChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return RequestReminderAttempt{}, false, nil
	}
	if err != nil {
		return a, false, err
	}
	if parentKey != "" {
		var kind string
		err = tx.QueryRowContext(ctx, `SELECT kind,remote_id FROM mirrors WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, thread, parentKey).Scan(&kind, &a.ParentMessageID)
		if err != nil {
			return a, false, err
		}
		if kind != "human" {
			return a, false, ErrConflict
		}
	}
	if a.ParentMessageID == "" {
		return RequestReminderAttempt{}, false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT open_id FROM request_mentions WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=? AND state='eligible' ORDER BY open_id`, host, app, thread, a.Key)
	if err != nil {
		return a, false, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return a, false, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return a, false, err
	}
	var selected []string
	for _, id := range ids {
		reason := ""
		var revision int64
		switch {
		case replay:
			reason = "replay"
		case eventAt <= 0 || eventAt > nowMS/1000:
			reason = "event_time_unknown"
		case !options.Enabled:
			reason = "disabled"
		case options.MentionOn != nil && !containsMentionID(options.MentionOn, "needs_user"):
			reason = "event_disabled"
		case !containsMentionID(options.Targets, id):
			reason = "target_removed"
		case options.ChatID != a.ChatID || !containsMentionID(options.MemberIDs, id):
			reason = "unreachable"
		}
		if reason == "" {
			reason, revision, err = mentionPolicyReason(ctx, tx, app, id, time.UnixMilli(nowMS))
			if err != nil {
				return a, false, err
			}
		}
		state := "eligible"
		if reason != "" {
			state = "suppressed"
		} else {
			selected = append(selected, id)
		}
		if err = reminderWriteOne(ctx, tx, `UPDATE request_mentions SET state=?,reason=?,policy_revision=? WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=? AND open_id=?`, state, reason, revision, host, app, thread, a.Key, id); err != nil {
			return a, false, err
		}
	}
	if len(selected) == 0 {
		_, err = tx.ExecContext(ctx, `UPDATE app_requests SET reminder_state='suppressed' WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=?`, host, app, thread, a.Key)
		if err != nil {
			return a, false, err
		}
		return RequestReminderAttempt{}, false, tx.Commit()
	}
	a.Card, err = cards.WithMentions(cards.Message("需要处理", "Codex App", "此轮正在等待审批或原生问题，请回 Codex App 处理。"), selected)
	if err != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE request_mentions SET state='suppressed',reason='render_failed' WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=? AND state='eligible'`, host, app, thread, a.Key); err != nil {
			return RequestReminderAttempt{}, false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE app_requests SET reminder_state='suppressed' WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=?`, host, app, thread, a.Key); err != nil {
			return RequestReminderAttempt{}, false, err
		}
		return RequestReminderAttempt{}, false, tx.Commit()
	}
	a.ID, a.HostID, a.AppID, a.ThreadID, a.CreatedAtMS, a.State = ID(), host, app, thread, nowMS, "dispatching"
	err = reminderWriteOne(ctx, tx, `INSERT INTO request_reminder_attempts(attempt_id,host_id,app_id,thread_id,request_key,chat_id,parent_message_id,card_json,created_at_ms,state) VALUES(?,?,?,?,?,?,?,?,?,'dispatching')`, a.ID, host, app, thread, a.Key, a.ChatID, a.ParentMessageID, a.Card, nowMS)
	if err == nil {
		err = freezeFollowAttemptTx(ctx, tx, a.ID, host, thread, a.ChatID, a.ParentMessageID)
	}
	if err == nil {
		a.Card, _, err = freezeCardRequestMarker(ctx, tx, "reminder", a.ID, a.Card)
	}
	if err == nil {
		err = reminderWriteOne(ctx, tx, `UPDATE request_reminder_attempts SET card_json=? WHERE attempt_id=?`, a.Card, a.ID)
	}
	if err == nil {
		err = reminderWriteOne(ctx, tx, `UPDATE app_requests SET reminder_state='dispatching' WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=?`, host, app, thread, a.Key)
	}
	if err == nil {
		for _, id := range selected {
			err = reminderWriteOne(ctx, tx, `UPDATE request_mentions SET state='dispatching',attempt_id=? WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=? AND open_id=? AND state='eligible'`, a.ID, host, app, thread, a.Key, id)
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		return RequestReminderAttempt{}, false, err
	}
	err = tx.Commit()
	return a, err == nil, err
}

func (s *Store) FinishRequestReminder(ctx context.Context, id, state, remote string) error {
	if state != "confirmed" && state != "unknown" && state != "rejected" || state == "confirmed" && remote == "" || state != "confirmed" && remote != "" {
		return errors.New("invalid request reminder outcome")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var host, app, thread, key, old, oldRemote string
	if err = tx.QueryRowContext(ctx, `SELECT host_id,app_id,thread_id,request_key,state,remote_id FROM request_reminder_attempts WHERE attempt_id=?`, id).Scan(&host, &app, &thread, &key, &old, &oldRemote); err != nil {
		return err
	}
	if old == "confirmed" || old == "rejected" {
		if old == state && oldRemote == remote {
			return nil
		}
		return ErrConflict
	}
	if err = reminderWriteOne(ctx, tx, `UPDATE request_reminder_attempts SET state=?,remote_id=? WHERE attempt_id=?`, state, remote, id); err != nil {
		return err
	}
	if err = reminderWriteOne(ctx, tx, `UPDATE app_requests SET reminder_state=? WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=?`, state, host, app, thread, key); err != nil {
		return err
	}
	var selected int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_mentions WHERE attempt_id=? AND state IN ('dispatching','unknown')`, id).Scan(&selected); err != nil {
		return err
	}
	if err = reminderWriteCount(ctx, tx, selected, `UPDATE request_mentions SET state=? WHERE attempt_id=? AND state IN ('dispatching','unknown')`, state, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM request_reminder_throttles WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=?`, host, app, thread, key); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecoverRequestReminders(ctx context.Context, host string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{`UPDATE request_reminder_attempts SET state='unknown' WHERE host_id=? AND state='dispatching'`, `UPDATE request_mentions SET state='unknown' WHERE host_id=? AND state='dispatching'`, `UPDATE app_requests SET confirmed_pending=0,reminder_state=CASE WHEN reminder_state='dispatching' THEN 'unknown' ELSE reminder_state END WHERE host_id=?`} {
		if _, err = tx.ExecContext(ctx, query, host); err != nil {
			return err
		}
	}
	return tx.Commit()
}
