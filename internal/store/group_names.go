package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"unicode/utf8"
)

const GroupNameOperationKind = "feishu.chat.name_sync"

type GroupNameScope struct {
	AppID   string  `json:"app_id"`
	OwnerID string  `json:"owner_id"`
	Binding Binding `json:"binding"`
}

// GroupNamePlan is private journal data, never diagnostic/log output.
type GroupNamePlan struct {
	Scope          GroupNameScope `json:"scope"`
	Desired        string         `json:"desired"`
	Previous       string         `json:"previous"`
	RetryAtMS      int64          `json:"retry_at_ms"`
	RejectionCount int64          `json:"rejection_count"`
}

type GroupNameAttempt struct {
	Key, State  string
	CreatedAtMS int64
	Plan        GroupNamePlan
	encoded     string
}

func groupNamePrefix(s GroupNameScope) string {
	b, _ := json.Marshal([]string{s.AppID, s.Binding.HostID, s.Binding.ThreadID, s.Binding.ChatID, s.Binding.IntentID})
	h := sha256.Sum256(b)
	return "group-name:" + hex.EncodeToString(h[:]) + ":"
}

func validGroupNameScope(s GroupNameScope) bool {
	return s.AppID != "" && s.OwnerID != "" && s.Binding.HostID != "" && s.Binding.ThreadID != "" && s.Binding.ChatID != "" && s.Binding.IntentID != ""
}

func scanGroupName(row *sql.Row, scope GroupNameScope) (GroupNameAttempt, error) {
	var a GroupNameAttempt
	if err := row.Scan(&a.Key, &a.State, &a.CreatedAtMS, &a.encoded); err != nil {
		return a, err
	}
	if json.Unmarshal([]byte(a.encoded), &a.Plan) != nil || a.Plan.Scope != scope || strings.TrimSpace(a.Plan.Desired) == "" || !utf8.ValidString(a.Plan.Desired) || a.Plan.RetryAtMS < 0 || a.Plan.RejectionCount < 0 {
		return GroupNameAttempt{}, ErrConflict
	}
	switch a.State {
	case "dispatching", "unknown", "confirmed", "rejected":
	default:
		return GroupNameAttempt{}, ErrConflict
	}
	return a, nil
}

// Pending attempts take precedence over newer rows: a changed title cannot
// bypass an ambiguous request, including an interrupted dispatch.
func (s *Store) GroupNameAttempt(ctx context.Context, scope GroupNameScope) (GroupNameAttempt, error) {
	if !validGroupNameScope(scope) {
		return GroupNameAttempt{}, ErrConflict
	}
	return scanGroupName(s.DB.QueryRowContext(ctx, `SELECT key,state,created_at,remote_id FROM m0_operations WHERE kind=? AND key LIKE ? ORDER BY (state IN ('dispatching','unknown')) DESC,rowid DESC LIMIT 1`, GroupNameOperationKind, groupNamePrefix(scope)+"%"), scope)
}

