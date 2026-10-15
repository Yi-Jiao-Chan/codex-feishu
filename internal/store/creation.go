package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

// CreationSchema keeps UI drafts separate from the immutable remote-write
// journal. A draft's expiry never discards an already claimed creation intent.
const CreationSchema = `
CREATE TABLE IF NOT EXISTS creation_drafts (
 draft_id TEXT PRIMARY KEY, host_id TEXT NOT NULL REFERENCES deployment(host_id), app_id TEXT NOT NULL,
 creator_id TEXT NOT NULL, source_message_id TEXT NOT NULL, source_chat_id TEXT NOT NULL,
 source_chat_type TEXT NOT NULL, source_root_id TEXT NOT NULL DEFAULT '', source_thread_id TEXT NOT NULL DEFAULT '',
 body TEXT NOT NULL, directory TEXT NOT NULL DEFAULT '', project_id TEXT NOT NULL DEFAULT '',
 follow_mode TEXT NOT NULL DEFAULT 'group' CHECK(follow_mode IN ('group','topic')),
 target_chat_id TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 1,
 token TEXT NOT NULL UNIQUE, expires_at_ms INTEGER NOT NULL, created_at_ms INTEGER NOT NULL,
 state TEXT NOT NULL DEFAULT 'draft' CHECK(state IN ('draft','creating','created','canceled','expired')),
 view TEXT NOT NULL DEFAULT 'main', browse_path TEXT NOT NULL DEFAULT '', browse_cursor TEXT NOT NULL DEFAULT '',
 card_id TEXT NOT NULL DEFAULT '', card_revision INTEGER NOT NULL DEFAULT 0,
 error_code TEXT NOT NULL DEFAULT '', last_polled_at_ms INTEGER NOT NULL DEFAULT 0,
 UNIQUE(host_id,app_id,source_message_id)
);
CREATE TABLE IF NOT EXISTS creation_workflows (
 intent_id TEXT PRIMARY KEY, draft_id TEXT NOT NULL UNIQUE REFERENCES creation_drafts(draft_id),
 host_id TEXT NOT NULL, app_id TEXT NOT NULL, creator_id TEXT NOT NULL,
 body TEXT NOT NULL, directory TEXT NOT NULL, project_id TEXT NOT NULL, follow_mode TEXT NOT NULL,
 target_chat_id TEXT NOT NULL, owner_id TEXT NOT NULL, members_json TEXT NOT NULL, name TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'ready' CHECK(state IN ('ready','working','unknown','rejected','confirmed')),
 chat_id TEXT NOT NULL DEFAULT '', root_message_id TEXT NOT NULL DEFAULT '', platform_thread_id TEXT NOT NULL DEFAULT '',
 native_thread_id TEXT NOT NULL DEFAULT '', receipt_id TEXT NOT NULL DEFAULT '', initial_add_stage TEXT NOT NULL DEFAULT 'add',
 created_at_ms INTEGER NOT NULL, error_code TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS creation_attempts (
 attempt_id TEXT PRIMARY KEY, draft_id TEXT NOT NULL REFERENCES creation_drafts(draft_id),
 step TEXT NOT NULL, params_json TEXT NOT NULL, state TEXT NOT NULL
 CHECK(state IN ('prepared','dispatching','unknown','rejected','confirmed')),
 remote_id TEXT NOT NULL DEFAULT '', result_json TEXT NOT NULL DEFAULT '{}', created_at_ms INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS creation_one_pending_step ON creation_attempts(draft_id,step)
 WHERE state IN ('prepared','dispatching','unknown');
CREATE INDEX IF NOT EXISTS creation_attempt_order ON creation_attempts(draft_id,step,created_at_ms);
CREATE TABLE IF NOT EXISTS creation_actions (
 action_id TEXT PRIMARY KEY, draft_id TEXT NOT NULL REFERENCES creation_drafts(draft_id),
 revision INTEGER NOT NULL, kind TEXT NOT NULL, payload_json TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'ready' CHECK(state IN ('ready','working','confirmed','rejected')),
 created_at_ms INTEGER NOT NULL, UNIQUE(draft_id,revision)
);
`

