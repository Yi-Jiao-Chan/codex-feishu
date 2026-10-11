package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/rules"
)

const MentionsSchema = `
CREATE TABLE notification_policy_history (
 app_id TEXT NOT NULL, open_id TEXT NOT NULL, revision INTEGER NOT NULL,
 effective_at_ms INTEGER NOT NULL CHECK(effective_at_ms>0), enabled INTEGER NOT NULL,
 snooze_until TEXT NOT NULL, quiet_start TEXT NOT NULL, quiet_end TEXT NOT NULL, timezone TEXT NOT NULL,
 PRIMARY KEY(app_id,open_id,revision)
);
CREATE INDEX notification_policy_history_time ON notification_policy_history(app_id,open_id,effective_at_ms,revision);
CREATE TABLE mention_events (
 host_id TEXT NOT NULL, thread_id TEXT NOT NULL, mirror_key TEXT NOT NULL,
 PRIMARY KEY(host_id,thread_id,mirror_key),
 FOREIGN KEY(host_id,thread_id,mirror_key) REFERENCES mirrors(host_id,thread_id,mirror_key)
);
CREATE TABLE mentions (
 host_id TEXT NOT NULL, thread_id TEXT NOT NULL, mirror_key TEXT NOT NULL, open_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('eligible','suppressed','dispatching','confirmed','unknown','rejected')),
 reason TEXT NOT NULL, policy_revision INTEGER NOT NULL,
 attempt_id TEXT REFERENCES outbound_attempts(attempt_id),
 PRIMARY KEY(host_id,thread_id,mirror_key,open_id),
 FOREIGN KEY(host_id,thread_id,mirror_key) REFERENCES mention_events(host_id,thread_id,mirror_key)
);
CREATE INDEX mentions_attempt ON mentions(attempt_id);
`

// MentionOptions separates configured targets/events from dispatch-time proof.
// ScheduleMentions ignores ChatID and MemberIDs. Claim requires matching ChatID
// and verified members; an empty or failed lookup is never proof of reachability.
type MentionOptions struct {
	Enabled   bool
	Targets   []string
	MentionOn []string
	ChatID    string
	MemberIDs []string
}

type MentionDecision struct {
	OpenID, State, Reason, AttemptID string
	PolicyRevision                   int64
}

