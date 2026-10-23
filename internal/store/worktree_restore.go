package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// WorktreeRestoreSchema belongs in the next application migration. Restore
// metadata contains filesystem/routing identity, never a copy of input bodies.
const WorktreeRestoreSchema = `
CREATE TABLE IF NOT EXISTS thread_worktree_identities (
 host_id TEXT NOT NULL,app_id TEXT NOT NULL,thread_id TEXT NOT NULL,
 raw_cwd TEXT NOT NULL,root TEXT NOT NULL,source_root TEXT NOT NULL,common_dir TEXT NOT NULL,relative_cwd TEXT NOT NULL,
 binding_json TEXT NOT NULL,PRIMARY KEY(host_id,app_id,thread_id),
 FOREIGN KEY(host_id,thread_id) REFERENCES bindings(host_id,thread_id)
);
CREATE TABLE IF NOT EXISTS worktree_restore_grants (
 receipt_id TEXT PRIMARY KEY REFERENCES m0_receipts(id),host_id TEXT NOT NULL,app_id TEXT NOT NULL,thread_id TEXT NOT NULL,
 binding_json TEXT NOT NULL,origin TEXT NOT NULL CHECK(origin IN ('ordinary','legacy-pending')),created_at_ms INTEGER NOT NULL,
 FOREIGN KEY(host_id,thread_id) REFERENCES bindings(host_id,thread_id)
);
CREATE INDEX IF NOT EXISTS worktree_restore_grant_thread ON worktree_restore_grants(host_id,app_id,thread_id);
CREATE TABLE IF NOT EXISTS worktree_restore_attempts (
 attempt_id TEXT PRIMARY KEY,host_id TEXT NOT NULL,app_id TEXT NOT NULL,thread_id TEXT NOT NULL,
 receipt_id TEXT NOT NULL UNIQUE REFERENCES worktree_restore_grants(receipt_id),params_json TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('prepared','dispatching','unknown','confirmed','blocked')),
 phase TEXT NOT NULL DEFAULT 'planned',result_json TEXT NOT NULL DEFAULT '{}',error_code TEXT NOT NULL DEFAULT '',
 created_at_ms INTEGER NOT NULL,updated_at_ms INTEGER NOT NULL,
 FOREIGN KEY(host_id,thread_id) REFERENCES bindings(host_id,thread_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS worktree_restore_one_pending ON worktree_restore_attempts(host_id,app_id,thread_id)
 WHERE state IN ('prepared','dispatching','unknown');
`

var ErrWorktreeRestoreBusy = errors.New("thread has a worktree restore awaiting verification")

type ThreadWorktreeIdentity struct {
	HostID, AppID, ThreadID                          string
	RawCWD, Root, SourceRoot, CommonDir, RelativeCWD string
}

type WorktreeRestoreGrant struct {
	Scope       SubmissionScope
	ReceiptID   string
	Origin      string
	CreatedAtMS int64
}

type WorktreeRestoreAttempt struct {
	ID, ReceiptID, ParamsJSON, State, Phase, ResultJSON, ErrorCode string
	Scope                                                          SubmissionScope
	CreatedAtMS, UpdatedAtMS                                       int64
}

type WorktreeRestorePublicationCandidate struct {
	ReceiptID, ClientID string
}

// Freeze the route independently of the mutable availability/repair flag. A
// platform thread ID may legitimately become known after admission; the root
// and normalized follow mode already identify that same target.
func worktreeRestoreBinding(ctx context.Context, q submissionQuerier, scope SubmissionScope) (string, error) {
	if _, err := submissionBinding(ctx, q, scope); err != nil {
		return "", err
	}
	var b Binding
	if err := scanBinding(q.QueryRowContext(ctx, `SELECT `+bindingColumns+` FROM bindings WHERE host_id=? AND thread_id=?`, scope.HostID, scope.ThreadID), &b); err != nil {
		return "", err
	}
	if b.RepairRequired || b.FollowMode != "" && b.FollowMode != "group" && b.FollowMode != "topic" || b.FollowMode == "topic" && b.RootMessageID == "" {
		return "", ErrTargetRepair
	}
	mode := "group"
	if b.FollowMode == "topic" {
		mode = "topic"
	}
	raw, err := json.Marshal(struct{ IntentID, ChatID, RootID, FollowMode string }{b.IntentID, b.ChatID, b.RootMessageID, mode})
	return string(raw), err
}