// Existing drafts and workflows keep their original, local-directory mode.
func migrateCreationWorktrees(tx *sql.Tx) error {
	for _, column := range []struct{ table, name, definition string }{
		{"creation_drafts", "workspace_mode", "TEXT NOT NULL DEFAULT 'local' CHECK(workspace_mode IN ('local','worktree'))"},
		{"creation_drafts", "base_branch", "TEXT NOT NULL DEFAULT ''"},
		{"creation_workflows", "workspace_mode", "TEXT NOT NULL DEFAULT 'local' CHECK(workspace_mode IN ('local','worktree'))"},
		{"creation_workflows", "base_branch", "TEXT NOT NULL DEFAULT ''"},
		{"creation_workflows", "base_commit", "TEXT NOT NULL DEFAULT ''"},
		{"creation_workflows", "git_root", "TEXT NOT NULL DEFAULT ''"},
		{"creation_workflows", "git_common_dir", "TEXT NOT NULL DEFAULT ''"},
		{"creation_workflows", "worktree_path", "TEXT NOT NULL DEFAULT ''"},
	} {
		var present int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, column.table, column.name).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			if _, err := tx.Exec(`ALTER TABLE ` + column.table + ` ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
				return err
			}
		}
	}
	return nil
}

type CreationDraft struct {
	ID, HostID, AppID, CreatorID, SourceMessageID, SourceChatID, SourceChatType        string
	SourceRootID, SourceThreadID, Body, Directory, ProjectID, FollowMode, TargetChatID string
	Token, State, View, BrowsePath, BrowseCursor, CardID, ErrorCode                    string
	WorkspaceMode, BaseBranch                                                          string
	Revision, ExpiresAtMS, CreatedAtMS, CardRevision                                   int64
}

type CreationIntent struct {
	ID, DraftID, HostID, AppID, CreatorID, Body, Directory, ProjectID, FollowMode, TargetChatID         string
	OwnerID, Name, State, ChatID, RootMessageID, PlatformThreadID, NativeThreadID, ReceiptID, ErrorCode string
	WorkspaceMode, BaseBranch, BaseCommit, GitRoot, GitCommonDir, WorktreePath                          string
	Members                                                                                             []string
	CreatedAtMS                                                                                         int64
}

// ExecutionDirectory preserves a selected project subdirectory in the new
// checkout. The frozen worktree identity is validated before it is persisted.
func (i CreationIntent) ExecutionDirectory() string {
	if i.WorkspaceMode == "worktree" {
		rel, err := filepath.Rel(i.GitRoot, i.Directory)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		return filepath.Join(i.WorktreePath, rel)
	}
	return i.Directory
}

var creationCommit = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func validCreationWorkspace(i CreationIntent) bool {
	if i.WorkspaceMode == "local" {
		return i.BaseBranch == "" && i.BaseCommit == "" && i.GitRoot == "" && i.GitCommonDir == "" && i.WorktreePath == ""
	}
	return i.WorkspaceMode == "worktree" && i.BaseBranch != "" && creationCommit.MatchString(i.BaseCommit) &&
		filepath.IsAbs(i.GitRoot) && filepath.IsAbs(i.GitCommonDir) && filepath.IsAbs(i.WorktreePath) && i.ExecutionDirectory() != ""
}

type CreationAttempt struct {
	ID, DraftID, Step, Params, State, RemoteID, Result string
	CreatedAtMS                                        int64
}

type CreationAction struct {
	ID, DraftID, Kind, Payload, State string
	Revision, CreatedAtMS             int64
}

const creationDraftColumns = `draft_id,host_id,app_id,creator_id,source_message_id,source_chat_id,source_chat_type,source_root_id,source_thread_id,body,directory,project_id,follow_mode,target_chat_id,revision,token,expires_at_ms,created_at_ms,state,view,browse_path,browse_cursor,card_id,card_revision,error_code,workspace_mode,base_branch`
const creationIntentColumns = `intent_id,draft_id,host_id,app_id,creator_id,body,directory,project_id,follow_mode,target_chat_id,owner_id,members_json,name,state,chat_id,root_message_id,platform_thread_id,native_thread_id,receipt_id,created_at_ms,error_code,workspace_mode,base_branch,base_commit,git_root,git_common_dir,worktree_path`
const creationAttemptColumns = `attempt_id,draft_id,step,params_json,state,remote_id,result_json,created_at_ms`

func scanCreationDraft(row rowScanner) (d CreationDraft, err error) {
	err = row.Scan(&d.ID, &d.HostID, &d.AppID, &d.CreatorID, &d.SourceMessageID, &d.SourceChatID, &d.SourceChatType, &d.SourceRootID, &d.SourceThreadID, &d.Body, &d.Directory, &d.ProjectID, &d.FollowMode, &d.TargetChatID, &d.Revision, &d.Token, &d.ExpiresAtMS, &d.CreatedAtMS, &d.State, &d.View, &d.BrowsePath, &d.BrowseCursor, &d.CardID, &d.CardRevision, &d.ErrorCode, &d.WorkspaceMode, &d.BaseBranch)
	return
}

func scanCreationIntent(row rowScanner) (i CreationIntent, err error) {
	var members string
	err = row.Scan(&i.ID, &i.DraftID, &i.HostID, &i.AppID, &i.CreatorID, &i.Body, &i.Directory, &i.ProjectID, &i.FollowMode, &i.TargetChatID, &i.OwnerID, &members, &i.Name, &i.State, &i.ChatID, &i.RootMessageID, &i.PlatformThreadID, &i.NativeThreadID, &i.ReceiptID, &i.CreatedAtMS, &i.ErrorCode, &i.WorkspaceMode, &i.BaseBranch, &i.BaseCommit, &i.GitRoot, &i.GitCommonDir, &i.WorktreePath)
	if err == nil {
		err = json.Unmarshal([]byte(members), &i.Members)
	}
	return
}

func scanCreationAttempt(row rowScanner) (a CreationAttempt, err error) {
	err = row.Scan(&a.ID, &a.DraftID, &a.Step, &a.Params, &a.State, &a.RemoteID, &a.Result, &a.CreatedAtMS)
	return
}

func (s *Store) CreationDraft(ctx context.Context, id string) (CreationDraft, error) {
	return scanCreationDraft(s.DB.QueryRowContext(ctx, `SELECT `+creationDraftColumns+` FROM creation_drafts WHERE draft_id=?`, id))
}

func (s *Store) CreationDraftByToken(ctx context.Context, host, app, token string) (CreationDraft, error) {
	return scanCreationDraft(s.DB.QueryRowContext(ctx, `SELECT `+creationDraftColumns+` FROM creation_drafts WHERE host_id=? AND app_id=? AND token=?`, host, app, token))
}

// ReceiveCreationDraft deduplicates the source event even after cancellation,
// expiry, submission, or a response lost between commit and receiver return.
func (s *Store) ReceiveCreationDraft(ctx context.Context, d CreationDraft) (CreationDraft, bool, error) {
	if d.HostID == "" || d.AppID == "" || d.CreatorID == "" || d.SourceMessageID == "" || d.SourceChatID == "" || d.ExpiresAtMS <= d.CreatedAtMS || d.CreatedAtMS <= 0 || d.SourceChatType != "p2p" && d.SourceChatType != "group" {
		return d, false, ErrConflict
	}
	if d.FollowMode == "" {
		d.FollowMode = "group"
	}
	if d.WorkspaceMode == "" {
		d.WorkspaceMode = "local"
	}
	d.ID, d.Token = ID(), ID()
	r, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO creation_drafts(draft_id,host_id,app_id,creator_id,source_message_id,source_chat_id,source_chat_type,source_root_id,source_thread_id,body,directory,project_id,follow_mode,target_chat_id,token,expires_at_ms,created_at_ms,workspace_mode,base_branch) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, d.ID, d.HostID, d.AppID, d.CreatorID, d.SourceMessageID, d.SourceChatID, d.SourceChatType, d.SourceRootID, d.SourceThreadID, d.Body, d.Directory, d.ProjectID, d.FollowMode, d.TargetChatID, d.Token, d.ExpiresAtMS, d.CreatedAtMS, d.WorkspaceMode, d.BaseBranch)
	if err != nil {
		return d, false, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return d, false, err
	}
	saved, err := scanCreationDraft(s.DB.QueryRowContext(ctx, `SELECT `+creationDraftColumns+` FROM creation_drafts WHERE host_id=? AND app_id=? AND source_message_id=?`, d.HostID, d.AppID, d.SourceMessageID))
	if err == nil && (saved.CreatorID != d.CreatorID || saved.SourceChatID != d.SourceChatID || saved.SourceChatType != d.SourceChatType) {
		return saved, false, ErrConflict
	}
	return saved, n == 1, err
}

// UpdateCreationDraft applies only fields revalidated by the use case. It
// never accepts a native thread or a remote object ID from a callback.
func (s *Store) UpdateCreationDraft(ctx context.Context, d CreationDraft, expected int64, nowMS int64) (CreationDraft, error) {
	if d.WorkspaceMode == "" {
		d.WorkspaceMode = "local"
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE creation_drafts SET body=?,directory=?,project_id=?,follow_mode=?,target_chat_id=?,view=?,browse_path=?,browse_cursor=?,error_code=?,workspace_mode=?,base_branch=?,revision=revision+1 WHERE draft_id=? AND host_id=? AND app_id=? AND creator_id=? AND token=? AND card_id=? AND revision=? AND state='draft' AND expires_at_ms>?`, d.Body, d.Directory, d.ProjectID, d.FollowMode, d.TargetChatID, d.View, d.BrowsePath, d.BrowseCursor, d.ErrorCode, d.WorkspaceMode, d.BaseBranch, d.ID, d.HostID, d.AppID, d.CreatorID, d.Token, d.CardID, expected, nowMS)
	if err != nil {
		return d, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return d, err
	}
	if n != 1 {
		return d, ErrSubmissionStale
	}
	return s.CreationDraft(ctx, d.ID)
}

