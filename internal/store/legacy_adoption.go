package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

// AdoptLegacy commits schema, existing routes, input links and known message
// IDs together. The caller must hold both deployment locks, stop the old bridge,
// save a private backup and obtain fresh complete native/directory/membership
// proof. No remote operation belongs in this transaction.
//
// An exact committed plan is a no-op, even after new inputs have arrived. A
// stale/unverified plan leaves schema 1 and every M0 journal unchanged.
// Seeds must be produced by sync.PlanLegacyThread from the complete native
// snapshot bound to NativeDigest. This persistence primitive validates local
// identity and delivery evidence, not native completeness or public rendering.
// A CLI must regenerate that plan; it must never accept caller-supplied seeds.
func (s *Store) AdoptLegacy(ctx context.Context, p LegacyAdoption) (changed bool, err error) {
	if p.HostID == "" || p.AppID == "" || p.OwnerID == "" || !legacyDigest(p.SourceDigest) || !legacyDigest(p.NativeDigest) || p.StartedAtMS <= 0 {
		return false, ErrConflict
	}
	// SQLite errors may include private values. Keep the causal classification,
	// but never return raw driver text from this private-data transaction.
	defer func() {
		if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrLegacyPending) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("legacy adoption transaction failed; original journals preserved")
		}
	}()
	planID := p.ID()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return false, err
	}
	if version >= 11 && version <= SchemaVersion {
		var host, app string
		err = tx.QueryRowContext(ctx, `SELECT host_id,app_id FROM legacy_adoptions WHERE plan_id=?`, planID).Scan(&host, &app)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (host != p.HostID || app != p.AppID) {
			return false, ErrConflict
		}
		return false, err
	}
	if version != 1 {
		return false, ErrConflict
	}
	i, err := readLegacyInventory(ctx, tx)
	if err != nil {
		return false, err
	}
	if err = i.Quiescent(p.AppID); err != nil {
		return false, err
	}
	if i.Digest() != p.SourceDigest {
		return false, ErrConflict
	}
	if err = validateLegacySeeds(i, p); err != nil {
		return false, err
	}
	if err = migrateSchema(tx); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployment(host_id,app_id,baseline_started_at_ms) VALUES(?,?,?)`, p.HostID, p.AppID, p.StartedAtMS); err != nil {
		return false, err
	}
	for _, t := range p.Threads {
		if _, err = tx.ExecContext(ctx, `INSERT INTO thread_observations(host_id,thread_id,origin,proof,state,cwd,created_at,first_user_item_id,project_root) VALUES(?,?,'baseline','baseline','eligible',?,?,?,?)`, p.HostID, t.ThreadID, t.CWD, t.CreatedAt, t.FirstUserItemID, t.ProjectRoot); err != nil {
			return false, err
		}
		// The intent is already confirmed by the old route; it will never be
		// claimed for CreateGroup. A fresh baseline still ignores other history.
		h := sha256.Sum256([]byte(planID + "\x00" + t.ThreadID))
		intentID := hex.EncodeToString(h[:16])
		if _, err = tx.ExecContext(ctx, `INSERT INTO create_intents(intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,created_at) VALUES(?,?,?,?,'M0 adopted binding','[]','confirmed',?,?)`, intentID, p.HostID, t.ThreadID, p.OwnerID, t.ChatID, p.StartedAtMS/1000); err != nil {
			return false, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO bindings(host_id,thread_id,intent_id,chat_id) VALUES(?,?,?,?)`, p.HostID, t.ThreadID, intentID, t.ChatID); err != nil {
			return false, err
		}
		for _, m := range t.Mirrors {
			if _, err = tx.ExecContext(ctx, `INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,revision,delivered_revision,terminal,retired,replay,parent_key,parent_message_id,remote_id,event_type,event_at) VALUES(?,?,?,?,?,?,1,0,1,?,1,?,?,?,?,?)`, p.HostID, t.ThreadID, m.Key, m.TurnID, m.Kind, m.Card, m.Retired, m.ParentKey, m.ParentMessageID, m.RemoteID, m.EventType, m.EventAt); err != nil {
				return false, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,?,'ready')`, p.HostID, t.ThreadID, m.Key); err != nil {
				return false, err
			}
		}
		for _, link := range t.Links {
			if err = stageInputLink(ctx, tx, p.HostID, t.ThreadID, link); err != nil {
				return false, err
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO submission_inputs(receipt_id,host_id,app_id,thread_id,state,queue_id,turn_id,item_id)
 SELECT r.id,?,?,?,r.state,r.queue_id,COALESCE(l.turn_id,''),COALESCE(l.item_id,'') FROM m0_receipts r
 LEFT JOIN input_history_links l ON l.receipt_id=r.id
 WHERE r.app_id=? AND r.thread_id=? AND r.chat_id=?`, p.HostID, p.AppID, t.ThreadID, p.AppID, t.ThreadID, t.ChatID); err != nil {
			return false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO legacy_adoptions(plan_id,host_id,app_id,source_digest,native_digest,adopted_at_ms) VALUES(?,?,?,?,?,?)`, planID, p.HostID, p.AppID, p.SourceDigest, p.NativeDigest, p.StartedAtMS); err != nil {
		return false, err
	}
	err = tx.Commit()
	return err == nil, err
}

func legacyDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && strings.ToLower(s) == s
}

func validateLegacySeeds(i LegacyInventory, p LegacyAdoption) error {
	if len(i.Bindings) == 0 || len(p.Threads) != len(i.Bindings) {
		return ErrConflict
	}
	bound := map[string]string{}
	receipts := map[string]Receipt{}
	for _, b := range i.Bindings {
		bound[b.ThreadID] = b.ChatID
	}
	for _, r := range i.Receipts {
		receipts[r.ID] = r
	}
	retiredIDs := map[string]bool{}
	for _, o := range i.Operations {
		if o.State == "retired" && o.RemoteID != "" {
			retiredIDs[o.RemoteID] = true
		}
	}
	seenThreads, remoteIDs, linked := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, t := range p.Threads {
		if t.ThreadID == "" || t.ChatID == "" || bound[t.ThreadID] != t.ChatID || seenThreads[t.ThreadID] || len(t.Mirrors) == 0 || t.FirstUserItemID == "" || t.CreatedAt <= 0 || t.CreatedAt > p.StartedAtMS/1000 || !filepath.IsAbs(t.CWD) || filepath.Clean(t.CWD) != t.CWD || !filepath.IsAbs(t.ProjectRoot) || filepath.Clean(t.ProjectRoot) != t.ProjectRoot {
			return ErrConflict
		}
		seenThreads[t.ThreadID] = true
		known := map[string]bool{}
		for _, o := range i.Operations {
			if o.State == "confirmed" && o.RemoteID != "" && (strings.HasPrefix(o.Key, "m0.mirror."+t.ThreadID+".") && o.Kind == "feishu.mirror" || strings.HasPrefix(o.Key, "m0.progress."+t.ThreadID+".") && o.Kind == "feishu.progress" || strings.HasPrefix(o.Key, "m0.final."+t.ThreadID+".") && o.Kind == "feishu.final") {
				known[o.RemoteID] = true
			}
			if o.State == "confirmed" && o.Kind == "feishu.send_card" {
				for _, r := range i.Receipts {
					if r.ThreadID == t.ThreadID && o.Key == "m0.receipt-card."+r.ID && o.RemoteID != "" && r.CardID == o.RemoteID {
						known[o.RemoteID] = true
					}
				}
			}
		}
		keys := map[string]Mirror{}
		parents := map[string]bool{}
		for _, r := range i.Receipts {
			if r.ThreadID == t.ThreadID {
				parents[r.MessageID] = true
			}
		}
		for _, m := range t.Mirrors {
			if m.Key == "" || m.TurnID == "" || !json.Valid([]byte(m.Card)) || m.RemoteID == "" || !known[m.RemoteID] || retiredIDs[m.RemoteID] || remoteIDs[m.RemoteID] || keys[m.Key].Key != "" || m.HostID != "" && m.HostID != p.HostID || m.ThreadID != "" && m.ThreadID != t.ThreadID || m.Revision != 1 || m.DeliveredRevision != 0 || m.State != "ready" || !m.Terminal || !m.Replay {
				return ErrConflict
			}
			if m.EventAt < 0 || m.EventAt > p.StartedAtMS/1000 || m.EventType == "" && m.EventAt != 0 || m.EventType != "" && (m.Kind != "final" || m.EventType != "completed" && m.EventType != "failed") {
				return ErrConflict
			}
			switch m.Kind {
			case "human":
				if m.ParentKey != "" || m.ParentMessageID != "" || !legacyMirrorKey(m.Key, "human") {
					return ErrConflict
				}
			case "progress", "final":
				if (m.ParentKey == "") == (m.ParentMessageID == "") || m.ParentKey == m.Key || m.ParentMessageID != "" && !parents[m.ParentMessageID] || !legacyMirrorKey(m.Key, m.Kind) && !strings.HasPrefix(m.Key, "ui:queue:") {
					return ErrConflict
				}
			default:
				return ErrConflict
			}
			if strings.HasPrefix(m.Key, "ui:queue:") {
				r, ok := receipts[strings.TrimPrefix(m.Key, "ui:queue:")]
				if !ok || r.ThreadID != t.ThreadID || m.Kind != "progress" || m.TurnID != m.Key || m.ParentMessageID != r.MessageID || r.CardID != "" && r.CardID != m.RemoteID {
					return ErrConflict
				}
			}
			keys[m.Key], remoteIDs[m.RemoteID] = m, true
		}
		for _, m := range t.Mirrors {
			if m.ParentKey != "" {
				parent := keys[m.ParentKey]
				if parent.Kind != "human" || parent.TurnID != m.TurnID || parent.Retired {
					return ErrConflict
				}
			}
		}
		items := map[string]bool{}
		for _, l := range t.Links {
			r, ok := receipts[l.ReceiptID]
			if !ok || r.ThreadID != t.ThreadID || l.TurnID == "" || l.ItemID == "" || linked[l.ReceiptID] || items[l.TurnID+"\x00"+l.ItemID] || r.State != "consumed" && r.State != "steered" {
				return ErrConflict
			}
			linked[l.ReceiptID], items[l.TurnID+"\x00"+l.ItemID] = true, true
		}
	}
	for _, r := range i.Receipts {
		if (r.State == "consumed" || r.State == "steered") && !linked[r.ID] {
			return ErrLegacyPending
		}
	}
	return nil
}

func legacyMirrorKey(key, kind string) bool {
	prefix := kind + ":"
	return strings.HasPrefix(key, prefix) && legacyDigest(strings.TrimPrefix(key, prefix))
}
