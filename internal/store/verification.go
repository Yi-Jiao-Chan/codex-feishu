package store

import (
	"context"
	"errors"
)

const VerificationSchema = `
CREATE TABLE IF NOT EXISTS verification_cards (
 request_id TEXT PRIMARY KEY CHECK(length(request_id)=32),
 host_id TEXT NOT NULL, app_id TEXT NOT NULL, owner_id TEXT NOT NULL,
 bridge_commit TEXT NOT NULL, binary_sha256 TEXT NOT NULL CHECK(length(binary_sha256)=64),
 token TEXT NOT NULL UNIQUE CHECK(length(token)=32),
 request_uuid TEXT NOT NULL UNIQUE, card TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('ready','dispatching','confirmed','unknown','rejected')),
 message_id TEXT NOT NULL DEFAULT '',
 callback_message_id TEXT NOT NULL DEFAULT '', callback_chat_id TEXT NOT NULL DEFAULT '',
 private_message_id TEXT NOT NULL DEFAULT '',
 created_at_ms INTEGER NOT NULL, expires_at_ms INTEGER NOT NULL CHECK(expires_at_ms>created_at_ms)
);
`

// VerificationCard is a private journal record, never a diagnostic payload.
// Probe tokens and frozen wire bodies stay out of normal JSON and logs.
type VerificationCard struct {
	RequestID, HostID, AppID, OwnerID, BridgeCommit     string `json:"-"`
	BinarySHA256                                        string `json:"-"`
	Token, UUID, Card, State, MessageID                 string `json:"-"`
	CallbackMessageID, CallbackChatID, PrivateMessageID string `json:"-"`
	CreatedAtMS, ExpiresAtMS                            int64  `json:"-"`
}

func (s *Store) PrepareVerificationCard(ctx context.Context, c VerificationCard) (VerificationCard, error) {
	if len(c.RequestID) != 32 || c.HostID == "" || c.AppID == "" || c.OwnerID == "" || c.BridgeCommit == "" || len(c.BinarySHA256) != 64 || len(c.Token) != 32 || c.UUID == "" || c.Card == "" || c.CreatedAtMS <= 0 || c.ExpiresAtMS <= c.CreatedAtMS {
		return VerificationCard{}, errors.New("invalid verification journal")
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO verification_cards(request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,token,request_uuid,card,state,created_at_ms,expires_at_ms)
 VALUES(?,?,?,?,?,?,?,?,?,'ready',?,?) ON CONFLICT(request_id) DO NOTHING`, c.RequestID, c.HostID, c.AppID, c.OwnerID, c.BridgeCommit, c.BinarySHA256, c.Token, c.UUID, c.Card, c.CreatedAtMS, c.ExpiresAtMS)
	if err != nil {
		return VerificationCard{}, err
	}
	return s.VerificationCard(ctx, c.RequestID, c.HostID, c.AppID, c.OwnerID)
}

func (s *Store) VerificationCard(ctx context.Context, id, host, app, owner string) (VerificationCard, error) {
	var c VerificationCard
	err := s.DB.QueryRowContext(ctx, `SELECT request_id,host_id,app_id,owner_id,bridge_commit,binary_sha256,token,request_uuid,card,state,message_id,callback_message_id,callback_chat_id,private_message_id,created_at_ms,expires_at_ms FROM verification_cards WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=?`, id, host, app, owner).Scan(&c.RequestID, &c.HostID, &c.AppID, &c.OwnerID, &c.BridgeCommit, &c.BinarySHA256, &c.Token, &c.UUID, &c.Card, &c.State, &c.MessageID, &c.CallbackMessageID, &c.CallbackChatID, &c.PrivateMessageID, &c.CreatedAtMS, &c.ExpiresAtMS)
	return c, err
}

func (s *Store) ClaimVerificationCard(ctx context.Context, c VerificationCard, now int64) (bool, error) {
	r, err := s.DB.ExecContext(ctx, `UPDATE verification_cards SET state='dispatching' WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=? AND bridge_commit=? AND binary_sha256=? AND state='ready' AND expires_at_ms>?`, c.RequestID, c.HostID, c.AppID, c.OwnerID, c.BridgeCommit, c.BinarySHA256, now)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *Store) FinishVerificationCard(ctx context.Context, c VerificationCard, state, id string) error {
	if state != "confirmed" && state != "unknown" && state != "rejected" || state == "confirmed" && id == "" || state != "confirmed" && id != "" {
		return errors.New("invalid verification outcome")
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE verification_cards SET state=?,message_id=? WHERE request_id=? AND host_id=? AND app_id=? AND owner_id=? AND bridge_commit=? AND binary_sha256=? AND state='dispatching'`, state, id, c.RequestID, c.HostID, c.AppID, c.OwnerID, c.BridgeCommit, c.BinarySHA256)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("verification claim changed")
	}
	return nil
}

// A callback can race the send response. Preserve its exact message identity;
// the manager only reports it proven once it equals the confirmed send ID.
func (s *Store) ObserveVerificationCallback(ctx context.Context, host, app, owner, token, message, chat string, now int64) (bool, error) {
	if message == "" || chat == "" {
		return false, nil
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE verification_cards SET callback_message_id=?,callback_chat_id=? WHERE host_id=? AND app_id=? AND owner_id=? AND token=? AND state IN ('dispatching','unknown','confirmed') AND expires_at_ms>? AND (message_id='' OR message_id=?) AND (callback_message_id='' OR (callback_message_id=? AND callback_chat_id=?))`, message, chat, host, app, owner, token, now, message, message, chat)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *Store) ObserveVerificationPrivateMessage(ctx context.Context, host, app, owner, token, message string, now int64) (bool, error) {
	if message == "" {
		return false, nil
	}
	r, err := s.DB.ExecContext(ctx, `UPDATE verification_cards SET private_message_id=? WHERE host_id=? AND app_id=? AND owner_id=? AND token=? AND state IN ('dispatching','unknown','confirmed') AND expires_at_ms>? AND (private_message_id='' OR private_message_id=?)`, message, host, app, owner, token, now, message)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
