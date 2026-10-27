package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrIneligible = errors.New("thread is not eligible for automatic group creation")
var ErrConflict = errors.New("persistent binding or deployment conflicts")

type Observation struct {
	HostID, ThreadID, Origin, Proof, State, CWD, FirstUserItemID, ProjectRoot, ExclusionReason string
	CreatedAt                                                                                  int64
}
type GroupIntent struct {
	ID, HostID, ThreadID, OwnerID, Name, State, ChatID, Description string
	Members                                                         []string
}

func (s *Store) BaselineFence(ctx context.Context, host string) (int64, bool, error) {
	var started, completed int64
	err := s.DB.QueryRowContext(ctx, "SELECT baseline_started_at_ms,baseline_completed_at FROM deployment WHERE host_id=?", host).Scan(&started, &completed)
	return started, completed != 0, err
}

func (s *Store) PendingObservations(ctx context.Context, host string) ([]Observation, error) {
	// Bound routes are activated by native activity or durable work, not by
	// their old discovery eligibility. Including them here re-created dormant
	// actors after every discovery scan.
	rows, err := s.DB.QueryContext(ctx, `SELECT thread_id,origin,proof,state,cwd,created_at,first_user_item_id,project_root,exclusion_reason
		FROM thread_observations o WHERE host_id=? AND state IN ('pending','eligible')
		AND NOT EXISTS (SELECT 1 FROM bindings b WHERE b.host_id=o.host_id AND b.thread_id=o.thread_id)
		ORDER BY created_at,thread_id`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Observation
	for rows.Next() {
		o := Observation{HostID: host}
		if err := rows.Scan(&o.ThreadID, &o.Origin, &o.Proof, &o.State, &o.CWD, &o.CreatedAt, &o.FirstUserItemID, &o.ProjectRoot, &o.ExclusionReason); err != nil {
			return nil, err
		}
		result = append(result, o)
	}
	return result, rows.Err()
}

type Binding struct {
	HostID, ThreadID, IntentID, ChatID string
	// Empty FollowMode is the legacy group mode.
	FollowMode, RootMessageID, PlatformThreadID string
	RepairRequired                              bool
	RepairReason                                string
}

func (s *Store) Binding(ctx context.Context, host, thread string) (Binding, error) {
	b := Binding{HostID: host, ThreadID: thread}
	err := scanBinding(s.DB.QueryRowContext(ctx, "SELECT "+bindingColumns+" FROM bindings WHERE host_id=? AND thread_id=?", host, thread), &b)
	return b, err
}

func (s *Store) GroupIntent(ctx context.Context, id string) (GroupIntent, error) {
	var i GroupIntent
	var members string
	err := s.DB.QueryRowContext(ctx, "SELECT intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,description FROM create_intents WHERE intent_id=?", id).Scan(&i.ID, &i.HostID, &i.ThreadID, &i.OwnerID, &i.Name, &members, &i.State, &i.ChatID, &i.Description)
	if err != nil {
		return i, err
	}
	err = json.Unmarshal([]byte(members), &i.Members)
	return i, err
}

func (s *Store) BeginBaseline(ctx context.Context, host, app string, startedAt int64) (bool, error) {
	if host == "" || app == "" || startedAt <= 0 {
		return false, ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO deployment(host_id,app_id,baseline_started_at_ms) VALUES(?,?,?)", host, app, startedAt); err != nil {
		return false, err
	}
	var recordedApp string
	var completed int64
	if err = tx.QueryRowContext(ctx, "SELECT app_id,baseline_completed_at FROM deployment WHERE host_id=?", host).Scan(&recordedApp, &completed); err != nil {
		return false, err
	}
	if app != recordedApp {
		return false, ErrConflict
	}
	return completed == 0, tx.Commit()
}

// Snapshot and completion are atomic. Reopening a completed baseline is a no-op;
// candidates already recorded from proven new events retain their origin.
func (s *Store) CompleteBaseline(ctx context.Context, host string, ids []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var completed int64
	if err = tx.QueryRowContext(ctx, "SELECT baseline_completed_at FROM deployment WHERE host_id=?", host).Scan(&completed); err != nil {
		return err
	}
	if completed != 0 {
		return nil
	}
	for _, id := range ids {
		if id == "" {
			return ErrConflict
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO thread_observations(host_id,thread_id,origin,state) VALUES(?,?,'baseline','ignored') ON CONFLICT(host_id,thread_id) DO UPDATE SET origin='baseline',proof='baseline',state='ignored' WHERE thread_observations.origin='unknown' AND thread_observations.state='pending'", host, id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE deployment SET baseline_completed_at=? WHERE host_id=?", time.Now().Unix(), host); err != nil {
		return err
	}
	return tx.Commit()
}

// The controller must prove new origin using the event/baseline boundary. This
// method cannot promote a baseline/excluded row merely because its input changed.
func (s *Store) ObserveNew(ctx context.Context, host, thread, cwd string, createdAt int64) error {
	return s.ObserveCandidate(ctx, host, thread, cwd, createdAt, "birth")
}

// Unknown candidates survive restart without becoming eligible. Only explicit
// provenance can promote an ambiguous pending row; historical/excluded rows are
// immutable for automatic discovery.
func (s *Store) ObserveCandidate(ctx context.Context, host, thread, cwd string, createdAt int64, proof string) error {
	if host == "" || thread == "" || cwd == "" && proof != "ambiguous" {
		return ErrConflict
	}
	origin := "new"
	switch proof {
	case "birth", "draft", "v7":
	case "ambiguous":
		origin = "unknown"
	default:
		return ErrConflict
	}
	_, err := s.DB.ExecContext(ctx, "INSERT INTO thread_observations(host_id,thread_id,origin,proof,state,cwd,created_at) VALUES(?,?,?,?,'pending',?,?) ON CONFLICT(host_id,thread_id) DO UPDATE SET origin=excluded.origin,proof=excluded.proof,cwd=excluded.cwd,created_at=excluded.created_at WHERE thread_observations.origin='unknown' AND thread_observations.state='pending' AND excluded.origin='new'", host, thread, origin, proof, cwd, createdAt)
	return err
}

func (s *Store) NotePending(ctx context.Context, host, thread, reason string) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE thread_observations SET exclusion_reason=? WHERE host_id=? AND thread_id=? AND state IN ('pending','eligible')", reason, host, thread)
	return err
}

func (s *Store) HoldCandidate(ctx context.Context, host, thread, reason string) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE thread_observations SET state='pending',exclusion_reason=? WHERE host_id=? AND thread_id=? AND state IN ('pending','eligible') AND NOT EXISTS (SELECT 1 FROM bindings WHERE host_id=? AND thread_id=?)", reason, host, thread, host, thread)
	return err
}

