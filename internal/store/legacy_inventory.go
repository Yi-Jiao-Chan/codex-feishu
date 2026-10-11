package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

var ErrLegacyAdoptionRequired = errors.New("legacy M0 database requires reviewed migration and binding adoption")
var ErrLegacyPending = errors.New("legacy M0 has unfinished or unverified operations; preserve its journals")

type LegacyOperation struct {
	Key, Kind, State, RemoteID string
	CreatedAt                  int64
}
type LegacyAction struct{ ReceiptID, Kind, State string }
type LegacyReaction struct {
	ReceiptID, Emoji, ReactionID, State string
	Revision                            int64
}

// Raw records contain private conversation data. JSON CLI output must use only
// aggregate summaries; the raw inventory is never serialized as diagnostics.
type LegacyInventory struct {
	Schema     int               `json:"-"`
	Bindings   []Binding         `json:"-"`
	Receipts   []Receipt         `json:"-"`
	Operations []LegacyOperation `json:"-"`
	Actions    []LegacyAction    `json:"-"`
	Reactions  []LegacyReaction  `json:"-"`
}

// Digest includes private receipt bodies without exposing them. Row order is
// deterministic; the read transaction gives every table one consistent view.
func (i LegacyInventory) Digest() string {
	b, _ := json.Marshal([]any{i.Schema, i.Bindings, i.Receipts, i.Operations, i.Actions, i.Reactions})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *Store) LegacyInventory(ctx context.Context) (LegacyInventory, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return LegacyInventory{}, errors.New("legacy inventory unavailable")
	}
	defer tx.Rollback()
	i, err := readLegacyInventory(ctx, tx)
	if err != nil {
		return i, errors.New("legacy inventory unavailable")
	}
	if err = tx.Commit(); err != nil {
		return LegacyInventory{}, errors.New("legacy inventory unavailable")
	}
	return i, nil
}