func validWorktreeRestoreIdentity(v ThreadWorktreeIdentity) bool {
	cleanAbsolute := func(path string) bool {
		return filepath.IsAbs(path) && filepath.Clean(path) == path && strings.IndexFunc(path, unicode.IsControl) < 0
	}
	return v.HostID != "" && v.AppID != "" && v.ThreadID != "" && cleanAbsolute(v.RawCWD) && cleanAbsolute(v.Root) &&
		cleanAbsolute(v.SourceRoot) && cleanAbsolute(v.CommonDir) && v.Root != v.SourceRoot && v.RelativeCWD != "" &&
		!filepath.IsAbs(v.RelativeCWD) && filepath.Clean(v.RelativeCWD) == v.RelativeCWD && v.RelativeCWD != ".." &&
		!strings.HasPrefix(v.RelativeCWD, ".."+string(filepath.Separator)) && strings.IndexFunc(v.RelativeCWD, unicode.IsControl) < 0 && filepath.Join(v.Root, v.RelativeCWD) == v.RawCWD
}

// RegisterThreadWorktreeIdentity accepts identity already physically verified by
// its caller. Registration is immutable: metadata drift requires explicit review.
func (s *Store) RegisterThreadWorktreeIdentity(ctx context.Context, v ThreadWorktreeIdentity) error {
	if !validWorktreeRestoreIdentity(v) {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	scope := SubmissionScope{v.HostID, v.AppID, v.ThreadID}
	binding, err := worktreeRestoreBinding(ctx, tx, scope)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO thread_worktree_identities(host_id,app_id,thread_id,raw_cwd,root,source_root,common_dir,relative_cwd,binding_json) VALUES(?,?,?,?,?,?,?,?,?)`, v.HostID, v.AppID, v.ThreadID, v.RawCWD, v.Root, v.SourceRoot, v.CommonDir, v.RelativeCWD, binding)
	if err != nil {
		return err
	}
	got, err := readThreadWorktreeIdentity(ctx, tx, scope, binding)
	if err != nil || got != v {
		return ErrConflict
	}
	return tx.Commit()
}

func readThreadWorktreeIdentity(ctx context.Context, q submissionQuerier, scope SubmissionScope, binding string) (v ThreadWorktreeIdentity, err error) {
	v.HostID, v.AppID, v.ThreadID = scope.HostID, scope.AppID, scope.ThreadID
	err = q.QueryRowContext(ctx, `SELECT raw_cwd,root,source_root,common_dir,relative_cwd FROM thread_worktree_identities WHERE host_id=? AND app_id=? AND thread_id=? AND binding_json=?`, scope.HostID, scope.AppID, scope.ThreadID, binding).Scan(&v.RawCWD, &v.Root, &v.SourceRoot, &v.CommonDir, &v.RelativeCWD)
	if err == nil && !validWorktreeRestoreIdentity(v) {
		err = ErrConflict
	}
	return
}

func (s *Store) ThreadWorktreeIdentity(ctx context.Context, scope SubmissionScope) (ThreadWorktreeIdentity, error) {
	binding, err := worktreeRestoreBinding(ctx, s.DB, scope)
	if err != nil {
		return ThreadWorktreeIdentity{}, err
	}
	return readThreadWorktreeIdentity(ctx, s.DB, scope, binding)
}

func stageWorktreeRestoreGrantTx(ctx context.Context, tx *sql.Tx, scope SubmissionScope, receipt string) error {
	binding, err := worktreeRestoreBinding(ctx, tx, scope)
	if err != nil {
		return err
	}
	// This helper is called only by fresh ordinary-message admission, never by
	// generic receipt adoption, creation, async answers or download completion.
	r, err := tx.ExecContext(ctx, `INSERT INTO worktree_restore_grants(receipt_id,host_id,app_id,thread_id,binding_json,origin,created_at_ms)
 SELECT id,?,?,?,?,'ordinary',? FROM m0_receipts WHERE id=? AND app_id=? AND thread_id=? AND state IN ('received','collecting','draft')`, scope.HostID, scope.AppID, scope.ThreadID, binding, time.Now().UnixMilli(), receipt, scope.AppID, scope.ThreadID)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return ErrConflict
	}
	return err
}

// This predicate excludes every alternate receipt writer in the current code:
// creation, async answers, old M0 writes/actions and native queue/history evidence.
// The ID relation is an additional positive check of the ordinary receipt writer.
const worktreeRestoreUnsent = `r.state='received' AND r.queue_id='' AND r.reply_message_id='' AND r.client_id='cfb-'||r.id
 AND (n.receipt_id IS NULL OR (n.host_id=? AND n.app_id=? AND n.thread_id=? AND n.state='received' AND n.queue_id='' AND n.queue_ordinal=0 AND n.turn_id='' AND n.item_id=''))
 AND NOT EXISTS(SELECT 1 FROM submission_attempts a WHERE a.receipt_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM input_history_links h WHERE h.receipt_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM creation_workflows c WHERE c.receipt_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM interaction_requests i WHERE i.submission_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM submission_actions a WHERE a.receipt_id=r.id AND a.kind<>'continue')
 AND NOT EXISTS(SELECT 1 FROM m0_actions a WHERE a.receipt_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM m0_operations o WHERE o.key='m0.add.'||r.id)
 AND (r.input_json='' OR EXISTS(SELECT 1 FROM attachment_batches b JOIN attachment_events e ON e.batch_id=b.batch_id WHERE b.receipt_id=r.id AND e.receipt_id=r.id AND b.state='sealed' AND b.host_id=? AND b.app_id=? AND b.thread_id=?))`

// BackfillWorktreeRestoreGrants adopts only definitely unsent pending receipts
// whose current writer provenance excludes creation and async answers. It never
// authorizes an unknown/native-attempted input or an unsent attachment draft.
func (s *Store) BackfillWorktreeRestoreGrants(ctx context.Context, scope SubmissionScope, nowMS int64) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	binding, err := worktreeRestoreBinding(ctx, tx, scope)
	if err != nil {
		return 0, err
	}
	r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO worktree_restore_grants(receipt_id,host_id,app_id,thread_id,binding_json,origin,created_at_ms)
 SELECT r.id,?,?,?,?,'legacy-pending',? FROM m0_receipts r LEFT JOIN submission_inputs n ON n.receipt_id=r.id
 WHERE r.app_id=? AND r.thread_id=? AND r.chat_id=(SELECT chat_id FROM bindings WHERE host_id=? AND thread_id=?) AND `+worktreeRestoreUnsent+`
 AND NOT EXISTS(SELECT 1 FROM worktree_restore_grants g WHERE g.receipt_id=r.id)
 ORDER BY r.rowid LIMIT 256`, scope.HostID, scope.AppID, scope.ThreadID, binding, nowMS, scope.AppID, scope.ThreadID, scope.HostID, scope.ThreadID, scope.HostID, scope.AppID, scope.ThreadID, scope.HostID, scope.AppID, scope.ThreadID)
	if err != nil {
		return 0, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}

func readWorktreeRestoreGrant(ctx context.Context, q submissionQuerier, scope SubmissionScope, receipt, binding string) (g WorktreeRestoreGrant, err error) {
	g.Scope = scope
	err = q.QueryRowContext(ctx, `SELECT receipt_id,origin,created_at_ms FROM worktree_restore_grants WHERE host_id=? AND app_id=? AND thread_id=? AND receipt_id=? AND binding_json=?`, scope.HostID, scope.AppID, scope.ThreadID, receipt, binding).Scan(&g.ReceiptID, &g.Origin, &g.CreatedAtMS)
	return
}

func (s *Store) WorktreeRestoreGrant(ctx context.Context, scope SubmissionScope, receipt string) (WorktreeRestoreGrant, error) {
	binding, err := worktreeRestoreBinding(ctx, s.DB, scope)
	if err != nil {
		return WorktreeRestoreGrant{}, err
	}
	return readWorktreeRestoreGrant(ctx, s.DB, scope, receipt, binding)
}

func worktreeRestoreReceiptReady(ctx context.Context, q submissionQuerier, scope SubmissionScope, receipt string) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM m0_receipts r LEFT JOIN submission_inputs n ON n.receipt_id=r.id WHERE r.id=? AND r.app_id=? AND r.thread_id=? AND r.chat_id=(SELECT chat_id FROM bindings WHERE host_id=? AND thread_id=?) AND `+worktreeRestoreUnsent, receipt, scope.AppID, scope.ThreadID, scope.HostID, scope.ThreadID, scope.HostID, scope.AppID, scope.ThreadID, scope.HostID, scope.AppID, scope.ThreadID).Scan(&count)
	return count == 1, err
}

// NextWorktreeRestoreGrant also backfills safe pre-upgrade pending input. A
// collecting receipt preserves FIFO and blocks later received receipts.
func (s *Store) NextWorktreeRestoreGrant(ctx context.Context, scope SubmissionScope) (WorktreeRestoreGrant, error) {
	if _, err := s.BackfillWorktreeRestoreGrants(ctx, scope, time.Now().UnixMilli()); err != nil {
		return WorktreeRestoreGrant{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return WorktreeRestoreGrant{}, err
	}
	defer tx.Rollback()
	binding, err := worktreeRestoreBinding(ctx, tx, scope)
	if err != nil {
		return WorktreeRestoreGrant{}, err
	}
	if blocked, err := submissionBlocked(ctx, tx, scope); err != nil {
		return WorktreeRestoreGrant{}, err
	} else if blocked {
		return WorktreeRestoreGrant{}, ErrSubmissionBusy
	}
	pending, err := scanWorktreeRestoreAttempt(tx.QueryRowContext(ctx, `SELECT `+worktreeRestoreAttemptColumns+` FROM worktree_restore_attempts WHERE host_id=? AND app_id=? AND thread_id=? AND state IN ('prepared','dispatching','unknown')`, scope.HostID, scope.AppID, scope.ThreadID))
	if err == nil {
		g, err := readWorktreeRestoreGrant(ctx, tx, scope, pending.ReceiptID, binding)
		if err != nil {
			return g, err
		}
		return g, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorktreeRestoreGrant{}, err
	}
	var receipt, state string
	err = tx.QueryRowContext(ctx, `SELECT r.id,COALESCE(n.state,r.state) FROM m0_receipts r LEFT JOIN submission_inputs n ON n.receipt_id=r.id LEFT JOIN submission_order o ON o.receipt_id=r.id
 WHERE r.app_id=? AND r.thread_id=? AND r.chat_id=(SELECT chat_id FROM bindings WHERE host_id=? AND thread_id=?) AND (COALESCE(n.state,r.state)='collecting' OR (COALESCE(n.state,r.state)='received' AND EXISTS(SELECT 1 FROM worktree_restore_grants g WHERE g.receipt_id=r.id AND g.host_id=? AND g.app_id=? AND g.thread_id=?) AND NOT EXISTS(SELECT 1 FROM worktree_restore_attempts a WHERE a.receipt_id=r.id)))
 ORDER BY COALESCE(o.sequence,0),r.rowid LIMIT 1`, scope.AppID, scope.ThreadID, scope.HostID, scope.ThreadID, scope.HostID, scope.AppID, scope.ThreadID).Scan(&receipt, &state)
	if err != nil || state != "received" {
		if err == nil {
			err = sql.ErrNoRows
		}
		return WorktreeRestoreGrant{}, err
	}
	ready, err := worktreeRestoreReceiptReady(ctx, tx, scope, receipt)
	if err != nil || !ready {
		if err == nil {
			err = sql.ErrNoRows
		}
		return WorktreeRestoreGrant{}, err
	}
	g, err := readWorktreeRestoreGrant(ctx, tx, scope, receipt, binding)
	if err != nil {
		return g, err
	}
	return g, tx.Commit()
}

const worktreeRestoreAttemptColumns = `attempt_id,host_id,app_id,thread_id,receipt_id,params_json,state,phase,result_json,error_code,created_at_ms,updated_at_ms`

func scanWorktreeRestoreAttempt(row rowScanner) (a WorktreeRestoreAttempt, err error) {
	err = row.Scan(&a.ID, &a.Scope.HostID, &a.Scope.AppID, &a.Scope.ThreadID, &a.ReceiptID, &a.ParamsJSON, &a.State, &a.Phase, &a.ResultJSON, &a.ErrorCode, &a.CreatedAtMS, &a.UpdatedAtMS)
	return
}

func readWorktreeRestoreAttempt(ctx context.Context, q submissionQuerier, scope SubmissionScope, receipt string) (WorktreeRestoreAttempt, error) {
	return scanWorktreeRestoreAttempt(q.QueryRowContext(ctx, `SELECT `+worktreeRestoreAttemptColumns+` FROM worktree_restore_attempts WHERE host_id=? AND app_id=? AND thread_id=? AND receipt_id=?`, scope.HostID, scope.AppID, scope.ThreadID, receipt))
}

func (s *Store) WorktreeRestoreAttempt(ctx context.Context, scope SubmissionScope, receipt string) (WorktreeRestoreAttempt, error) {
	binding, err := worktreeRestoreBinding(ctx, s.DB, scope)
	if err != nil {
		return WorktreeRestoreAttempt{}, err
	}
	if _, err := readWorktreeRestoreGrant(ctx, s.DB, scope, receipt, binding); err != nil {
		return WorktreeRestoreAttempt{}, err
	}
	return readWorktreeRestoreAttempt(ctx, s.DB, scope, receipt)
}

func validRestoreJSON(raw string) bool {
	return len(raw) > 0 && len(raw) <= 64<<10 && json.Valid([]byte(raw))
}

func (s *Store) PrepareWorktreeRestoreAttempt(ctx context.Context, scope SubmissionScope, receipt, params string, nowMS int64) (WorktreeRestoreAttempt, error) {
	if !validRestoreJSON(params) {
		return WorktreeRestoreAttempt{}, ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return WorktreeRestoreAttempt{}, err
	}
	defer tx.Rollback()
	binding, err := worktreeRestoreBinding(ctx, tx, scope)
	if err != nil {
		return WorktreeRestoreAttempt{}, err
	}
	if _, err := readWorktreeRestoreGrant(ctx, tx, scope, receipt, binding); err != nil {
		return WorktreeRestoreAttempt{}, err
	}
	prior, err := readWorktreeRestoreAttempt(ctx, tx, scope, receipt)
	if err == nil {
		if prior.ParamsJSON != params {
			return prior, ErrConflict
		}
		return prior, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return prior, err
	}
	if blocked, err := submissionBlocked(ctx, tx, scope); err != nil {
		return WorktreeRestoreAttempt{}, err
	} else if blocked {
		return WorktreeRestoreAttempt{}, ErrSubmissionBusy
	}
	ready, err := worktreeRestoreReceiptReady(ctx, tx, scope, receipt)
	if err != nil || !ready {
		if err == nil {
			err = ErrSubmissionStale
		}
		return WorktreeRestoreAttempt{}, err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worktree_restore_attempts WHERE host_id=? AND app_id=? AND thread_id=? AND state IN ('prepared','dispatching','unknown')`, scope.HostID, scope.AppID, scope.ThreadID).Scan(&pending); err != nil {
		return WorktreeRestoreAttempt{}, err
	}
	if pending != 0 {
		return WorktreeRestoreAttempt{}, ErrWorktreeRestoreBusy
	}
	a := WorktreeRestoreAttempt{ID: ID(), Scope: scope, ReceiptID: receipt, ParamsJSON: params, State: "prepared", Phase: "planned", ResultJSON: `{}`, CreatedAtMS: nowMS, UpdatedAtMS: nowMS}
	_, err = tx.ExecContext(ctx, `INSERT INTO worktree_restore_attempts(`+worktreeRestoreAttemptColumns+`) VALUES(?,?,?,?,?,?,'prepared','planned','{}','',?,?)`, a.ID, scope.HostID, scope.AppID, scope.ThreadID, receipt, params, nowMS, nowMS)
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func (s *Store) ClaimWorktreeRestoreAttempt(ctx context.Context, a WorktreeRestoreAttempt, nowMS int64) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	binding, err := worktreeRestoreBinding(ctx, tx, a.Scope)
	if err != nil {
		return false, err
	}
	if _, err := readWorktreeRestoreGrant(ctx, tx, a.Scope, a.ReceiptID, binding); err != nil {
		return false, err
	}
	if blocked, err := submissionBlocked(ctx, tx, a.Scope); err != nil {
		return false, err
	} else if blocked {
		return false, ErrSubmissionBusy
	}
	ready, err := worktreeRestoreReceiptReady(ctx, tx, a.Scope, a.ReceiptID)
	if err != nil || !ready {
		if err == nil {
			err = ErrSubmissionStale
		}
		return false, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE worktree_restore_attempts SET state='dispatching',updated_at_ms=? WHERE attempt_id=? AND host_id=? AND app_id=? AND thread_id=? AND receipt_id=? AND params_json=? AND state='prepared'`, nowMS, a.ID, a.Scope.HostID, a.Scope.AppID, a.Scope.ThreadID, a.ReceiptID, a.ParamsJSON)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, tx.Commit()
}

func restorePhase(phase string) bool {
	switch phase {
	case "planned", "stage-dispatching", "stage-ready", "owner-dispatching", "owner-ready", "move-dispatching", "moved", "repair-dispatching", "restored":
		return true
	}
	return false
}

func restoreCheckpoint(phase, result string, confirmed bool) bool {
	if !restorePhase(phase) || !validRestoreJSON(result) {
		return false
	}
	var proof struct {
		Version    int    `json:"version"`
		Phase      string `json:"phase"`
		Generation string `json:"generation"`
	}
	if json.Unmarshal([]byte(result), &proof) != nil || proof.Version != 1 || proof.Phase != phase {
		return false
	}
	return !confirmed || phase == "restored" && len(proof.Generation) == 67 && strings.HasPrefix(proof.Generation, "v1:") && strings.Trim(proof.Generation[3:], "0123456789abcdef") == ""
}

func canceledWorktreeRestoreInput(ctx context.Context, q submissionQuerier, scope SubmissionScope, receipt, binding string) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM m0_receipts r
 JOIN submission_inputs n ON n.receipt_id=r.id AND n.app_id=r.app_id AND n.thread_id=r.thread_id
 JOIN worktree_restore_grants g ON g.receipt_id=r.id AND g.host_id=n.host_id AND g.app_id=n.app_id AND g.thread_id=n.thread_id
 WHERE r.id=? AND n.host_id=? AND n.app_id=? AND n.thread_id=? AND g.binding_json=?
 AND r.state='removed' AND n.state='removed' AND r.queue_id='' AND n.queue_id='' AND n.queue_ordinal=0 AND n.turn_id='' AND n.item_id=''
 AND r.client_id='cfb-'||r.id AND r.chat_id=(SELECT chat_id FROM bindings WHERE host_id=? AND thread_id=?)
 AND EXISTS(SELECT 1 FROM submission_actions a WHERE a.host_id=n.host_id AND a.app_id=n.app_id AND a.thread_id=n.thread_id AND a.receipt_id=r.id
  AND a.kind='remove' AND a.stage='local' AND a.state='confirmed' AND a.result='removed' AND a.token=r.action_token AND a.client_id=r.client_id AND a.revision+1=n.revision)
 AND NOT EXISTS(SELECT 1 FROM submission_attempts a WHERE a.receipt_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM input_history_links h WHERE h.receipt_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM m0_operations o WHERE o.key='m0.add.'||r.id)
 AND NOT EXISTS(SELECT 1 FROM m0_actions a WHERE a.receipt_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM creation_workflows c WHERE c.receipt_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM interaction_requests i WHERE i.submission_id=r.id)`, receipt, scope.HostID, scope.AppID, scope.ThreadID, binding, scope.HostID, scope.ThreadID).Scan(&count)
	return count == 1, err
}