func (s *Store) CancelCreationDraft(ctx context.Context, d CreationDraft, nowMS int64) error {
	r, err := s.DB.ExecContext(ctx, `UPDATE creation_drafts SET state='canceled',revision=revision+1 WHERE draft_id=? AND creator_id=? AND revision=? AND state='draft' AND expires_at_ms>?`, d.ID, d.CreatorID, d.Revision, nowMS)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrSubmissionStale
	}
	return nil
}

func (s *Store) ClaimCreationIntent(ctx context.Context, d CreationDraft, i CreationIntent, nowMS int64) (CreationIntent, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return i, false, err
	}
	defer tx.Rollback()
	old, err := scanCreationIntent(tx.QueryRowContext(ctx, `SELECT `+creationIntentColumns+` FROM creation_workflows WHERE draft_id=?`, d.ID))
	if err == nil {
		return old, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return i, false, err
	}
	if strings.TrimSpace(d.Body) == "" || d.Directory == "" || i.OwnerID == "" {
		return i, false, ErrConflict
	}
	if d.WorkspaceMode == "" {
		d.WorkspaceMode = "local"
	}
	i.Directory, i.WorkspaceMode, i.BaseBranch = d.Directory, d.WorkspaceMode, d.BaseBranch
	if !validCreationWorkspace(i) {
		return i, false, ErrConflict
	}
	r, err := tx.ExecContext(ctx, `UPDATE creation_drafts SET body=?,directory=?,project_id=?,follow_mode=?,target_chat_id=?,workspace_mode=?,base_branch=?,state='creating',revision=revision+1,error_code='' WHERE draft_id=? AND host_id=? AND app_id=? AND creator_id=? AND token=? AND card_id=? AND revision=? AND state='draft' AND expires_at_ms>?`, d.Body, d.Directory, d.ProjectID, d.FollowMode, d.TargetChatID, d.WorkspaceMode, d.BaseBranch, d.ID, d.HostID, d.AppID, d.CreatorID, d.Token, d.CardID, d.Revision, nowMS)
	if err != nil {
		return i, false, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return i, false, ErrSubmissionStale
	}
	i.ID, i.DraftID, i.HostID, i.AppID, i.CreatorID = ID(), d.ID, d.HostID, d.AppID, d.CreatorID
	i.Body, i.Directory, i.ProjectID, i.FollowMode, i.TargetChatID = d.Body, d.Directory, d.ProjectID, d.FollowMode, d.TargetChatID
	i.State, i.CreatedAtMS = "ready", nowMS
	members, err := json.Marshal(i.Members)
	if err != nil {
		return i, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO creation_workflows(intent_id,draft_id,host_id,app_id,creator_id,body,directory,project_id,follow_mode,target_chat_id,owner_id,members_json,name,state,created_at_ms,workspace_mode,base_branch,base_commit,git_root,git_common_dir,worktree_path) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'ready',?,?,?,?,?,?,?)`, i.ID, i.DraftID, i.HostID, i.AppID, i.CreatorID, i.Body, i.Directory, i.ProjectID, i.FollowMode, i.TargetChatID, i.OwnerID, string(members), i.Name, nowMS, i.WorkspaceMode, i.BaseBranch, i.BaseCommit, i.GitRoot, i.GitCommonDir, i.WorktreePath)
	if err != nil {
		return i, false, err
	}
	return i, true, tx.Commit()
}

func (s *Store) CreationIntent(ctx context.Context, draft string) (CreationIntent, error) {
	return scanCreationIntent(s.DB.QueryRowContext(ctx, `SELECT `+creationIntentColumns+` FROM creation_workflows WHERE draft_id=?`, draft))
}

func (s *Store) CreationDrafts(ctx context.Context, host, app string, nowMS int64) ([]CreationDraft, error) {
	if _, err := s.DB.ExecContext(ctx, `UPDATE creation_drafts SET state='expired',revision=revision+1 WHERE host_id=? AND app_id=? AND state='draft' AND expires_at_ms<=?`, host, app, nowMS); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+creationDraftColumns+` FROM creation_drafts WHERE host_id=? AND app_id=? AND (state='creating' OR card_revision<revision) ORDER BY last_polled_at_ms,created_at_ms,draft_id LIMIT 64`, host, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreationDraft
	for rows.Next() {
		d, err := scanCreationDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CreatingDrafts is owned by the single remote-creation worker. UI polling
// uses CreationDrafts independently, so a slow native RPC cannot hold its lane.
func (s *Store) CreatingDrafts(ctx context.Context, host, app string) ([]CreationDraft, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+creationDraftColumns+` FROM creation_drafts WHERE host_id=? AND app_id=? AND state='creating' ORDER BY last_polled_at_ms,created_at_ms,draft_id LIMIT 64`, host, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreationDraft
	for rows.Next() {
		d, err := scanCreationDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Polling order is persisted per visited draft. Marking an entire selected
// batch would still starve its tail when an early unknown consumes the budget.
func (s *Store) PollCreationDraft(ctx context.Context, host, app, draft string, nowMS int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE creation_drafts SET last_polled_at_ms=MAX(?,(SELECT COALESCE(MAX(last_polled_at_ms),0)+1 FROM creation_drafts WHERE host_id=? AND app_id=?)) WHERE draft_id=? AND host_id=? AND app_id=?`, nowMS, host, app, draft, host, app)
	return err
}