// ScheduleMentions must run immediately after first snapshot observation, before
// binding or dispatch. The marker freezes first configuration, including disabled
// notifications and an empty target list. Reconnects cannot add new recipients.
func (s *Store) ScheduleMentions(ctx context.Context, host, thread string, options MentionOptions) error {
	if host == "" || thread == "" {
		return errors.New("notification requires deployment and thread")
	}
	for _, id := range options.Targets {
		if !cards.ValidMentionID(id) {
			return errors.New("notification target must be a Feishu open ID")
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var app string
	var observedMS int64
	if err := tx.QueryRowContext(ctx, `SELECT d.app_id,w.observed_at_ms FROM deployment d JOIN history_watermarks w USING(host_id) WHERE d.host_id=? AND w.thread_id=?`, host, thread).Scan(&app, &observedMS); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+mirrorColumns+` FROM mirrors m JOIN outbound o USING(host_id,thread_id,mirror_key) WHERE m.host_id=? AND m.thread_id=? AND m.event_type!='' ORDER BY m.rowid`, host, thread)
	if err != nil {
		return err
	}
	var events []Mirror
	for rows.Next() {
		m, err := scanMirror(rows)
		if err != nil {
			rows.Close()
			return err
		}
		events = append(events, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, m := range events {
		result, err := tx.ExecContext(ctx, `INSERT INTO mention_events(host_id,thread_id,mirror_key) VALUES(?,?,?) ON CONFLICT(host_id,thread_id,mirror_key) DO NOTHING`, host, thread, m.Key)
		if err != nil {
			return err
		}
		fresh, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if fresh == 0 {
			continue
		}
		var attempted int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_attempts WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, thread, m.Key).Scan(&attempted); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, id := range options.Targets {
			if seen[id] {
				continue
			}
			seen[id] = true
			reason := mentionEventReason(m, options)
			if reason == "" && (m.EventAt <= 0 || m.EventAt > observedMS/1000) {
				reason = "event_time_unknown"
			}
			if reason == "" && (attempted != 0 || m.RemoteID != "") {
				reason = "already_attempted"
			}
			var revision int64
			if reason == "" {
				reason, revision, err = mentionEventPolicyReason(ctx, tx, app, id, m.EventAt)
				if err != nil {
					return err
				}
			}
			state := "eligible"
			if reason != "" {
				state = "suppressed"
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO mentions(host_id,thread_id,mirror_key,open_id,state,reason,policy_revision) VALUES(?,?,?,?,?,?,?)`, host, thread, m.Key, id, state, reason, revision); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) MentionDecisions(ctx context.Context, host, thread, key string) ([]MentionDecision, error) {
	return mentionDecisions(ctx, s.DB, host, thread, key)
}

func mentionDecisions(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, host, thread, key string) ([]MentionDecision, error) {
	rows, err := q.QueryContext(ctx, `SELECT open_id,state,reason,policy_revision,attempt_id FROM mentions WHERE host_id=? AND thread_id=? AND mirror_key=? ORDER BY open_id`, host, thread, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var decisions []MentionDecision
	for rows.Next() {
		var d MentionDecision
		var attempt sql.NullString
		if err := rows.Scan(&d.OpenID, &d.State, &d.Reason, &d.PolicyRevision, &attempt); err != nil {
			return nil, err
		}
		d.AttemptID = attempt.String
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}

func mentionEventReason(m Mirror, options MentionOptions) string {
	switch {
	case !options.Enabled:
		return "disabled"
	case m.Kind != "final" || !m.Terminal || m.EventType != "completed" && m.EventType != "failed":
		return "unsupported"
	case m.Replay:
		return "replay"
	case m.Retired:
		return "retired"
	case !containsMentionEvent(options.MentionOn, m.EventType):
		return "event_disabled"
	}
	return ""
}

func containsMentionEvent(events []string, event string) bool {
	if events == nil {
		return event == "completed" || event == "failed"
	}
	return containsMentionID(events, event)
}

func containsMentionID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

// SQL read errors abort dispatch; invalid policy data only suppresses mentions.
func mentionPolicyReason(ctx context.Context, tx *sql.Tx, app, user string, at time.Time) (string, int64, error) {
	var p rules.Policy
	var enabled int
	var snooze string
	err := tx.QueryRowContext(ctx, `SELECT enabled,snooze_until,quiet_start,quiet_end,timezone,revision FROM notification_rules WHERE app_id=? AND open_id=?`, app, user).Scan(&enabled, &snooze, &p.QuietStart, &p.QuietEnd, &p.Timezone, &p.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "policy_missing", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if enabled != 0 && enabled != 1 {
		return "invalid", p.Revision, nil
	}
	p.Enabled = enabled == 1
	if snooze != "" {
		p.SnoozeUntil, err = time.Parse(time.RFC3339Nano, snooze)
		if err != nil {
			return "invalid", p.Revision, nil
		}
	}
	reason, err := p.Suppression(at)
	if err != nil {
		return "invalid", p.Revision, nil
	}
	return string(reason), p.Revision, nil
}

// Native events have second precision. Require a known policy at the start of
// that second and deny if it, or any change within the second, suppresses. This
// preserves off->on and subsecond snooze suppression before history arrives.
func mentionEventPolicyReason(ctx context.Context, tx *sql.Tx, app, user string, second int64) (string, int64, error) {
	if second <= 0 || second > (math.MaxInt64-999)/1000 {
		return "event_time_unknown", 0, nil
	}
	start, end := second*1000, second*1000+999
	var baseline int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM notification_policy_history WHERE app_id=? AND open_id=? AND effective_at_ms<=? ORDER BY effective_at_ms DESC,revision DESC LIMIT 1`, app, user, start).Scan(&baseline)
	if errors.Is(err, sql.ErrNoRows) {
		return "policy_history_unknown", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT enabled,snooze_until,quiet_start,quiet_end,timezone,revision,effective_at_ms FROM notification_policy_history WHERE app_id=? AND open_id=? AND (revision=? OR effective_at_ms>=? AND effective_at_ms<=?) ORDER BY effective_at_ms,revision`, app, user, baseline, start, end)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	var latest int64
	for rows.Next() {
		var p rules.Policy
		var enabled int
		var snooze string
		var effective int64
		if err := rows.Scan(&enabled, &snooze, &p.QuietStart, &p.QuietEnd, &p.Timezone, &p.Revision, &effective); err != nil {
			return "", 0, err
		}
		latest = p.Revision
		if enabled != 0 && enabled != 1 {
			return "invalid", latest, nil
		}
		p.Enabled = enabled == 1
		if snooze != "" {
			p.SnoozeUntil, err = time.Parse(time.RFC3339Nano, snooze)
			if err != nil {
				return "invalid", latest, nil
			}
		}
		if effective < start {
			effective = start
		}
		reason, err := p.Suppression(time.UnixMilli(effective))
		if err != nil {
			return "invalid", latest, nil
		}
		if reason != rules.NotificationAllowed {
			return string(reason), latest, nil
		}
	}
	return "", latest, rows.Err()
}

func prepareMirrorMentions(ctx context.Context, tx *sql.Tx, m Mirror, a *MirrorAttempt, options MentionOptions) ([]string, error) {
	decisions, err := mentionDecisions(ctx, tx, m.HostID, m.ThreadID, m.Key)
	if err != nil {
		return nil, err
	}
	var app string
	if err := tx.QueryRowContext(ctx, `SELECT app_id FROM deployment WHERE host_id=?`, m.HostID).Scan(&app); err != nil {
		return nil, err
	}
	var selected []string
	for _, d := range decisions {
		if d.State != "eligible" {
			continue
		}
		reason := mentionEventReason(m, options)
		if reason == "" && (m.EventAt <= 0 || m.EventAt > a.CreatedAtMS/1000) {
			reason = "event_time_unknown"
		}
		if reason == "" && a.Operation == "update" {
			reason = "update"
		}
		if reason == "" && !containsMentionID(options.Targets, d.OpenID) {
			reason = "target_removed"
		}
		if reason == "" && (options.ChatID != a.ChatID || !containsMentionID(options.MemberIDs, d.OpenID)) {
			reason = "unreachable"
		}
		var revision int64
		if reason == "" {
			reason, revision, err = mentionPolicyWindowReason(ctx, tx, app, d.OpenID, m.EventAt*1000, a.CreatedAtMS)
			if err != nil {
				return nil, err
			}
		}
		if reason != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE mentions SET state='suppressed',reason=? WHERE host_id=? AND thread_id=? AND mirror_key=? AND open_id=? AND state='eligible'`, reason, m.HostID, m.ThreadID, m.Key, d.OpenID); err != nil {
				return nil, err
			}
		} else {
			selected = append(selected, d.OpenID)
			if _, err := tx.ExecContext(ctx, `UPDATE mentions SET policy_revision=? WHERE host_id=? AND thread_id=? AND mirror_key=? AND open_id=?`, revision, m.HostID, m.ThreadID, m.Key, d.OpenID); err != nil {
				return nil, err
			}
		}
	}
	if len(selected) != 0 {
		rendered, err := cards.WithMentions(a.Card, selected)
		if err != nil {
			for _, id := range selected {
				if _, err := tx.ExecContext(ctx, `UPDATE mentions SET state='suppressed',reason='render_failed' WHERE host_id=? AND thread_id=? AND mirror_key=? AND open_id=?`, m.HostID, m.ThreadID, m.Key, id); err != nil {
					return nil, err
				}
			}
			return nil, nil // Public body still dispatches.
		}
		a.Card = rendered
	}
	return selected, nil
}

func linkMirrorMentions(ctx context.Context, tx *sql.Tx, a MirrorAttempt, ids []string) error {
	for _, id := range ids {
		if err := mirrorWriteOne(ctx, tx, `UPDATE mentions SET state='dispatching',attempt_id=? WHERE host_id=? AND thread_id=? AND mirror_key=? AND open_id=? AND state='eligible'`, a.ID, a.HostID, a.ThreadID, a.Key, id); err != nil {
			return err
		}
	}
	return nil
}