func abandonRestorePhase(phase string) bool {
	return phase == "planned" || phase == "stage-ready" || phase == "owner-ready"
}

func preMoveRestorePhase(phase string) bool {
	return abandonRestorePhase(phase) || phase == "stage-dispatching" || phase == "owner-dispatching"
}

// AbandonCanceledWorktreeRestore records only a caller's complete physical
// Reconcile proof. It never removes a partial stage or releases a moved root.
func (s *Store) AbandonCanceledWorktreeRestore(ctx context.Context, a WorktreeRestoreAttempt, phase, result string, nowMS int64) error {
	if a.State != "unknown" || !preMoveRestorePhase(a.Phase) || !abandonRestorePhase(phase) || !restoreCheckpoint(phase, result, false) {
		return ErrConflict
	}
	return s.updateWorktreeRestoreAttempt(ctx, a, "unknown", "blocked", phase, result, "input-canceled", nowMS)
}

// Publication candidates are later ordinary receipts, not a replacement for
// the canceled input. The caller must match their exact client ID to current
// native history and independently verify owner activity or a terminal turn.
func (s *Store) WorktreeRestorePublicationCandidates(ctx context.Context, scope SubmissionScope, originalReceipt string) ([]WorktreeRestorePublicationCandidate, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	binding, err := worktreeRestoreBinding(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	canceled, err := canceledWorktreeRestoreInput(ctx, tx, scope, originalReceipt, binding)
	if err != nil || !canceled {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.id,r.client_id FROM worktree_restore_grants g
 JOIN m0_receipts r ON r.id=g.receipt_id AND r.app_id=g.app_id AND r.thread_id=g.thread_id
 JOIN submission_inputs n ON n.receipt_id=r.id AND n.host_id=g.host_id AND n.app_id=g.app_id AND n.thread_id=g.thread_id
 JOIN submission_order candidate_order ON candidate_order.receipt_id=r.id
 JOIN submission_order original_order ON original_order.receipt_id=?
 WHERE g.host_id=? AND g.app_id=? AND g.thread_id=? AND g.binding_json=? AND candidate_order.sequence>original_order.sequence
 AND r.client_id='cfb-'||r.id AND r.chat_id=(SELECT chat_id FROM bindings WHERE host_id=? AND thread_id=?)
 AND n.state NOT IN ('draft','collecting','removed','failed') AND r.state NOT IN ('draft','collecting','removed','failed')
 ORDER BY candidate_order.sequence LIMIT 256`, originalReceipt, scope.HostID, scope.AppID, scope.ThreadID, binding, scope.HostID, scope.ThreadID)
	if err != nil {
		return nil, err
	}
	var out []WorktreeRestorePublicationCandidate
	for rows.Next() {
		var candidate WorktreeRestorePublicationCandidate
		if err := rows.Scan(&candidate.ReceiptID, &candidate.ClientID); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, candidate)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return out, tx.Commit()
}

func (s *Store) updateWorktreeRestoreAttempt(ctx context.Context, a WorktreeRestoreAttempt, from, state, phase, result, code string, nowMS int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	binding, err := worktreeRestoreBinding(ctx, tx, a.Scope)
	if err != nil {
		return err
	}
	if _, err := readWorktreeRestoreGrant(ctx, tx, a.Scope, a.ReceiptID, binding); err != nil {
		return err
	}
	if code == "input-canceled" {
		if from != "unknown" || state != "blocked" || !preMoveRestorePhase(a.Phase) || !abandonRestorePhase(phase) {
			return ErrConflict
		}
		canceled, err := canceledWorktreeRestoreInput(ctx, tx, a.Scope, a.ReceiptID, binding)
		if err != nil {
			return err
		}
		if !canceled {
			return ErrSubmissionStale
		}
	}
	if state == "dispatching" && from == "unknown" {
		ready, err := worktreeRestoreReceiptReady(ctx, tx, a.Scope, a.ReceiptID)
		if err != nil {
			return err
		}
		if !ready {
			return ErrSubmissionStale
		}
		if blocked, err := submissionBlocked(ctx, tx, a.Scope); err != nil {
			return err
		} else if blocked {
			return ErrSubmissionBusy
		}
	}
	r, err := tx.ExecContext(ctx, `UPDATE worktree_restore_attempts SET state=?,phase=?,result_json=?,error_code=?,updated_at_ms=? WHERE attempt_id=? AND host_id=? AND app_id=? AND thread_id=? AND receipt_id=? AND params_json=? AND state=? AND phase=? AND result_json=?`, state, phase, result, code, nowMS, a.ID, a.Scope.HostID, a.Scope.AppID, a.Scope.ThreadID, a.ReceiptID, a.ParamsJSON, from, a.Phase, a.ResultJSON)
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
	return tx.Commit()
}

// CheckpointWorktreeRestoreAttempt commits the next physical phase using the
// caller's exact previous checkpoint. The caller must refresh a after success.
func (s *Store) CheckpointWorktreeRestoreAttempt(ctx context.Context, a WorktreeRestoreAttempt, phase, result string, nowMS int64) error {
	if !restoreCheckpoint(phase, result, false) {
		return ErrConflict
	}
	return s.updateWorktreeRestoreAttempt(ctx, a, "dispatching", "dispatching", phase, result, "", nowMS)
}

// ResumeWorktreeRestoreAttempt is available only after the restorer physically
// reconciles its frozen plan/checkpoint. It never authorizes a fresh Git add.
func (s *Store) ResumeWorktreeRestoreAttempt(ctx context.Context, a WorktreeRestoreAttempt, phase, result string, nowMS int64) error {
	if !restoreCheckpoint(phase, result, false) {
		return ErrConflict
	}
	return s.updateWorktreeRestoreAttempt(ctx, a, "unknown", "dispatching", phase, result, "", nowMS)
}

// FinishWorktreeRestoreAttempt accepts confirmed only with a restored physical
// generation supplied by the restorer. Queue/history inference is insufficient.
func (s *Store) FinishWorktreeRestoreAttempt(ctx context.Context, a WorktreeRestoreAttempt, state, result, code string, nowMS int64) error {
	if state != "unknown" && state != "confirmed" && state != "blocked" || !validRestoreJSON(result) || len(code) > 96 || strings.IndexFunc(code, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') }) >= 0 {
		return ErrConflict
	}
	phase := a.Phase
	if state != "confirmed" && result == `{}` {
		result = a.ResultJSON // An error must not erase the last durable checkpoint.
	}
	if state == "confirmed" {
		phase = "restored"
		if !restoreCheckpoint(phase, result, true) {
			return ErrConflict
		}
	} else if result != `{}` && !restoreCheckpoint(phase, result, false) {
		return ErrConflict
	}
	prior, err := s.WorktreeRestoreAttempt(ctx, a.Scope, a.ReceiptID)
	if err != nil {
		return err
	}
	if prior.State != "dispatching" && prior.State != "unknown" && !(state == "blocked" && prior.State == "prepared") {
		return ErrConflict
	}
	return s.updateWorktreeRestoreAttempt(ctx, a, prior.State, state, phase, result, code, nowMS)
}

func (s *Store) RecoverWorktreeRestoreDispatches(ctx context.Context, scope SubmissionScope, nowMS int64) error {
	binding, err := worktreeRestoreBinding(ctx, s.DB, scope)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE worktree_restore_attempts SET state='unknown',error_code='recovery-unknown',updated_at_ms=? WHERE host_id=? AND app_id=? AND thread_id=? AND state='dispatching' AND receipt_id IN (SELECT receipt_id FROM worktree_restore_grants WHERE host_id=? AND app_id=? AND thread_id=? AND binding_json=?)`, nowMS, scope.HostID, scope.AppID, scope.ThreadID, scope.HostID, scope.AppID, scope.ThreadID, binding)
	return err
}

// WorktreeRestoreAttempts returns immutable journals in creation order, including
// blocked outcomes so feedback and physical-generation validation can reuse them.
func (s *Store) WorktreeRestoreAttempts(ctx context.Context, scope SubmissionScope) ([]WorktreeRestoreAttempt, error) {
	binding, err := worktreeRestoreBinding(ctx, s.DB, scope)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+worktreeRestoreAttemptColumns+` FROM worktree_restore_attempts WHERE host_id=? AND app_id=? AND thread_id=? AND receipt_id IN (SELECT receipt_id FROM worktree_restore_grants WHERE host_id=? AND app_id=? AND thread_id=? AND binding_json=?) ORDER BY rowid`, scope.HostID, scope.AppID, scope.ThreadID, scope.HostID, scope.AppID, scope.ThreadID, binding)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorktreeRestoreAttempt
	for rows.Next() {
		a, err := scanWorktreeRestoreAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) LatestConfirmedWorktreeRestoreAttempt(ctx context.Context, scope SubmissionScope) (WorktreeRestoreAttempt, error) {
	binding, err := worktreeRestoreBinding(ctx, s.DB, scope)
	if err != nil {
		return WorktreeRestoreAttempt{}, err
	}
	return scanWorktreeRestoreAttempt(s.DB.QueryRowContext(ctx, `SELECT `+worktreeRestoreAttemptColumns+` FROM worktree_restore_attempts WHERE host_id=? AND app_id=? AND thread_id=? AND state='confirmed' AND receipt_id IN (SELECT receipt_id FROM worktree_restore_grants WHERE host_id=? AND app_id=? AND thread_id=? AND binding_json=?) ORDER BY rowid DESC LIMIT 1`, scope.HostID, scope.AppID, scope.ThreadID, scope.HostID, scope.AppID, scope.ThreadID, binding))
}

func (s *Store) ConfirmedWorktreeRestoreAttempts(ctx context.Context, scope SubmissionScope) ([]WorktreeRestoreAttempt, error) {
	all, err := s.WorktreeRestoreAttempts(ctx, scope)
	if err != nil {
		return nil, err
	}
	var confirmed []WorktreeRestoreAttempt
	for _, a := range all {
		if a.State == "confirmed" {
			confirmed = append(confirmed, a)
		}
	}
	return confirmed, nil
}

// Keep every other field byte-exact after insignificant JSON whitespace is
// removed. Raw values preserve large filesystem identity integers and future
// proof fields; duplicate top-level fields are never accepted.
func restorePublicationFields(raw string) (map[string]string, error) {
	if !restoreCheckpoint("restored", raw, true) {
		return nil, ErrConflict
	}
	d := json.NewDecoder(strings.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, ErrConflict
	}
	fields := map[string]string{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return nil, ErrConflict
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, ErrConflict
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, ErrConflict
		}
		var compact bytes.Buffer
		if json.Compact(&compact, value) != nil {
			return nil, ErrConflict
		}
		fields[name] = compact.String()
	}
	if _, err := d.Token(); err != nil {
		return nil, ErrConflict
	}
	var extra any
	if !errors.Is(d.Decode(&extra), io.EOF) {
		return nil, ErrConflict
	}
	return fields, nil
}