// PrepareCreationAttempt freezes the complete request before a separate
// dispatch claim. A rejected attempt may be replaced only by an explicit retry.
func (s *Store) PrepareCreationAttempt(ctx context.Context, draft, step, params string, nowMS int64) (CreationAttempt, error) {
	if draft == "" || step == "" || !json.Valid([]byte(params)) {
		return CreationAttempt{}, ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return CreationAttempt{}, err
	}
	defer tx.Rollback()
	a, err := scanCreationAttempt(tx.QueryRowContext(ctx, `SELECT `+creationAttemptColumns+` FROM creation_attempts WHERE draft_id=? AND step=? ORDER BY rowid DESC LIMIT 1`, draft, step))
	if err == nil {
		if a.Params != params {
			return a, ErrConflict
		}
		return a, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return a, err
	}
	a = CreationAttempt{ID: ID(), DraftID: draft, Step: step, Params: params, State: "prepared", CreatedAtMS: nowMS}
	_, err = tx.ExecContext(ctx, `INSERT INTO creation_attempts(attempt_id,draft_id,step,params_json,state,created_at_ms) VALUES(?,?,?,?,'prepared',?)`, a.ID, draft, step, params, nowMS)
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func (s *Store) DispatchCreationAttempt(ctx context.Context, a CreationAttempt) (bool, error) {
	r, err := s.DB.ExecContext(ctx, `UPDATE creation_attempts SET state='dispatching' WHERE attempt_id=? AND draft_id=? AND step=? AND params_json=? AND state='prepared' AND (step NOT LIKE 'entry:%' OR NOT EXISTS(SELECT 1 FROM creation_attempts prior WHERE prior.draft_id=creation_attempts.draft_id AND prior.step LIKE 'entry:%' AND prior.state IN ('prepared','dispatching','unknown') AND prior.rowid<creation_attempts.rowid))`, a.ID, a.DraftID, a.Step, a.Params)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *Store) CreationAttempt(ctx context.Context, draft, step string) (CreationAttempt, error) {
	return scanCreationAttempt(s.DB.QueryRowContext(ctx, `SELECT `+creationAttemptColumns+` FROM creation_attempts WHERE draft_id=? AND step=? ORDER BY rowid DESC LIMIT 1`, draft, step))
}

func (s *Store) FinishCreationAttempt(ctx context.Context, a CreationAttempt, outcome, remote, result string) error {
	if outcome != "confirmed" && outcome != "rejected" && outcome != "unknown" || !json.Valid([]byte(result)) || outcome == "confirmed" && remote == "" {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if a.Step == "worktree" && outcome == "confirmed" {
		var path string
		if err = tx.QueryRowContext(ctx, `SELECT worktree_path FROM creation_workflows WHERE draft_id=? AND workspace_mode='worktree'`, a.DraftID).Scan(&path); err != nil || path == "" || path != remote {
			return ErrConflict
		}
	}
	r, err := tx.ExecContext(ctx, `UPDATE creation_attempts SET state=?,remote_id=?,result_json=? WHERE attempt_id=? AND draft_id=? AND step=? AND params_json=? AND state IN ('dispatching','unknown')`, outcome, remote, result, a.ID, a.DraftID, a.Step, a.Params)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	if a.Step == "group" || a.Step == "root" || a.Step == "native" || a.Step == "worktree" {
		state, code := "working", ""
		if outcome != "confirmed" {
			state, code = outcome, a.Step+"-"+outcome
		}
		if _, err = tx.ExecContext(ctx, `UPDATE creation_workflows SET state=?,error_code=? WHERE draft_id=?`, state, code, a.DraftID); err != nil {
			return err
		}
		if outcome == "confirmed" && a.Step != "worktree" {
			column := map[string]string{"group": "chat_id", "root": "root_message_id", "native": "native_thread_id"}[a.Step]
			if _, err = tx.ExecContext(ctx, `UPDATE creation_workflows SET `+column+`=? WHERE draft_id=? AND (`+column+`='' OR `+column+`=?)`, remote, a.DraftID, remote); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE creation_drafts SET error_code=?,revision=revision+1 WHERE draft_id=?`, code, a.DraftID); err != nil {
			return err
		}
	}
	if strings.HasPrefix(a.Step, "entry:") && outcome == "confirmed" {
		var p struct {
			Revision int64 `json:"revision"`
		}
		if json.Unmarshal([]byte(a.Params), &p) != nil || p.Revision <= 0 {
			return ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE creation_drafts SET card_id=?,card_revision=MAX(card_revision,?) WHERE draft_id=? AND (card_id='' OR card_id=?)`, remote, p.Revision, a.DraftID, remote); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RecoverCreations(ctx context.Context, host, app string) error {
	return s.recoverCreationLanes(ctx, host, app, true, true)
}

// Each live worker recovers only its own previous dispatches after acquiring
// its lane mutex. It must never classify another worker's in-flight RPC lost.
func (s *Store) RecoverCreationUI(ctx context.Context, host, app string) error {
	return s.recoverCreationLanes(ctx, host, app, true, false)
}

func (s *Store) RecoverCreationWorkflows(ctx context.Context, host, app string) error {
	return s.recoverCreationLanes(ctx, host, app, false, true)
}

func (s *Store) recoverCreationLanes(ctx context.Context, host, app string, ui, workflows bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE creation_attempts SET state='unknown' WHERE state='dispatching' AND ((? AND step LIKE 'entry:%') OR (? AND step NOT LIKE 'entry:%')) AND draft_id IN (SELECT draft_id FROM creation_drafts WHERE host_id=? AND app_id=?)`, ui, workflows, host, app); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE creation_workflows SET state='unknown',error_code='recovery-unknown' WHERE ? AND host_id=? AND app_id=? AND EXISTS(SELECT 1 FROM creation_attempts a WHERE a.draft_id=creation_workflows.draft_id AND a.step IN ('group','root','native','worktree') AND a.state='unknown')`, workflows, host, app); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE creation_drafts SET error_code='recovery-unknown',revision=revision+1 WHERE ? AND host_id=? AND app_id=? AND error_code<>'recovery-unknown' AND EXISTS(SELECT 1 FROM creation_workflows i WHERE i.draft_id=creation_drafts.draft_id AND i.state='unknown' AND i.error_code='recovery-unknown')`, workflows, host, app); err != nil {
		return err
	}
	// UI mutations are local and revision protected; a worker interrupted
	// before marking its action done can safely inspect that revision again.
	if _, err = tx.ExecContext(ctx, `UPDATE creation_actions SET state='ready' WHERE ? AND state='working' AND draft_id IN (SELECT draft_id FROM creation_drafts WHERE host_id=? AND app_id=?)`, ui, host, app); err != nil {
		return err
	}
	return tx.Commit()
}

// A retry is a fresh attempt only after a definite rejection. Unknowns stay
// immutable and cannot be cleared by a button, expiry, or process restart.
func (s *Store) RetryCreation(ctx context.Context, d CreationDraft) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM creation_workflows WHERE draft_id=?`, d.ID).Scan(&state); err != nil {
		return err
	}
	if state != "rejected" {
		return ErrSubmissionStale
	}
	var receipt, code string
	if err = tx.QueryRowContext(ctx, `SELECT receipt_id,error_code FROM creation_workflows WHERE draft_id=?`, d.ID).Scan(&receipt, &code); err != nil {
		return err
	}
	if code == "input-rejected" && receipt != "" {
		var unknown, rejected int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submission_attempts WHERE receipt_id=? AND state IN ('unknown','dispatching')`, receipt).Scan(&unknown); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submission_attempts WHERE receipt_id=? AND state='rejected'`, receipt).Scan(&rejected); err != nil {
			return err
		}
		if unknown != 0 || rejected == 0 {
			return ErrSubmissionStale
		}
		stage := "add:creation:" + ID()
		if _, err = tx.ExecContext(ctx, `UPDATE creation_workflows SET initial_add_stage=? WHERE draft_id=?`, stage, d.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE submission_inputs SET state='received',revision=revision+1 WHERE receipt_id=? AND state='failed'`, receipt); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE m0_receipts SET state='received' WHERE id=? AND state='failed'`, receipt); err != nil {
			return err
		}
	}
	// Keep the old request and outcome intact. Only a confirmed rejection
	// authorizes a fresh prepared attempt with the very same frozen params.
	if _, err = tx.ExecContext(ctx, `INSERT INTO creation_attempts(attempt_id,draft_id,step,params_json,state,created_at_ms)
 SELECT lower(hex(randomblob(16))),draft_id,step,params_json,'prepared',created_at_ms+1 FROM creation_attempts a
 WHERE draft_id=? AND step IN ('group','root','native','worktree') AND state='rejected'
 AND rowid=(SELECT MAX(rowid) FROM creation_attempts latest WHERE latest.draft_id=a.draft_id AND latest.step=a.step)`, d.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE creation_workflows SET state='ready',error_code='' WHERE draft_id=? AND state='rejected'`, d.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE creation_drafts SET error_code='',revision=revision+1 WHERE draft_id=?`, d.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) InitialCreationAddStage(ctx context.Context, host, app, thread, receipt string) (string, error) {
	var stage string
	err := s.DB.QueryRowContext(ctx, `SELECT initial_add_stage FROM creation_workflows WHERE host_id=? AND app_id=? AND native_thread_id=? AND receipt_id=?`, host, app, thread, receipt).Scan(&stage)
	return stage, err
}

func (s *Store) SetCreationInputState(ctx context.Context, draft, state string) error {
	if state != "unknown" && state != "rejected" {
		return ErrConflict
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE creation_workflows SET state=?,error_code=? WHERE draft_id=?`, state, "input-"+state, draft)
	if err == nil {
		err = s.SetCreationError(ctx, draft, "input-"+state)
	}
	return err
}

func (s *Store) SetCreationError(ctx context.Context, draft, code string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE creation_drafts SET error_code=?,revision=revision+1 WHERE draft_id=? AND error_code<>?`, code, draft, code)
	return err
}

// Each consumed UI version must advance, even when the user encounters the
// same validation error again. Otherwise its rejected action owns that version
// forever and prevents selecting a directory or canceling the draft.
func (s *Store) RefreshCreationAction(ctx context.Context, a CreationAction, code string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE creation_drafts SET error_code=?,revision=revision+1 WHERE draft_id=? AND revision=?`, code, a.DraftID, a.Revision)
	return err
}

func (s *Store) CompleteCreation(ctx context.Context, draft string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM creation_workflows i JOIN submission_inputs n ON n.receipt_id=i.receipt_id WHERE i.draft_id=? AND i.native_thread_id<>'' AND n.state IN ('accepted','queued','consumed','steered')`, draft).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE creation_workflows SET state='confirmed',error_code='' WHERE draft_id=?`, draft); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE creation_drafts SET state='created',error_code='',revision=revision+1 WHERE draft_id=? AND state='creating'`, draft); err != nil {
		return err
	}
	return tx.Commit()
}

