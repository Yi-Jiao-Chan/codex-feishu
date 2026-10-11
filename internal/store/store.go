// Package store journals remote writes before they leave this process.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"time"
)

type Store struct{ DB *sql.DB }

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func Open(path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("database must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// Create restrictively before SQLite opens sidecars (umask also set by CLI).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.DB.Close() }

// Begin returns false for every replay, including requests whose response was lost.
func (s *Store) Begin(ctx context.Context, key, kind string) (bool, error) {
	r, err := s.DB.ExecContext(ctx, "INSERT OR IGNORE INTO m0_operations(key,kind,state,created_at) VALUES(?,?,'sending',?)", key, kind, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
func (s *Store) Finish(ctx context.Context, key, state, id string) error {
	r, err := s.DB.ExecContext(ctx, "UPDATE m0_operations SET state=?,remote_id=? WHERE key=?", state, id, key)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("operation journal missing")
	}
	return nil
}
func (s *Store) Operation(ctx context.Context, key string) (state, id string, err error) {
	err = s.DB.QueryRowContext(ctx, "SELECT state,remote_id FROM m0_operations WHERE key=?", key).Scan(&state, &id)
	return
}
func (s *Store) Bind(ctx context.Context, thread, chat string) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO m0_bindings(thread_id,chat_id) VALUES(?,?) ON CONFLICT(thread_id) DO NOTHING", thread, chat)
	if err != nil {
		return err
	}
	var bound string
	err = s.DB.QueryRowContext(ctx, "SELECT chat_id FROM m0_bindings WHERE thread_id=?", thread).Scan(&bound)
	if err == nil && bound != chat {
		return errors.New("thread already bound elsewhere")
	}
	return err
}

type Receipt struct{ ID, AppID, MessageID, ThreadID, ChatID, Body, ClientID, QueueID, State, CardID, ActionToken string }

func (s *Store) Claim(ctx context.Context, r Receipt) (Receipt, bool, error) {
	r.ID = ID()
	r.ClientID = "cfb-" + r.ID
	r.ActionToken = ID()
	r.State = "received"
	result, err := s.DB.ExecContext(ctx, "INSERT OR IGNORE INTO m0_receipts(id,app_id,message_id,thread_id,chat_id,body,client_id,state,action_token) VALUES(?,?,?,?,?,?,?,?,?)", r.ID, r.AppID, r.MessageID, r.ThreadID, r.ChatID, r.Body, r.ClientID, r.State, r.ActionToken)
	if err != nil {
		return r, false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return r, false, err
	}
	return r, n == 1, nil
}
func (s *Store) SaveReceipt(ctx context.Context, r Receipt) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE m0_receipts SET queue_id=?,state=?,card_id=? WHERE id=?", r.QueueID, r.State, r.CardID, r.ID)
	return err
}
func (s *Store) ByToken(ctx context.Context, token string) (Receipt, error) {
	var r Receipt
	err := s.DB.QueryRowContext(ctx, "SELECT id,app_id,message_id,thread_id,chat_id,body,client_id,queue_id,state,card_id,action_token FROM m0_receipts WHERE action_token=?", token).Scan(&r.ID, &r.AppID, &r.MessageID, &r.ThreadID, &r.ChatID, &r.Body, &r.ClientID, &r.QueueID, &r.State, &r.CardID, &r.ActionToken)
	return r, err
}
func (s *Store) Receipts(ctx context.Context, thread string) ([]Receipt, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id,app_id,message_id,thread_id,chat_id,body,client_id,queue_id,state,card_id,action_token FROM m0_receipts WHERE thread_id=?", thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Receipt
	for rows.Next() {
		var r Receipt
		if err = rows.Scan(&r.ID, &r.AppID, &r.MessageID, &r.ThreadID, &r.ChatID, &r.Body, &r.ClientID, &r.QueueID, &r.State, &r.CardID, &r.ActionToken); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