func (s *Store) Observation(ctx context.Context, host, thread string) (Observation, error) {
	o := Observation{HostID: host, ThreadID: thread}
	err := s.DB.QueryRowContext(ctx, "SELECT origin,proof,state,cwd,created_at,first_user_item_id,project_root,exclusion_reason FROM thread_observations WHERE host_id=? AND thread_id=?", host, thread).Scan(&o.Origin, &o.Proof, &o.State, &o.CWD, &o.CreatedAt, &o.FirstUserItemID, &o.ProjectRoot, &o.ExclusionReason)
	return o, err
}

func (s *Store) MarkEligible(ctx context.Context, host, thread, userItem, cwd, projectRoot string) error {
	if userItem == "" || cwd == "" || projectRoot == "" {
		return ErrIneligible
	}
	r, err := s.DB.ExecContext(ctx, "UPDATE thread_observations SET state='eligible',first_user_item_id=?,cwd=?,project_root=?,exclusion_reason='' WHERE host_id=? AND thread_id=? AND origin='new' AND state IN ('pending','eligible')", userItem, cwd, projectRoot, host, thread)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return ErrIneligible
	}
	return err
}

func (s *Store) Exclude(ctx context.Context, host, thread, reason string) error {
	if reason == "" {
		return ErrConflict
	}
	_, err := s.DB.ExecContext(ctx, "UPDATE thread_observations SET state='excluded',exclusion_reason=? WHERE host_id=? AND thread_id=? AND state IN ('pending','eligible')", reason, host, thread)
	return err
}