func readLegacyInventory(ctx context.Context, tx *sql.Tx) (i LegacyInventory, err error) {
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&i.Schema); err != nil {
		return
	}
	rows, err := tx.QueryContext(ctx, "SELECT thread_id,chat_id FROM m0_bindings ORDER BY thread_id")
	if err != nil {
		return i, err
	}
	for rows.Next() {
		var v Binding
		if err = rows.Scan(&v.ThreadID, &v.ChatID); err != nil {
			rows.Close()
			return
		}
		i.Bindings = append(i.Bindings, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	rows, err = tx.QueryContext(ctx, "SELECT id,app_id,message_id,thread_id,chat_id,body,client_id,queue_id,state,card_id,action_token FROM m0_receipts ORDER BY id")
	if err != nil {
		return
	}
	for rows.Next() {
		var v Receipt
		if err = rows.Scan(&v.ID, &v.AppID, &v.MessageID, &v.ThreadID, &v.ChatID, &v.Body, &v.ClientID, &v.QueueID, &v.State, &v.CardID, &v.ActionToken); err != nil {
			rows.Close()
			return
		}
		i.Receipts = append(i.Receipts, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	rows, err = tx.QueryContext(ctx, "SELECT key,kind,state,remote_id,created_at FROM m0_operations ORDER BY key")
	if err != nil {
		return
	}
	for rows.Next() {
		var v LegacyOperation
		if err = rows.Scan(&v.Key, &v.Kind, &v.State, &v.RemoteID, &v.CreatedAt); err != nil {
			rows.Close()
			return
		}
		i.Operations = append(i.Operations, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	rows, err = tx.QueryContext(ctx, "SELECT receipt_id,kind,state FROM m0_actions ORDER BY receipt_id")
	if err != nil {
		return
	}
	for rows.Next() {
		var v LegacyAction
		if err = rows.Scan(&v.ReceiptID, &v.Kind, &v.State); err != nil {
			rows.Close()
			return
		}
		i.Actions = append(i.Actions, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	rows, err = tx.QueryContext(ctx, "SELECT receipt_id,emoji,reaction_id,revision,state FROM m0_reactions ORDER BY receipt_id")
	if err != nil {
		return
	}
	for rows.Next() {
		var v LegacyReaction
		if err = rows.Scan(&v.ReceiptID, &v.Emoji, &v.ReactionID, &v.Revision, &v.State); err != nil {
			rows.Close()
			return
		}
		i.Reactions = append(i.Reactions, v)
	}
	err = rows.Err()
	rows.Close()
	return
}

// Quiescent rejects uncertain activity, including done actions whose underlying
// remote operation is still unknown. It never changes old state to manufacture
// migration readiness. Native turn/queue quiescence is a separate caller proof.
func (i LegacyInventory) Quiescent(app string) error {
	if i.Schema != 1 || app == "" {
		return ErrConflict
	}
	for _, r := range i.Receipts {
		if r.AppID != app || r.ID == "" || r.ClientID == "" || r.ActionToken == "" || r.MessageID == "" {
			return ErrConflict
		}
		switch r.State {
		case "consumed", "steered", "removed", "failed":
		default:
			return ErrLegacyPending
		}
	}
	bindings := make(map[string]string, len(i.Bindings))
	chats := make(map[string]bool, len(i.Bindings))
	for _, b := range i.Bindings {
		if b.ThreadID == "" || b.ChatID == "" || bindings[b.ThreadID] != "" || chats[b.ChatID] {
			return ErrConflict
		}
		bindings[b.ThreadID], chats[b.ChatID] = b.ChatID, true
	}
	receipts := make(map[string]bool, len(i.Receipts))
	clients, messages, tokens := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range i.Receipts {
		if r.ThreadID == "" || r.ChatID == "" || bindings[r.ThreadID] != r.ChatID || receipts[r.ID] || clients[r.ClientID] || messages[r.MessageID] || tokens[r.ActionToken] {
			return ErrConflict
		}
		receipts[r.ID], clients[r.ClientID], messages[r.MessageID], tokens[r.ActionToken] = true, true, true, true
	}
	operations := make(map[string]bool, len(i.Operations))
	for _, o := range i.Operations {
		if o.Key == "" || o.Kind == "" || operations[o.Key] {
			return ErrConflict
		}
		operations[o.Key] = true
		switch o.State {
		case "confirmed", "retired", "rejected":
		default:
			return ErrLegacyPending
		}
	}
	actions := map[string]bool{}
	for _, a := range i.Actions {
		if !receipts[a.ReceiptID] || actions[a.ReceiptID] || a.Kind != "steer" && a.Kind != "remove" {
			return ErrConflict
		}
		actions[a.ReceiptID] = true
		switch a.State {
		case "done", "failed":
		default:
			return ErrLegacyPending
		}
	}
	reactions := map[string]bool{}
	for _, r := range i.Reactions {
		if !receipts[r.ReceiptID] || reactions[r.ReceiptID] || r.Revision < 0 || r.State == "confirmed" && (r.Emoji == "") != (r.ReactionID == "") {
			return ErrConflict
		}
		reactions[r.ReceiptID] = true
		switch r.State {
		case "confirmed", "rejected":
		default:
			return ErrLegacyPending
		}
	}
	return nil
}

// LegacyThreadSeed is produced from complete, quiescent native history after
// directory/origin/membership proof. Only known old message IDs are activated.
// Each imported document is ready for PATCH, never a new send/reply.
type LegacyThreadSeed struct {
	ThreadID, ChatID, CWD, ProjectRoot, FirstUserItemID string
	CreatedAt                                           int64
	Mirrors                                             []Mirror
	Links                                               []InputLink
}

type LegacyAdoption struct {
	HostID, AppID, OwnerID, SourceDigest, NativeDigest string             `json:"-"`
	StartedAtMS                                        int64              `json:"-"`
	Threads                                            []LegacyThreadSeed `json:"-"`
}

func (p LegacyAdoption) ID() string {
	b, _ := json.Marshal([]any{p.HostID, p.AppID, p.OwnerID, p.SourceDigest, p.NativeDigest, p.StartedAtMS, p.Threads})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