// ClaimGroupName atomically checks binding identity, deployment, the caller's
// last observed attempt and all unresolved writes before inserting dispatching.
// expectedKey is empty only when no previous attempt exists.
func (s *Store) ClaimGroupName(ctx context.Context, plan GroupNamePlan, expectedKey string, nowMS int64) (GroupNameAttempt, bool, error) {
	if !validGroupNameScope(plan.Scope) || strings.TrimSpace(plan.Desired) == "" || !utf8.ValidString(plan.Desired) || plan.Desired == plan.Previous || plan.RetryAtMS != 0 || plan.RejectionCount < 0 || nowMS <= 0 {
		return GroupNameAttempt{}, false, ErrConflict
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return GroupNameAttempt{}, false, err
	}
	a := GroupNameAttempt{Key: groupNamePrefix(plan.Scope) + ID(), State: "dispatching", CreatedAtMS: nowMS, Plan: plan, encoded: string(encoded)}
	b := plan.Scope.Binding
	prefix := groupNamePrefix(plan.Scope) + "%"
	result, err := s.DB.ExecContext(ctx, `INSERT INTO m0_operations(key,kind,state,remote_id,created_at)
 SELECT ?,?,'dispatching',?,? WHERE
 EXISTS(SELECT 1 FROM bindings b JOIN deployment d USING(host_id) WHERE b.host_id=? AND b.thread_id=? AND b.chat_id=? AND b.intent_id=? AND d.app_id=?)
 AND NOT EXISTS(SELECT 1 FROM m0_operations WHERE kind=? AND key LIKE ? AND state IN ('dispatching','unknown'))
 AND COALESCE((SELECT key FROM m0_operations WHERE kind=? AND key LIKE ? ORDER BY rowid DESC LIMIT 1),'')=?
 AND NOT EXISTS(SELECT 1 FROM m0_operations WHERE key=? AND kind=? AND
 (json_extract(remote_id,'$.retry_at_ms')>? OR (state='rejected' AND json_extract(remote_id,'$.retry_at_ms')=0 AND json_extract(remote_id,'$.desired')=?)))`,
		a.Key, GroupNameOperationKind, a.encoded, nowMS, b.HostID, b.ThreadID, b.ChatID, b.IntentID, plan.Scope.AppID,
		GroupNameOperationKind, prefix, GroupNameOperationKind, prefix, expectedKey, expectedKey, GroupNameOperationKind, nowMS, plan.Desired)
	if err != nil {
		return GroupNameAttempt{}, false, err
	}
	n, err := result.RowsAffected()
	return a, n == 1, err
}

// FinishGroupName CAS preserves the complete frozen plan. Unknown can only
// become confirmed through authoritative readback, never rejected/retryable.
func (s *Store) FinishGroupName(ctx context.Context, a GroupNameAttempt, state string) error {
	if state != "unknown" && state != "rejected" && state != "confirmed" || a.State != "dispatching" && !(a.State == "unknown" && state == "confirmed") || a.Key == "" || a.encoded == "" {
		return ErrConflict
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE m0_operations SET state=? WHERE key=? AND kind=? AND state=? AND remote_id=?`, state, a.Key, GroupNameOperationKind, a.State, a.encoded)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) DeferGroupName(ctx context.Context, a GroupNameAttempt, nowMS, minimumDelayMS int64) (int64, error) {
	if a.State != "dispatching" || a.encoded == "" || nowMS < a.CreatedAtMS || minimumDelayMS < 0 || a.Plan.RejectionCount == math.MaxInt64 {
		return 0, ErrConflict
	}
	var p GroupNamePlan
	if json.Unmarshal([]byte(a.encoded), &p) != nil || p != a.Plan {
		return 0, ErrConflict
	}
	p.RejectionCount++
	delay := int64(1000)
	for n := int64(1); n < p.RejectionCount && delay < 60000; n++ {
		delay *= 2
	}
	if delay > 60000 {
		delay = 60000
	}
	if minimumDelayMS > delay {
		delay = minimumDelayMS
	}
	if delay > math.MaxInt64-nowMS {
		return 0, ErrConflict
	}
	p.RetryAtMS = nowMS + delay
	encoded, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE m0_operations SET state='rejected',remote_id=? WHERE key=? AND kind=? AND state='dispatching' AND remote_id=?`, string(encoded), a.Key, GroupNameOperationKind, a.encoded)
	if err != nil {
		return 0, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrConflict
	}
	return p.RetryAtMS, nil
}

// GroupNameHealth exposes counts only, never frozen names or group identity.
func (s *Store) GroupNameHealth(ctx context.Context, host, app string) (map[string]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT state,COUNT(*) FROM m0_operations WHERE kind=? AND json_valid(remote_id) AND json_extract(remote_id,'$.scope.app_id')=? AND json_extract(remote_id,'$.scope.binding.HostID')=? GROUP BY state`, GroupNameOperationKind, app, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		counts[state] = n
	}
	return counts, rows.Err()
}