// MaterializeCreation uses the confirmed native ID and confirmed target only.
// First input keeps the original message for ingress dedup/reactions and a
// distinct local root as its reply anchor when creation crosses chats.
func (s *Store) MaterializeCreation(ctx context.Context, draft string) (Binding, Receipt, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Binding{}, Receipt{}, err
	}
	defer tx.Rollback()
	i, err := scanCreationIntent(tx.QueryRowContext(ctx, `SELECT `+creationIntentColumns+` FROM creation_workflows WHERE draft_id=?`, draft))
	if err != nil {
		return Binding{}, Receipt{}, err
	}
	d, err := scanCreationDraft(tx.QueryRowContext(ctx, `SELECT `+creationDraftColumns+` FROM creation_drafts WHERE draft_id=?`, draft))
	if err != nil {
		return Binding{}, Receipt{}, err
	}
	if i.NativeThreadID == "" || i.RootMessageID == "" || i.State == "rejected" {
		return Binding{}, Receipt{}, ErrConflict
	}
	if i.WorkspaceMode == "worktree" {
		var state, path string
		if err = tx.QueryRowContext(ctx, `SELECT state,remote_id FROM creation_attempts WHERE draft_id=? AND step='worktree' ORDER BY rowid DESC LIMIT 1`, draft).Scan(&state, &path); err != nil || state != "confirmed" || path != i.WorktreePath {
			return Binding{}, Receipt{}, ErrConflict
		}
	}
	chat := i.ChatID
	if i.FollowMode == "topic" {
		chat = i.TargetChatID
	}
	if chat == "" {
		return Binding{}, Receipt{}, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO thread_observations(host_id,thread_id,origin,proof,state,cwd,created_at,exclusion_reason) VALUES(?,?,'new','birth','excluded',?,?,'bridge_creation') ON CONFLICT(host_id,thread_id) DO UPDATE SET state='excluded',exclusion_reason='bridge_creation'`, i.HostID, i.NativeThreadID, i.ExecutionDirectory(), i.CreatedAtMS/1000); err != nil {
		return Binding{}, Receipt{}, err
	}
	members, _ := json.Marshal(i.Members)
	if _, err = tx.ExecContext(ctx, `INSERT INTO create_intents(intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,created_at,description) VALUES(?,?,?,?,?,?,'confirmed',?,?,?) ON CONFLICT(host_id,thread_id) DO NOTHING`, i.ID, i.HostID, i.NativeThreadID, i.OwnerID, i.Name, string(members), chat, i.CreatedAtMS/1000, "工作目录："+i.ExecutionDirectory()); err != nil {
		return Binding{}, Receipt{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO bindings(host_id,thread_id,intent_id,chat_id,follow_mode,root_message_id,platform_thread_id) VALUES(?,?,?,?,?,?,?) ON CONFLICT(host_id,thread_id) DO NOTHING`, i.HostID, i.NativeThreadID, i.ID, chat, i.FollowMode, i.RootMessageID, i.PlatformThreadID); err != nil {
		return Binding{}, Receipt{}, err
	}
	var b Binding
	if err = scanBinding(tx.QueryRowContext(ctx, `SELECT `+bindingColumns+` FROM bindings WHERE host_id=? AND thread_id=?`, i.HostID, i.NativeThreadID), &b); err != nil {
		return b, Receipt{}, err
	}
	if b.IntentID != i.ID || b.ChatID != chat || b.RootMessageID != i.RootMessageID || (b.FollowMode == "topic") != (i.FollowMode == "topic") {
		return b, Receipt{}, ErrConflict
	}
	r := Receipt{ID: ID(), AppID: i.AppID, MessageID: d.SourceMessageID, ReplyMessageID: i.RootMessageID, ThreadID: i.NativeThreadID, ChatID: chat, Body: i.Body, ActionToken: ID(), State: "received"}
	r.ClientID = "cfb-" + r.ID
	if _, err = tx.ExecContext(ctx, `INSERT INTO m0_receipts(id,app_id,message_id,reply_message_id,thread_id,chat_id,body,client_id,state,action_token) VALUES(?,?,?,?,?,?,?,?,'received',?) ON CONFLICT(app_id,message_id) DO NOTHING`, r.ID, r.AppID, r.MessageID, r.ReplyMessageID, r.ThreadID, r.ChatID, r.Body, r.ClientID, r.ActionToken); err != nil {
		return b, r, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT id,client_id,action_token,state,thread_id,chat_id,reply_message_id FROM m0_receipts WHERE app_id=? AND message_id=?`, i.AppID, d.SourceMessageID).Scan(&r.ID, &r.ClientID, &r.ActionToken, &r.State, &r.ThreadID, &r.ChatID, &r.ReplyMessageID); err != nil {
		return b, r, err
	}
	if r.ThreadID != i.NativeThreadID || r.ChatID != chat || r.ReplyMessageID != i.RootMessageID {
		return b, r, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE creation_workflows SET receipt_id=? WHERE intent_id=? AND (receipt_id='' OR receipt_id=?)`, r.ID, i.ID, r.ID); err != nil {
		return b, r, err
	}
	return b, r, tx.Commit()
}

