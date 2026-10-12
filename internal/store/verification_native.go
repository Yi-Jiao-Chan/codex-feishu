package store

import (
	"context"
	"errors"
	"path/filepath"
)

const VerificationNativeSchema = `
CREATE TABLE IF NOT EXISTS verification_native (
 request_id TEXT PRIMARY KEY CHECK(length(request_id)=32),
 host_id TEXT NOT NULL, app_id TEXT NOT NULL, owner_id TEXT NOT NULL,
 bridge_commit TEXT NOT NULL, binary_sha256 TEXT NOT NULL CHECK(length(binary_sha256)=64),
 connection_id TEXT NOT NULL DEFAULT '',
 cwd TEXT NOT NULL, model_url TEXT NOT NULL, request_json TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('ready','dispatching','confirmed','unknown','rejected')),
 thread_id TEXT NOT NULL DEFAULT '',
 native_generation INTEGER NOT NULL DEFAULT 0 CHECK(native_generation>=0),
 created_at_ms INTEGER NOT NULL, expires_at_ms INTEGER NOT NULL CHECK(expires_at_ms>created_at_ms)
);
`

// VerificationNative is a private journal record for an explicitly authorized
// local native-thread verification. Its frozen request is never a diagnostic
// payload or a remote deduplication identifier.
type VerificationNative struct {
	RequestID, HostID, AppID, OwnerID, BridgeCommit, BinarySHA256 string `json:"-"`
	ConnectionID                                                  string `json:"-"`
	CWD, ModelURL, RequestJSON, State, ThreadID                   string `json:"-"`
	CreatedAtMS, ExpiresAtMS, Generation                          int64  `json:"-"`
}

func validVerificationNative(c VerificationNative) bool {
	return len(c.RequestID) == 32 && c.HostID != "" && c.AppID != "" && c.OwnerID != "" && c.BridgeCommit != "" && len(c.BinarySHA256) == 64 && filepath.IsAbs(c.CWD) && filepath.Clean(c.CWD) == c.CWD && c.ModelURL != "" && c.RequestJSON != "" && c.CreatedAtMS > 0 && c.ExpiresAtMS > c.CreatedAtMS
}

func (s *Store) PrepareVerificationNative(ctx context.Context, c VerificationNative) (VerificationNative, error) {
	if !validVerificationNative(c) {
		return VerificationNative{}, errors.New("invalid verification native journal")
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO verification_native(request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,cwd,model_url,request_json,state,native_generation,created_at_ms,expires_at_ms)
 VALUES(?,?,?,?,?,?,?,?,?,'ready',0,?,?) ON CONFLICT(request_id) DO NOTHING`, c.RequestID, c.HostID, c.AppID, c.OwnerID, c.BridgeCommit, c.BinarySHA256, c.CWD, c.ModelURL, c.RequestJSON, c.CreatedAtMS, c.ExpiresAtMS)
	if err != nil {
		return VerificationNative{}, err
	}
	return s.VerificationNative(ctx, c.RequestID, c.HostID, c.AppID, c.OwnerID)
}

func (s *Store) VerificationNative(ctx context.Context, id, host, app, owner string) (VerificationNative, error) {
	var c VerificationNative
	err := s.DB.QueryRowContext(ctx, `SELECT request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,connection_id,cwd,model_url,request_json,state,thread_id,native_generation,created_at_ms,expires_at_ms
 FROM verification_native WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=?`, id, host, app, owner).Scan(
		&c.RequestID, &c.HostID, &c.AppID, &c.OwnerID, &c.BridgeCommit, &c.BinarySHA256, &c.ConnectionID, &c.CWD, &c.ModelURL, &c.RequestJSON, &c.State, &c.ThreadID, &c.Generation, &c.CreatedAtMS, &c.ExpiresAtMS)
	return c, err
}

func (s *Store) ClaimVerificationNative(ctx context.Context, c VerificationNative, generation int64, connectionID string, now int64) (bool, error) {
	if generation <= 0 || !validVerificationNativeConnectionID(connectionID) {
		return false, nil
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE verification_native SET state='dispatching',native_generation=?,connection_id=?
 WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=? AND bridge_commit=? AND binary_sha256=? AND cwd=? AND model_url=? AND request_json=? AND state='ready' AND native_generation=0 AND connection_id='' AND created_at_ms<=? AND expires_at_ms>?`, generation, connectionID, c.RequestID, c.HostID, c.AppID, c.OwnerID, c.BridgeCommit, c.BinarySHA256, c.CWD, c.ModelURL, c.RequestJSON, now, now)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *Store) FinishVerificationNative(ctx context.Context, c VerificationNative, state, thread string) error {
	if c.Generation <= 0 {
		return errors.New("invalid verification native generation")
	}
	if state != "confirmed" && state != "unknown" && state != "rejected" || state == "confirmed" && thread == "" || state != "confirmed" && thread != "" {
		return errors.New("invalid verification native outcome")
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE verification_native SET state=?,thread_id=?
 WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=? AND bridge_commit=? AND binary_sha256=? AND connection_id=? AND native_generation=? AND state='dispatching'`, state, thread, c.RequestID, c.HostID, c.AppID, c.OwnerID, c.BridgeCommit, c.BinarySHA256, c.ConnectionID, c.Generation)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("verification native claim changed")
	}
	return nil
}

func validVerificationNativeConnectionID(id string) bool {
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
