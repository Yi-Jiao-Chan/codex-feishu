package store

import (
	"context"
	"errors"
)

const VerificationGroupSchema = `
CREATE TABLE IF NOT EXISTS verification_groups (
 request_id TEXT PRIMARY KEY CHECK(length(request_id)=32),
 host_id TEXT NOT NULL, app_id TEXT NOT NULL, owner_id TEXT NOT NULL,
 bridge_commit TEXT NOT NULL, binary_sha256 TEXT NOT NULL CHECK(length(binary_sha256)=64),
 request_uuid TEXT NOT NULL UNIQUE,
 name TEXT NOT NULL, members_json TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('ready','dispatching','confirmed','unknown','rejected')),
 chat_id TEXT NOT NULL DEFAULT '', group_message_id TEXT NOT NULL DEFAULT '',
 created_at_ms INTEGER NOT NULL, expires_at_ms INTEGER NOT NULL CHECK(expires_at_ms>created_at_ms),
 membership_verified_at_ms INTEGER NOT NULL DEFAULT 0
);
`

// VerificationGroup is a private owner-only managed-group journal. Its fields
// never belong in diagnostics or public JSON; the manager freezes the member
// JSON before making the group API call.
type VerificationGroup struct {
	RequestID, HostID, AppID, OwnerID                   string `json:"-"`
	BridgeCommit, BinarySHA256, UUID, Name, MembersJSON string `json:"-"`
	State, ChatID, GroupMessageID                       string `json:"-"`
	CreatedAtMS, ExpiresAtMS, MembershipVerifiedAtMS    int64  `json:"-"`
}

func (s *Store) PrepareVerificationGroup(ctx context.Context, c VerificationGroup) (VerificationGroup, error) {
	if len(c.RequestID) != 32 || c.HostID == "" || c.AppID == "" || c.OwnerID == "" || c.BridgeCommit == "" || len(c.BinarySHA256) != 64 || c.UUID == "" || c.Name == "" || c.MembersJSON == "" || c.CreatedAtMS <= 0 || c.ExpiresAtMS <= c.CreatedAtMS {
		return VerificationGroup{}, errors.New("invalid verification group journal")
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO verification_groups(request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,request_uuid,name,members_json,state,created_at_ms,expires_at_ms)
 VALUES(?,?,?,?,?,?,?,?,?,'ready',?,?) ON CONFLICT(request_id) DO NOTHING`, c.RequestID, c.HostID, c.AppID, c.OwnerID, c.BridgeCommit, c.BinarySHA256, c.UUID, c.Name, c.MembersJSON, c.CreatedAtMS, c.ExpiresAtMS)
	if err != nil {
		return VerificationGroup{}, err
	}
	return s.VerificationGroup(ctx, c.RequestID, c.HostID, c.AppID, c.OwnerID)
}

func (s *Store) VerificationGroup(ctx context.Context, id, host, app, owner string) (VerificationGroup, error) {
	var c VerificationGroup
	err := s.DB.QueryRowContext(ctx, `SELECT request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,request_uuid,name,members_json,state,chat_id,group_message_id,created_at_ms,expires_at_ms,membership_verified_at_ms
 FROM verification_groups WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=?`, id, host, app, owner).Scan(
		&c.RequestID, &c.HostID, &c.AppID, &c.OwnerID, &c.BridgeCommit, &c.BinarySHA256, &c.UUID, &c.Name, &c.MembersJSON, &c.State, &c.ChatID, &c.GroupMessageID, &c.CreatedAtMS, &c.ExpiresAtMS, &c.MembershipVerifiedAtMS)
	return c, err
}

func (s *Store) ClaimVerificationGroup(ctx context.Context, c VerificationGroup, now int64) (bool, error) {
	r, err := s.DB.ExecContext(ctx, `UPDATE verification_groups SET state='dispatching' WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=? AND bridge_commit=? AND binary_sha256=? AND state='ready' AND expires_at_ms>?`, c.RequestID, c.HostID, c.AppID, c.OwnerID, c.BridgeCommit, c.BinarySHA256, now)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *Store) FinishVerificationGroup(ctx context.Context, c VerificationGroup, state, chat string) error {
	if state != "confirmed" && state != "unknown" && state != "rejected" || state == "confirmed" && chat == "" || state != "confirmed" && chat != "" {
		return errors.New("invalid verification group outcome")
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE verification_groups SET state=?,chat_id=? WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=? AND bridge_commit=? AND binary_sha256=? AND state='dispatching'`, state, chat, c.RequestID, c.HostID, c.AppID, c.OwnerID, c.BridgeCommit, c.BinarySHA256)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("verification group claim changed")
	}
	return nil
}

// ObserveVerificationGroupMembership records the first successful owner+bot
// membership proof for the exact confirmed chat. Replaying that observation
// remains accepted while the journal is live and preserves its first time.
func (s *Store) ObserveVerificationGroupMembership(ctx context.Context, c VerificationGroup, now int64) (bool, error) {
	r, err := s.DB.ExecContext(ctx, `UPDATE verification_groups SET membership_verified_at_ms=CASE WHEN membership_verified_at_ms=0 THEN ? ELSE membership_verified_at_ms END
 WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=? AND bridge_commit=? AND binary_sha256=? AND state='confirmed' AND chat_id=? AND created_at_ms<=? AND expires_at_ms>?`, now, c.RequestID, c.HostID, c.AppID, c.OwnerID, c.BridgeCommit, c.BinarySHA256, c.ChatID, now, now)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

// ObserveVerificationGroupMessage stores the first owner message observed in
// the confirmed group. It intentionally does not require membership proof;
// callers gate a passing incoming result on MembershipVerifiedAtMS separately.
func (s *Store) ObserveVerificationGroupMessage(ctx context.Context, host, app, owner, id, message, chat string, now int64) (bool, error) {
	if id == "" || message == "" || chat == "" {
		return false, nil
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE verification_groups SET group_message_id=CASE WHEN group_message_id='' THEN ? ELSE group_message_id END
 WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=? AND state='confirmed' AND chat_id=? AND created_at_ms<=? AND expires_at_ms>? AND (group_message_id='' OR group_message_id=?)`, message, id, host, app, owner, chat, now, now, message)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