func sameRestorePublicationProof(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if key != "publicationPending" && b[key] != value {
			return false
		}
	}
	return true
}

// MarkWorktreeRestorePublished is called after actual publication readback. It
// only clears publicationPending on the same confirmed physical generation.
func (s *Store) MarkWorktreeRestorePublished(ctx context.Context, a WorktreeRestoreAttempt, result string, nowMS int64) error {
	before, err := restorePublicationFields(a.ResultJSON)
	if err != nil || a.State != "confirmed" || a.Phase != "restored" || before["publicationPending"] != "true" && before["publicationPending"] != "false" {
		return ErrConflict
	}
	after, err := restorePublicationFields(result)
	if err != nil || after["publicationPending"] != "false" || !sameRestorePublicationProof(before, after) {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	binding, err := worktreeRestoreBinding(ctx, tx, a.Scope)
	if err != nil {
		return err
	}
	if _, err := readWorktreeRestoreGrant(ctx, tx, a.Scope, a.ReceiptID, binding); err != nil {
		return err
	}
	current, err := readWorktreeRestoreAttempt(ctx, tx, a.Scope, a.ReceiptID)
	if err != nil || current.ID != a.ID || current.ParamsJSON != a.ParamsJSON || current.State != "confirmed" || current.Phase != "restored" {
		return ErrConflict
	}
	observed, err := restorePublicationFields(current.ResultJSON)
	if err != nil || !sameRestorePublicationProof(observed, after) {
		return ErrConflict
	}
	if observed["publicationPending"] == "false" {
		return tx.Commit() // Also accepts replay of the original true checkpoint.
	}
	if observed["publicationPending"] != "true" || current.ResultJSON != a.ResultJSON {
		return ErrConflict
	}
	r, err := tx.ExecContext(ctx, `UPDATE worktree_restore_attempts SET result_json=?,updated_at_ms=? WHERE attempt_id=? AND host_id=? AND app_id=? AND thread_id=? AND receipt_id=? AND params_json=? AND state='confirmed' AND phase='restored' AND result_json=?`, result, nowMS, a.ID, a.Scope.HostID, a.Scope.AppID, a.Scope.ThreadID, a.ReceiptID, a.ParamsJSON, a.ResultJSON)
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
	return tx.Commit()
}
