package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const VerificationBindingSchema = `
CREATE TABLE IF NOT EXISTS verification_bindings (
 request_id TEXT PRIMARY KEY CHECK(length(request_id)=32),
 host_id TEXT NOT NULL, app_id TEXT NOT NULL, owner_id TEXT NOT NULL,
 bridge_commit TEXT NOT NULL, binary_sha256 TEXT NOT NULL CHECK(length(binary_sha256)=64),
 bot_id TEXT NOT NULL CHECK(bot_id<>owner_id), thread_id TEXT NOT NULL, chat_id TEXT NOT NULL,
 cwd TEXT NOT NULL, intent_id TEXT NOT NULL,
 bound_at_ms INTEGER NOT NULL CHECK(bound_at_ms>0),
 expires_at_ms INTEGER NOT NULL CHECK(expires_at_ms>bound_at_ms),
 membership_verified_at_ms INTEGER NOT NULL CHECK(membership_verified_at_ms=bound_at_ms),
 UNIQUE(host_id,thread_id), UNIQUE(host_id,chat_id), UNIQUE(intent_id),
 FOREIGN KEY(request_id) REFERENCES verification_native(request_id),
 FOREIGN KEY(request_id) REFERENCES verification_groups(request_id),
 FOREIGN KEY(intent_id) REFERENCES create_intents(intent_id)
);
`

// VerificationBinding is the private local proof that an explicitly
// authorized native birth and owner-only group observation refer to one
// durable binding. It is never a public or diagnostic payload.
type VerificationBinding struct {
	RequestID, HostID, AppID, OwnerID, BridgeCommit, BinarySHA256 string `json:"-"`
	BotID, ThreadID, ChatID, CWD, IntentID                        string `json:"-"`
	BoundAtMS, ExpiresAtMS, MembershipVerifiedAtMS                int64  `json:"-"`
}

func (s *Store) VerificationBinding(ctx context.Context, id, host, app, owner string) (VerificationBinding, error) {
	return scanVerificationBinding(ctx, s.DB, id, host, app, owner)
}

func (s *Store) VerificationBindingForThread(ctx context.Context, host, thread string) (VerificationBinding, error) {
	var b VerificationBinding
	err := s.DB.QueryRowContext(ctx, `SELECT request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,bot_id,thread_id,chat_id,cwd,intent_id,bound_at_ms,expires_at_ms,membership_verified_at_ms
 FROM verification_bindings WHERE host_id=? AND thread_id=?`, host, thread).Scan(
		&b.RequestID, &b.HostID, &b.AppID, &b.OwnerID, &b.BridgeCommit, &b.BinarySHA256, &b.BotID, &b.ThreadID, &b.ChatID, &b.CWD, &b.IntentID, &b.BoundAtMS, &b.ExpiresAtMS, &b.MembershipVerifiedAtMS)
	return b, err
}

