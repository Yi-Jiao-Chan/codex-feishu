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
const GroupNameRequestKind = "feishu.chat.name_sync.request"

type GroupNameScope struct {
	AppID   string  `json:"app_id"`
	OwnerID string  `json:"owner_id"`
	Binding Binding `json:"binding"`
}

// GroupNameRequest is private queue data, never diagnostic/log output.
type GroupNameRequest struct {
	Scope   GroupNameScope `json:"scope"`
	Desired string         `json:"desired"`
	encoded string
}

func validGroupNameRequest(r GroupNameRequest) bool {
	return validGroupNameScope(r.Scope) && (r.Scope.Binding.FollowMode == "" || r.Scope.Binding.FollowMode == "group") && strings.TrimSpace(r.Desired) != "" && utf8.ValidString(r.Desired)
}

// StageGroupNameRequest coalesces desired titles without dispatching a remote
// write. Restaging a completed title requests a fresh authoritative readback.
func (s *Store) StageGroupNameRequest(ctx context.Context, scope GroupNameScope, desired string, nowMS int64) (GroupNameRequest, error) {
	if scope.Binding.FollowMode == "group" {
		scope.Binding.FollowMode = ""
	}
	r := GroupNameRequest{Scope: scope, Desired: desired}
	if !validGroupNameRequest(r) || nowMS <= 0 {
		return GroupNameRequest{}, ErrConflict
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return GroupNameRequest{}, err
	}
	r.encoded = string(encoded)
	b := scope.Binding
	result, err := s.DB.ExecContext(ctx, `INSERT INTO m0_operations(key,kind,state,remote_id,created_at)
	SELECT ?,?,'ready',?,? FROM bindings b JOIN deployment d USING(host_id)
	WHERE b.host_id=? AND b.thread_id=? AND b.chat_id=? AND b.intent_id=? AND d.app_id=?
	AND b.follow_mode='group' AND b.root_message_id=? AND b.platform_thread_id=? AND b.repair_required=? AND b.repair_reason=?
	ON CONFLICT(key) DO UPDATE SET state='ready',remote_id=excluded.remote_id,created_at=excluded.created_at
	WHERE m0_operations.kind=excluded.kind`, groupNamePrefix(scope)+"request", GroupNameRequestKind, r.encoded, nowMS,
		b.HostID, b.ThreadID, b.ChatID, b.IntentID, scope.AppID, b.RootMessageID, b.PlatformThreadID, b.RepairRequired, b.RepairReason)
	if err != nil {
		return GroupNameRequest{}, err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		err = ErrConflict
	}
	return r, err
}

// PendingGroupNameRequests includes legacy unresolved/deferred writes so an
// upgrade can recover them without first reading native thread metadata.
func (s *Store) PendingGroupNameRequests(ctx context.Context, host, app, owner string) ([]GroupNameRequest, error) {
	if host == "" || app == "" || owner == "" {
		return nil, ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+bindingColumns+` FROM bindings WHERE host_id=? AND follow_mode='group'
	AND EXISTS(SELECT 1 FROM deployment d WHERE d.host_id=bindings.host_id AND d.app_id=?) ORDER BY thread_id`, host, app)
	if err != nil {
		return nil, err
	}
	var order []string
	scopes := map[string]GroupNameScope{}
	for rows.Next() {
		var b Binding
		if err := scanBinding(rows, &b); err != nil {
			rows.Close()
			return nil, err
		}
		scope := GroupNameScope{AppID: app, OwnerID: owner, Binding: b}
		prefix := groupNamePrefix(scope)
		scopes[prefix] = scope
		order = append(order, prefix)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(scopes) == 0 {
		return nil, nil
	}
	rows, err = tx.QueryContext(ctx, `SELECT key,kind,state,remote_id FROM m0_operations WHERE kind IN (?,?) ORDER BY rowid DESC`, GroupNameRequestKind, GroupNameOperationKind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ready := map[string]GroupNameRequest{}
	legacy := map[string]GroupNameRequest{}
	latest, unresolved := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var key, kind, state, encoded string
		if err := rows.Scan(&key, &kind, &state, &encoded); err != nil {
			return nil, err
		}
		end := strings.LastIndex(key, ":")
		if end < 0 {
			continue
		}
		prefix := key[:end+1]
		scope, relevant := scopes[prefix]
		if !relevant {
			continue
		}
		if !utf8.ValidString(encoded) {
			return nil, ErrConflict
		}
		var r GroupNameRequest
		if json.Unmarshal([]byte(encoded), &r) != nil || !validGroupNameScope(r.Scope) {
			return nil, ErrConflict
		}
		if r.Scope != scope {
			// Changed bindings and another owner's journal cannot authorize
			// this worker, even when they share the deterministic key prefix.
			continue
		}
		if !validGroupNameRequest(r) {
			return nil, ErrConflict
		}
		if kind == GroupNameRequestKind {
			if state == "ready" {
				if key != prefix+"request" {
					return nil, ErrConflict
				}
				r.encoded = encoded
				ready[prefix] = r
			}
			continue
		}
		var plan GroupNamePlan
		if json.Unmarshal([]byte(encoded), &plan) != nil || plan.RetryAtMS < 0 || plan.RejectionCount < 0 {
			return nil, ErrConflict
		}
		switch state {
		case "dispatching", "unknown":
			if !unresolved[prefix] {
				legacy[prefix], unresolved[prefix] = r, true
			}
		case "rejected":
			if !latest[prefix] && plan.RetryAtMS > 0 {
				legacy[prefix] = r
			}
		case "confirmed":
		default:
			return nil, ErrConflict
		}
		latest[prefix] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var requests []GroupNameRequest
	for _, prefix := range order {
		if r, ok := ready[prefix]; ok {
			requests = append(requests, r)
		} else if r, ok := legacy[prefix]; ok {
			requests = append(requests, r)
		}
	}
	return requests, nil
}

// CompleteGroupNameRequest cannot remove a newer desired title. A recovered
// legacy attempt has no queue row, so completing it is intentionally a no-op.
func (s *Store) CompleteGroupNameRequest(ctx context.Context, r GroupNameRequest) error {
	if !validGroupNameRequest(r) {
		return ErrConflict
	}
	if r.encoded == "" {
		return nil
	}
	var frozen GroupNameRequest
	if json.Unmarshal([]byte(r.encoded), &frozen) != nil || frozen.Scope != r.Scope || frozen.Desired != r.Desired {
		return ErrConflict
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM m0_operations WHERE key=? AND kind=? AND state='ready' AND remote_id=?`, groupNamePrefix(r.Scope)+"request", GroupNameRequestKind, r.encoded)
	return err
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
