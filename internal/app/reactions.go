package app

import (
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Emoji types are from Feishu's official enumeration, not display Unicode.
func reactionFor(r store.Receipt, t codexclient.Turn) string {
	switch r.State {
	case "received":
		return "Get"
	case "queued":
		return "THINKING"
	case "failed":
		return "CrossMark"
	case "removed":
		return ""
	case "unknown":
		return ""
	}
	if t.ID != "" {
		switch t.Status {
		case "completed":
			return "CheckMark"
		case "failed":
			return "CrossMark"
		case "interrupted":
			return ""
		}
	}
	return "OnIt"
}

// Optional feedback must not block native execution or cause an input retry.
// Journal ambiguous writes and freeze them; never guess a reaction ID to delete.
func (m *M0) syncReaction(ctx context.Context, r store.Receipt, t codexclient.Turn) {
	desired := reactionFor(r, t)
	if desired == "OnIt" && t.Status == "inProgress" && m.waitingInApp(t.ID) {
		desired = "THINKING"
	}
	if t.Status == "completed" && !m.finalDelivered(ctx, t) {
		desired = ""
	}
	var emoji, id, state string
	var revision int
	err := m.db.DB.QueryRowContext(ctx, "SELECT emoji,reaction_id,revision,state FROM m0_reactions WHERE receipt_id=?", r.ID).Scan(&emoji, &id, &revision, &state)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = m.db.DB.ExecContext(ctx, "INSERT OR IGNORE INTO m0_reactions(receipt_id) VALUES(?)", r.ID)
		state = "confirmed"
	}
	if err != nil || state != "confirmed" || emoji == desired {
		return
	}
	revision++
	// Crash while changing leaves the transition frozen rather than duplicating it.
	_, err = m.db.DB.ExecContext(ctx, "UPDATE m0_reactions SET revision=?,state='changing' WHERE receipt_id=?", revision, r.ID)
	if err != nil {
		return
	}
	key := fmt.Sprintf("m0.reaction.%s.%d", r.ID, revision)
	if id != "" {
		fresh, e := m.db.Begin(ctx, key+".delete", "feishu.reaction.delete")
		if e != nil || !fresh {
			return
		}
		e = m.g.DeleteReaction(ctx, r.MessageID, id)
		if e != nil {
			m.reactionError(ctx, r, key+".delete", e)
			return
		}
		if m.db.Finish(ctx, key+".delete", "confirmed", id) != nil {
			return
		}
	}
	nextID := ""
	if desired != "" {
		fresh, e := m.db.Begin(ctx, key+".add", "feishu.reaction.add")
		if e != nil || !fresh {
			return
		}
		nextID, e = m.g.AddReaction(ctx, r.MessageID, desired)
		if e != nil {
			m.reactionError(ctx, r, key+".add", e)
			return
		}
		if m.db.Finish(ctx, key+".add", "confirmed", nextID) != nil {
			return
		}
	}
	_, err = m.db.DB.ExecContext(ctx, "UPDATE m0_reactions SET emoji=?,reaction_id=?,state='confirmed' WHERE receipt_id=?", desired, nextID, r.ID)
	if err == nil {
		m.evidence("m0.reaction", map[string]any{"message_id": r.MessageID, "emoji": desired, "state": "confirmed"})
	}
}
func (m *M0) reactionError(ctx context.Context, r store.Receipt, key string, err error) {
	state := resultState(err)
	_ = m.db.Finish(ctx, key, state, "")
	_, _ = m.db.DB.ExecContext(ctx, "UPDATE m0_reactions SET state=? WHERE receipt_id=?", state, r.ID)
	m.evidence("m0.reaction_unavailable", map[string]any{"message_id": r.MessageID, "state": state, "detail": err.Error()})
}