func (s *Store) RegisterVerificationBinding(ctx context.Context, n VerificationNative, g VerificationGroup, bot string, now int64) (VerificationBinding, error) {
	if err := validateVerificationBindingInputs(n, g, bot, now); err != nil {
		return VerificationBinding{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return VerificationBinding{}, err
	}
	defer tx.Rollback()

	dbNative, err := scanVerificationNativeTx(ctx, tx, n.RequestID)
	if err != nil {
		return VerificationBinding{}, err
	}
	dbGroup, err := scanVerificationGroupTx(ctx, tx, g.RequestID)
	if err != nil {
		return VerificationBinding{}, err
	}
	if err := validateVerificationBindingInputs(dbNative, dbGroup, bot, now); err != nil {
		return VerificationBinding{}, err
	}
	if !sameVerificationNative(n, dbNative) || !sameVerificationGroup(g, dbGroup) {
		return VerificationBinding{}, ErrConflict
	}
	if dbNative.HostID != dbGroup.HostID || dbNative.AppID != dbGroup.AppID || dbNative.OwnerID != dbGroup.OwnerID || dbNative.BridgeCommit != dbGroup.BridgeCommit || dbNative.BinarySHA256 != dbGroup.BinarySHA256 {
		return VerificationBinding{}, ErrConflict
	}
	var deploymentApp string
	var baselineCompleted int64
	if err := tx.QueryRowContext(ctx, `SELECT app_id,baseline_completed_at FROM deployment WHERE host_id=?`, n.HostID).Scan(&deploymentApp, &baselineCompleted); err != nil {
		return VerificationBinding{}, err
	}
	if deploymentApp != n.AppID || baselineCompleted <= 0 {
		return VerificationBinding{}, ErrConflict
	}

	observation, observationExists, err := scanBindingObservation(ctx, tx, n.HostID, n.ThreadID)
	if err != nil {
		return VerificationBinding{}, err
	}
	if observationExists && observation.CWD != n.CWD {
		return VerificationBinding{}, ErrConflict
	}

	intentID := verificationBindingIntentID(n.HostID, n.AppID, n.OwnerID, n.RequestID)
	intent, intentExists, err := scanBindingIntent(ctx, tx, n.HostID, n.ThreadID)
	if err != nil {
		return VerificationBinding{}, err
	}
	if intentExists && !sameBindingIntent(intent, intentID, n.HostID, n.ThreadID, n.OwnerID, g.Name, g.MembersJSON, g.ChatID) {
		return VerificationBinding{}, ErrConflict
	}
	intentByID, intentByIDExists, err := scanBindingIntentByID(ctx, tx, intentID)
	if err != nil {
		return VerificationBinding{}, err
	}
	if intentByIDExists && !sameBindingIntent(intentByID, intentID, n.HostID, n.ThreadID, n.OwnerID, g.Name, g.MembersJSON, g.ChatID) {
		return VerificationBinding{}, ErrConflict
	}

	binding, bindingExists, err := scanBindingByThread(ctx, tx, n.HostID, n.ThreadID)
	if err != nil {
		return VerificationBinding{}, err
	}
	if bindingExists && (binding.IntentID != intentID || binding.ChatID != g.ChatID) {
		return VerificationBinding{}, ErrConflict
	}
	bindingByChat, bindingByChatExists, err := scanBindingByChat(ctx, tx, n.HostID, g.ChatID)
	if err != nil {
		return VerificationBinding{}, err
	}
	if bindingByChatExists && (bindingByChat.ThreadID != n.ThreadID || bindingByChat.IntentID != intentID) {
		return VerificationBinding{}, ErrConflict
	}
	bindingByIntent, bindingByIntentExists, err := scanBindingByIntent(ctx, tx, intentID)
	if err != nil {
		return VerificationBinding{}, err
	}
	if bindingByIntentExists && (bindingByIntent.HostID != n.HostID || bindingByIntent.ThreadID != n.ThreadID || bindingByIntent.ChatID != g.ChatID) {
		return VerificationBinding{}, ErrConflict
	}

	expires := n.ExpiresAtMS
	if g.ExpiresAtMS < expires {
		expires = g.ExpiresAtMS
	}
	existing, existingErr := scanVerificationBindingByRequestID(ctx, tx, n.RequestID)
	if existingErr == nil {
		if !observationExists || !intentExists || !intentByIDExists || !bindingExists || !bindingByChatExists || !bindingByIntentExists || !sameExistingVerificationBinding(existing, n, g, bot, intentID, expires) {
			return VerificationBinding{}, ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return VerificationBinding{}, err
		}
		return existing, nil
	}
	if !errors.Is(existingErr, sql.ErrNoRows) {
		return VerificationBinding{}, existingErr
	}
	for _, query := range []struct {
		name string
		args []any
	}{
		{"thread", []any{n.HostID, n.ThreadID}},
		{"chat", []any{n.HostID, g.ChatID}},
		{"intent", []any{intentID}},
	} {
		_, err := scanVerificationBindingUnique(ctx, tx, query.name, query.args...)
		if err == nil {
			return VerificationBinding{}, ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return VerificationBinding{}, err
		}
	}

	if !observationExists {
		createdAt := n.CreatedAtMS / 1000
		if _, err := tx.ExecContext(ctx, `INSERT INTO thread_observations(host_id,thread_id,origin,proof,state,cwd,created_at,project_root,exclusion_reason) VALUES(?,?, 'new','birth','excluded',?,?,?,?)`, n.HostID, n.ThreadID, n.CWD, createdAt, n.CWD, "verification_explicit_binding"); err != nil {
			return VerificationBinding{}, err
		}
	} else if observation.State != "excluded" && observation.State != "ignored" {
		if _, err := tx.ExecContext(ctx, `UPDATE thread_observations SET origin='new',proof='birth',state='excluded',project_root=?,exclusion_reason=? WHERE host_id=? AND thread_id=?`, n.CWD, "verification_explicit_binding", n.HostID, n.ThreadID); err != nil {
			return VerificationBinding{}, err
		}
	}
	createdAt := now / 1000
	if createdAt <= 0 {
		createdAt = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO create_intents(intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,created_at) VALUES(?,?,?,?,?,?,'confirmed',?,?) ON CONFLICT(intent_id) DO NOTHING`, intentID, n.HostID, n.ThreadID, n.OwnerID, g.Name, g.MembersJSON, g.ChatID, createdAt); err != nil {
		return VerificationBinding{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO bindings(host_id,thread_id,intent_id,chat_id) VALUES(?,?,?,?) ON CONFLICT(host_id,thread_id) DO NOTHING`, n.HostID, n.ThreadID, intentID, g.ChatID); err != nil {
		return VerificationBinding{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO verification_bindings(request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,bot_id,thread_id,chat_id,cwd,intent_id,bound_at_ms,expires_at_ms,membership_verified_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, n.RequestID, n.HostID, n.AppID, n.OwnerID, n.BridgeCommit, n.BinarySHA256, bot, n.ThreadID, g.ChatID, n.CWD, intentID, now, expires, now); err != nil {
		return VerificationBinding{}, err
	}
	result, err := scanVerificationBindingByRequestID(ctx, tx, n.RequestID)
	if err != nil {
		return VerificationBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return VerificationBinding{}, err
	}
	return result, nil
}

func validateVerificationBindingInputs(n VerificationNative, g VerificationGroup, bot string, now int64) error {
	if !validVerificationBindingID(n.RequestID) || n.RequestID != g.RequestID || n.HostID == "" || n.HostID != g.HostID || n.AppID == "" || n.AppID != g.AppID || n.OwnerID == "" || n.OwnerID != g.OwnerID || n.BridgeCommit == "" || n.BridgeCommit != g.BridgeCommit || !validVerificationBindingHash(n.BinarySHA256) || n.BinarySHA256 != g.BinarySHA256 || bot == "" || bot == n.OwnerID || n.CWD == "" || n.ModelURL == "" || n.RequestJSON == "" || n.ThreadID == "" || g.UUID == "" || g.Name == "" || g.ChatID == "" || g.GroupMessageID == "" || n.State != "confirmed" || n.Generation <= 0 || !validVerificationNativeConnectionID(n.ConnectionID) || g.State != "confirmed" || n.CreatedAtMS <= 0 || g.CreatedAtMS <= 0 || g.MembershipVerifiedAtMS <= 0 || now <= 0 || now < n.CreatedAtMS || now < g.CreatedAtMS || now >= n.ExpiresAtMS || now >= g.ExpiresAtMS || n.ExpiresAtMS <= n.CreatedAtMS || g.ExpiresAtMS <= g.CreatedAtMS || g.MembershipVerifiedAtMS > now {
		return ErrConflict
	}
	var members []string
	if json.Unmarshal([]byte(g.MembersJSON), &members) != nil || len(members) != 1 || members[0] != g.OwnerID {
		return ErrConflict
	}
	return nil
}

func validVerificationBindingID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, b := range []byte(id) {
		if b < '0' || b > '9' && b < 'a' || b > 'f' {
			return false
		}
	}
	return true
}

func validVerificationBindingHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, b := range []byte(hash) {
		if b < '0' || b > '9' && b < 'a' || b > 'f' {
			return false
		}
	}
	return true
}

func sameVerificationNative(a, b VerificationNative) bool {
	return a == b
}

func sameVerificationGroup(a, b VerificationGroup) bool {
	return a == b
}

type bindingObservation struct {
	CWD, State string
}

func scanBindingObservation(ctx context.Context, tx *sql.Tx, host, thread string) (bindingObservation, bool, error) {
	var o bindingObservation
	err := tx.QueryRowContext(ctx, `SELECT cwd,state FROM thread_observations WHERE host_id=? AND thread_id=?`, host, thread).Scan(&o.CWD, &o.State)
	if errors.Is(err, sql.ErrNoRows) {
		return o, false, nil
	}
	return o, err == nil, err
}

type bindingIntent struct {
	ID, HostID, ThreadID, OwnerID, Name, MembersJSON, State, ChatID string
}

func scanBindingIntent(ctx context.Context, tx *sql.Tx, host, thread string) (bindingIntent, bool, error) {
	var i bindingIntent
	err := tx.QueryRowContext(ctx, `SELECT intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id FROM create_intents WHERE host_id=? AND thread_id=?`, host, thread).Scan(&i.ID, &i.HostID, &i.ThreadID, &i.OwnerID, &i.Name, &i.MembersJSON, &i.State, &i.ChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return i, false, nil
	}
	return i, err == nil, err
}

func scanBindingIntentByID(ctx context.Context, tx *sql.Tx, id string) (bindingIntent, bool, error) {
	var i bindingIntent
	err := tx.QueryRowContext(ctx, `SELECT intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id FROM create_intents WHERE intent_id=?`, id).Scan(&i.ID, &i.HostID, &i.ThreadID, &i.OwnerID, &i.Name, &i.MembersJSON, &i.State, &i.ChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return i, false, nil
	}
	return i, err == nil, err
}

func sameBindingIntent(i bindingIntent, id, host, thread, owner, name, members, chat string) bool {
	return i.ID == id && i.HostID == host && i.ThreadID == thread && i.OwnerID == owner && i.Name == name && i.MembersJSON == members && i.State == "confirmed" && i.ChatID == chat
}

type bindingRoute struct {
	HostID, ThreadID, IntentID, ChatID string
}

func scanBindingByThread(ctx context.Context, tx *sql.Tx, host, thread string) (bindingRoute, bool, error) {
	var b bindingRoute
	err := tx.QueryRowContext(ctx, `SELECT host_id,thread_id,intent_id,chat_id FROM bindings WHERE host_id=? AND thread_id=?`, host, thread).Scan(&b.HostID, &b.ThreadID, &b.IntentID, &b.ChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	return b, err == nil, err
}

func scanBindingByChat(ctx context.Context, tx *sql.Tx, host, chat string) (bindingRoute, bool, error) {
	var b bindingRoute
	err := tx.QueryRowContext(ctx, `SELECT host_id,thread_id,intent_id,chat_id FROM bindings WHERE host_id=? AND chat_id=?`, host, chat).Scan(&b.HostID, &b.ThreadID, &b.IntentID, &b.ChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	return b, err == nil, err
}

func scanBindingByIntent(ctx context.Context, tx *sql.Tx, intent string) (bindingRoute, bool, error) {
	var b bindingRoute
	err := tx.QueryRowContext(ctx, `SELECT host_id,thread_id,intent_id,chat_id FROM bindings WHERE intent_id=?`, intent).Scan(&b.HostID, &b.ThreadID, &b.IntentID, &b.ChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	return b, err == nil, err
}

func verificationBindingIntentID(host, app, owner, requestID string) string {
	sum := sha256.Sum256([]byte("verification-binding\x00" + host + "\x00" + app + "\x00" + owner + "\x00" + requestID))
	return hex.EncodeToString(sum[:16])
}

func scanVerificationNativeTx(ctx context.Context, tx *sql.Tx, id string) (VerificationNative, error) {
	var n VerificationNative
	err := tx.QueryRowContext(ctx, `SELECT request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,connection_id,cwd,model_url,request_json,state,thread_id,native_generation,created_at_ms,expires_at_ms FROM verification_native WHERE request_id=?`, id).Scan(&n.RequestID, &n.HostID, &n.AppID, &n.OwnerID, &n.BridgeCommit, &n.BinarySHA256, &n.ConnectionID, &n.CWD, &n.ModelURL, &n.RequestJSON, &n.State, &n.ThreadID, &n.Generation, &n.CreatedAtMS, &n.ExpiresAtMS)
	return n, err
}

func scanVerificationGroupTx(ctx context.Context, tx *sql.Tx, id string) (VerificationGroup, error) {
	var g VerificationGroup
	err := tx.QueryRowContext(ctx, `SELECT request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,request_uuid,name,members_json,state,chat_id,group_message_id,created_at_ms,expires_at_ms,membership_verified_at_ms FROM verification_groups WHERE request_id=?`, id).Scan(&g.RequestID, &g.HostID, &g.AppID, &g.OwnerID, &g.BridgeCommit, &g.BinarySHA256, &g.UUID, &g.Name, &g.MembersJSON, &g.State, &g.ChatID, &g.GroupMessageID, &g.CreatedAtMS, &g.ExpiresAtMS, &g.MembershipVerifiedAtMS)
	return g, err
}

func scanVerificationBinding(ctx context.Context, querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id, host, app, owner string) (VerificationBinding, error) {
	var b VerificationBinding
	err := querier.QueryRowContext(ctx, `SELECT request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,bot_id,thread_id,chat_id,cwd,intent_id,bound_at_ms,expires_at_ms,membership_verified_at_ms FROM verification_bindings WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=?`, id, host, app, owner).Scan(&b.RequestID, &b.HostID, &b.AppID, &b.OwnerID, &b.BridgeCommit, &b.BinarySHA256, &b.BotID, &b.ThreadID, &b.ChatID, &b.CWD, &b.IntentID, &b.BoundAtMS, &b.ExpiresAtMS, &b.MembershipVerifiedAtMS)
	return b, err
}

func scanVerificationBindingByRequestID(ctx context.Context, tx *sql.Tx, id string) (VerificationBinding, error) {
	var b VerificationBinding
	err := tx.QueryRowContext(ctx, `SELECT request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,bot_id,thread_id,chat_id,cwd,intent_id,bound_at_ms,expires_at_ms,membership_verified_at_ms FROM verification_bindings WHERE request_id=?`, id).Scan(&b.RequestID, &b.HostID, &b.AppID, &b.OwnerID, &b.BridgeCommit, &b.BinarySHA256, &b.BotID, &b.ThreadID, &b.ChatID, &b.CWD, &b.IntentID, &b.BoundAtMS, &b.ExpiresAtMS, &b.MembershipVerifiedAtMS)
	return b, err
}

func scanVerificationBindingUnique(ctx context.Context, tx *sql.Tx, name string, args ...any) (VerificationBinding, error) {
	queries := map[string]string{
		"thread": `SELECT request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,bot_id,thread_id,chat_id,cwd,intent_id,bound_at_ms,expires_at_ms,membership_verified_at_ms FROM verification_bindings WHERE host_id=? AND thread_id=?`,
		"chat":   `SELECT request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,bot_id,thread_id,chat_id,cwd,intent_id,bound_at_ms,expires_at_ms,membership_verified_at_ms FROM verification_bindings WHERE host_id=? AND chat_id=?`,
		"intent": `SELECT request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,bot_id,thread_id,chat_id,cwd,intent_id,bound_at_ms,expires_at_ms,membership_verified_at_ms FROM verification_bindings WHERE intent_id=?`,
	}
	var b VerificationBinding
	err := tx.QueryRowContext(ctx, queries[name], args...).Scan(&b.RequestID, &b.HostID, &b.AppID, &b.OwnerID, &b.BridgeCommit, &b.BinarySHA256, &b.BotID, &b.ThreadID, &b.ChatID, &b.CWD, &b.IntentID, &b.BoundAtMS, &b.ExpiresAtMS, &b.MembershipVerifiedAtMS)
	return b, err
}

func sameExistingVerificationBinding(b VerificationBinding, n VerificationNative, g VerificationGroup, bot, intentID string, expires int64) bool {
	return b.RequestID == n.RequestID && b.HostID == n.HostID && b.AppID == n.AppID && b.OwnerID == n.OwnerID && b.BridgeCommit == n.BridgeCommit && b.BinarySHA256 == n.BinarySHA256 && b.BotID == bot && b.ThreadID == n.ThreadID && b.ChatID == g.ChatID && b.CWD == n.CWD && b.IntentID == intentID && b.BoundAtMS > 0 && b.ExpiresAtMS == expires && b.ExpiresAtMS > b.BoundAtMS && b.MembershipVerifiedAtMS == b.BoundAtMS
}