// BridgeCreationThread is stronger than a source label: the local intent,
// target and native identity must have all been confirmed and materialized.
func (s *Store) BridgeCreationThread(ctx context.Context, host, app, thread string) (CreationIntent, error) {
	return scanCreationIntent(s.DB.QueryRowContext(ctx, `SELECT `+creationIntentColumns+` FROM creation_workflows WHERE host_id=? AND app_id=? AND native_thread_id=? AND receipt_id<>''`, host, app, thread))
}

func (s *Store) ConfirmedCreationThreads(ctx context.Context, host, app string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT i.native_thread_id FROM creation_workflows i JOIN bindings b ON b.host_id=i.host_id AND b.thread_id=i.native_thread_id AND b.intent_id=i.intent_id WHERE i.host_id=? AND i.app_id=? AND i.state='confirmed'`, host, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClaimCreationAction is callback-only local work. It checks the actual card,
// creator, expiry, token and version before an async worker touches any RPC.
func (s *Store) ClaimCreationAction(ctx context.Context, host, app, user, chat, message, token string, revision int64, kind, payload string, nowMS int64) (CreationAction, bool, error) {
	if revision <= 0 || !strings.HasPrefix(kind, "creation_") || !json.Valid([]byte(payload)) || len(payload) > 32768 {
		return CreationAction{}, false, ErrSubmissionStale
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return CreationAction{}, false, err
	}
	defer tx.Rollback()
	d, err := scanCreationDraft(tx.QueryRowContext(ctx, `SELECT `+creationDraftColumns+` FROM creation_drafts WHERE host_id=? AND app_id=? AND token=?`, host, app, token))
	if err != nil {
		return CreationAction{}, false, ErrSubmissionStale
	}
	if d.CreatorID != user || d.SourceChatID != chat || d.CardID == "" || d.CardID != message {
		return CreationAction{}, false, ErrSubmissionStale
	}
	var old CreationAction
	err = tx.QueryRowContext(ctx, `SELECT action_id,draft_id,kind,payload_json,state,revision,created_at_ms FROM creation_actions WHERE draft_id=? AND revision=?`, d.ID, revision).Scan(&old.ID, &old.DraftID, &old.Kind, &old.Payload, &old.State, &old.Revision, &old.CreatedAtMS)
	if err == nil {
		if old.Kind != kind || old.Payload != payload {
			return old, false, ErrSubmissionStale
		}
		return old, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return old, false, err
	}
	if d.Revision != revision || d.State == "draft" && d.ExpiresAtMS <= nowMS || d.State == "canceled" || d.State == "expired" {
		return old, false, ErrSubmissionStale
	}
	if d.State != "draft" && kind != "creation_check" && kind != "creation_retry" && kind != "creation_submit" {
		return old, false, ErrSubmissionStale
	}
	a := CreationAction{ID: ID(), DraftID: d.ID, Kind: kind, Payload: payload, State: "ready", Revision: revision, CreatedAtMS: nowMS}
	_, err = tx.ExecContext(ctx, `INSERT INTO creation_actions(action_id,draft_id,revision,kind,payload_json,created_at_ms) VALUES(?,?,?,?,?,?)`, a.ID, a.DraftID, revision, kind, payload, nowMS)
	if err != nil {
		return a, false, err
	}
	return a, true, tx.Commit()
}

func (s *Store) NextCreationAction(ctx context.Context, host, app string) (CreationAction, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return CreationAction{}, false, err
	}
	defer tx.Rollback()
	var a CreationAction
	err = tx.QueryRowContext(ctx, `SELECT a.action_id,a.draft_id,a.kind,a.payload_json,a.state,a.revision,a.created_at_ms FROM creation_actions a JOIN creation_drafts d ON d.draft_id=a.draft_id WHERE d.host_id=? AND d.app_id=? AND a.state='ready' ORDER BY a.rowid LIMIT 1`, host, app).Scan(&a.ID, &a.DraftID, &a.Kind, &a.Payload, &a.State, &a.Revision, &a.CreatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE creation_actions SET state='working' WHERE action_id=? AND state='ready'`, a.ID); err != nil {
		return a, false, err
	}
	a.State = "working"
	return a, true, tx.Commit()
}

func (s *Store) FinishCreationAction(ctx context.Context, a CreationAction, ok bool) error {
	state := "rejected"
	if ok {
		state = "confirmed"
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE creation_actions SET state=? WHERE action_id=? AND draft_id=? AND state='working'`, state, a.ID, a.DraftID)
	return err
}