// Persist all group parameters and a stable intent/UUID before remote dispatch.
func (s *Store) PrepareGroup(ctx context.Context, host, thread, owner, name string, members []string) (GroupIntent, error) {
	var intent GroupIntent
	if owner == "" || name == "" {
		return intent, ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return intent, err
	}
	defer tx.Rollback()
	var origin, state, userItem string
	if err = tx.QueryRowContext(ctx, "SELECT origin,state,first_user_item_id FROM thread_observations WHERE host_id=? AND thread_id=?", host, thread).Scan(&origin, &state, &userItem); err != nil {
		return intent, err
	}
	if origin != "new" || state != "eligible" || userItem == "" {
		return intent, ErrIneligible
	}
	memberJSON, err := json.Marshal(members)
	if err != nil {
		return intent, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO create_intents(intent_id,host_id,thread_id,owner_id,name,members_json,state,created_at,description) VALUES(?,?,?,?,?,?,'ready',?,'')", ID(), host, thread, owner, name, string(memberJSON), time.Now().Unix()); err != nil {
		return intent, err
	}
	var raw string
	err = tx.QueryRowContext(ctx, "SELECT intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,description FROM create_intents WHERE host_id=? AND thread_id=?", host, thread).Scan(&intent.ID, &intent.HostID, &intent.ThreadID, &intent.OwnerID, &intent.Name, &raw, &intent.State, &intent.ChatID, &intent.Description)
	if err != nil {
		return intent, err
	}
	if err = json.Unmarshal([]byte(raw), &intent.Members); err != nil {
		return intent, err
	}
	return intent, tx.Commit()
}

func (s *Store) ClaimGroupDispatch(ctx context.Context, id string) (bool, error) {
	_, claimed, err := s.ClaimGroupAttempt(ctx, id, time.Now().UnixMilli())
	return claimed, err
}

// FreezeGroupDescription records the attached thread's actual cwd before its
// first dispatch. Rate-limit retries keep the original description and UUID.
func (s *Store) FreezeGroupDescription(ctx context.Context, id, description string) (string, error) {
	if description == "" {
		return "", ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE create_intents SET description=? WHERE intent_id=? AND state='ready' AND description='' AND NOT EXISTS(SELECT 1 FROM group_attempts a WHERE a.intent_id=create_intents.intent_id)`, description, id); err != nil {
		return "", err
	}
	var frozen string
	if err := tx.QueryRowContext(ctx, "SELECT description FROM create_intents WHERE intent_id=?", id).Scan(&frozen); err != nil {
		return "", err
	}
	if frozen == "" {
		return "", ErrConflict
	}
	return frozen, tx.Commit()
}

// Only use after acquiring this deployment's process lock at startup.
func (s *Store) RecoverGroupDispatches(ctx context.Context, host string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE group_attempts SET state='unknown' WHERE state='dispatching' AND intent_id IN (SELECT intent_id FROM create_intents WHERE host_id=?)`, host); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE create_intents SET state='unknown' WHERE host_id=? AND state='dispatching'", host); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishGroupAttempt(ctx context.Context, id, state string) error {
	if state != "unknown" && state != "rejected" {
		return ErrConflict
	}
	return s.finishGroup(ctx, id, "", state, "")
}

// A known remote ID can recover an unknown legacy intent without another POST.
// Saving the binding remains a separate recoverable step.
func (s *Store) ConfirmGroup(ctx context.Context, id, chat string) error {
	return s.finishGroup(ctx, id, "", "confirmed", chat)
}

func (s *Store) BindConfirmedGroup(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var host, thread, chat, state string
	if err = tx.QueryRowContext(ctx, "SELECT host_id,thread_id,chat_id,state FROM create_intents WHERE intent_id=?", id).Scan(&host, &thread, &chat, &state); err != nil {
		return err
	}
	if state != "confirmed" || chat == "" {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO bindings(host_id,thread_id,intent_id,chat_id) VALUES(?,?,?,?)", host, thread, id, chat); err != nil {
		return err
	}
	var bound string
	if err = tx.QueryRowContext(ctx, "SELECT chat_id FROM bindings WHERE host_id=? AND thread_id=?", host, thread).Scan(&bound); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConflict
		}
		return err
	}
	if bound != chat {
		return ErrConflict
	}
	return tx.Commit()
}
