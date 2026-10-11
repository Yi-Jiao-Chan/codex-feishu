package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"codex-feishu/internal/rules"
)

// Unsent notifications that crossed a suppression interval do not become a
// morning backlog. Policy audit covers off->on and snooze/quiet changes during
// a durable wait, even if the bridge did not run inside that interval.
func mentionPolicyWindowReason(ctx context.Context, tx *sql.Tx, app, user string, fromMS, toMS int64) (string, int64, error) {
	if fromMS <= 0 || toMS < fromMS {
		return "event_time_unknown", 0, nil
	}
	reason, current, err := mentionPolicyReason(ctx, tx, app, user, time.UnixMilli(toMS))
	if err != nil || reason != "" {
		return reason, current, err
	}
	var baseline int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM notification_policy_history WHERE app_id=? AND open_id=? AND effective_at_ms<=? ORDER BY effective_at_ms DESC,revision DESC LIMIT 1`, app, user, fromMS).Scan(&baseline)
	if errors.Is(err, sql.ErrNoRows) {
		return "policy_history_unknown", current, nil
	}
	if err != nil {
		return "", 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT enabled,snooze_until,quiet_start,quiet_end,timezone,revision,effective_at_ms FROM notification_policy_history WHERE app_id=? AND open_id=? AND (revision=? OR effective_at_ms>=? AND effective_at_ms<=?) ORDER BY effective_at_ms,revision`, app, user, baseline, fromMS, toMS)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	type segment struct {
		p  rules.Policy
		at int64
	}
	var all []segment
	for rows.Next() {
		var item segment
		var enabled int
		var snooze string
		if err := rows.Scan(&enabled, &snooze, &item.p.QuietStart, &item.p.QuietEnd, &item.p.Timezone, &item.p.Revision, &item.at); err != nil {
			return "", 0, err
		}
		if enabled != 0 && enabled != 1 {
			return "invalid", current, nil
		}
		item.p.Enabled = enabled == 1
		if snooze != "" {
			item.p.SnoozeUntil, err = time.Parse(time.RFC3339Nano, snooze)
			if err != nil {
				return "invalid", current, nil
			}
		}
		if err := item.p.Validate(); err != nil {
			return "invalid", current, nil
		}
		all = append(all, item)
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	if len(all) == 0 || all[len(all)-1].p.Revision != current {
		return "policy_history_unknown", current, nil
	}
	for n, item := range all {
		start := item.at
		if start < fromMS {
			start = fromMS
		}
		end := toMS
		if n+1 < len(all) && all[n+1].at <= end {
			end = all[n+1].at - 1
		}
		// Millisecond audit cannot disprove a brief off/quiet between revisions
		// recorded at the same instant. Retain that explicit suppression intent.
		if start > end {
			atReason, err := item.p.Suppression(time.UnixMilli(start))
			if err != nil {
				return "invalid", current, nil
			}
			if atReason != rules.NotificationAllowed {
				return string(atReason), item.p.Revision, nil
			}
			continue
		}
		windowReason, err := item.p.SuppressionBetween(time.UnixMilli(start), time.UnixMilli(end))
		if err != nil {
			return "invalid", current, nil
		}
		if windowReason != rules.NotificationAllowed {
			return string(windowReason), item.p.Revision, nil
		}
	}
	return "", current, nil
}
