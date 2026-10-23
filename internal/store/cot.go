package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"codex-feishu/internal/cot"
)

// CoT shares the existing outbox and write lease. Only its acknowledged public
// projection and native identity need a separate ledger; no second sender runs.
const COTSchema = `
CREATE TABLE IF NOT EXISTS cot_deliveries (
 host_id TEXT NOT NULL, thread_id TEXT NOT NULL, mirror_key TEXT NOT NULL,
 cot_id TEXT NOT NULL DEFAULT '', state_json TEXT NOT NULL DEFAULT '{}',
 completed INTEGER NOT NULL DEFAULT 0 CHECK(completed IN (0,1)),
 PRIMARY KEY(host_id,thread_id,mirror_key),
 FOREIGN KEY(host_id,thread_id,mirror_key) REFERENCES mirrors(host_id,thread_id,mirror_key)
);
`

func cotStateHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Native stop arrives as a command, without a card token. Freeze the unique
// locally known running turn at receipt time, so delayed actor dispatch cannot
// interrupt its successor. Ambiguous running turns fail closed; native terminal
// turns cannot block stopping their successor while CoT delivery is unfinished.
func cotStopTarget(ctx context.Context, tx *sql.Tx, scope SubmissionScope) (turn string, hasCOT, unambiguous bool, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.turn_id,m.terminal,COALESCE(d.completed,0),(o.state='rejected' AND m.remote_id='' AND COALESCE(d.cot_id,'')='') FROM mirrors m JOIN outbound o USING(host_id,thread_id,mirror_key) LEFT JOIN cot_deliveries d USING(host_id,thread_id,mirror_key) WHERE m.host_id=? AND m.thread_id=? AND m.kind='progress' AND json_extract(m.card_json,'$.codex_feishu_cot')=1`, scope.HostID, scope.ThreadID)
	if err != nil {
		return "", false, false, err
	}
	defer rows.Close()
	open := 0
	for rows.Next() {
		var id string
		var terminal, completed, neverCreated bool
		if err := rows.Scan(&id, &terminal, &completed, &neverCreated); err != nil {
			return "", false, false, err
		}
		hasCOT = true
		if neverCreated || terminal {
			continue
		}
		if !completed {
			open++
			turn = id
		}
	}
	return turn, hasCOT, open == 1 && turn != "", rows.Err()
}

// prepareCOTAttempt runs after the ordinary binding/parent/interval guards and
// before card-specific mention/marker logic. All wire events and their next ACK
// state are frozen in the same transaction as the dispatch claim.
func prepareCOTAttempt(ctx context.Context, tx *sql.Tx, m Mirror, a *MirrorAttempt) (bool, error) {
	doc, native, err := cot.Parse(m.Card)
	if err != nil || !native {
		return false, errors.New("invalid CoT progress document")
	}
	if m.Kind != "progress" || doc.ThreadID != m.ThreadID || doc.TurnID != m.TurnID {
		return false, ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cot_deliveries(host_id,thread_id,mirror_key) VALUES(?,?,?) ON CONFLICT DO NOTHING`, m.HostID, m.ThreadID, m.Key); err != nil {
		return false, err
	}
	var stateJSON string
	var completed bool
	if err := tx.QueryRowContext(ctx, `SELECT cot_id,state_json,completed FROM cot_deliveries WHERE host_id=? AND thread_id=? AND mirror_key=?`, m.HostID, m.ThreadID, m.Key).Scan(&a.COTID, &stateJSON, &completed); err != nil {
		return false, err
	}
	a.COTStateBefore = cotStateHash(stateJSON)
	a.COTState = stateJSON
	a.COTEvents = "[]"
	switch {
	case completed:
		// Native completed messages are immutable. Final answer corrections are
		// independent ordinary mirrors and remain deliverable.
		if err := acknowledgeCOTRevision(ctx, tx, m.HostID, m.ThreadID, m.Key, m.Revision); err != nil {
			return false, err
		}
		return false, nil
	case a.COTID == "":
		if m.RemoteID != "" {
			return false, ErrConflict
		}
		a.COTStep = "create"
	case m.RemoteID == "":
		return false, ErrConflict
	default:
		var state cot.State
		if json.Unmarshal([]byte(stateJSON), &state) != nil {
			return false, ErrConflict
		}
		if state.Finished {
			a.COTStep = "complete"
			a.COTCaughtUp = true
		} else {
			batch, err := cot.Plan(doc, state, a.CreatedAtMS)
			if err != nil {
				return false, err
			}
			if len(batch.Events) == 0 {
				if !batch.CaughtUp {
					return false, ErrConflict
				}
				if err := acknowledgeCOTRevision(ctx, tx, m.HostID, m.ThreadID, m.Key, m.Revision); err != nil {
					return false, err
				}
				return false, nil
			}
			events, err := json.Marshal(batch.Events)
			if err != nil {
				return false, err
			}
			next, err := json.Marshal(batch.State)
			if err != nil {
				return false, err
			}
			a.COTStep, a.COTEvents, a.COTState, a.COTCaughtUp = "append", string(events), string(next), batch.CaughtUp
		}
	}
	return true, nil
}

func acknowledgeCOTRevision(ctx context.Context, tx *sql.Tx, host, thread, key string, revision int64) error {
	if err := mirrorWriteOne(ctx, tx, `UPDATE mirrors SET delivered_revision=? WHERE host_id=? AND thread_id=? AND mirror_key=?`, revision, host, thread, key); err != nil {
		return err
	}
	return mirrorWriteOne(ctx, tx, `UPDATE outbound SET state='confirmed' WHERE host_id=? AND thread_id=? AND mirror_key=?`, host, thread, key)
}

// finishCOTMirror advances only the exact previously frozen acknowledged state.
// A lost response/commit stays unknown; ordinary post readback cannot prove it.
func finishCOTMirror(ctx context.Context, tx *sql.Tx, a MirrorAttempt, m Mirror, outcome, remoteID, cotID string) (string, error) {
	var actualID, stateJSON string
	var completed bool
	if err := tx.QueryRowContext(ctx, `SELECT cot_id,state_json,completed FROM cot_deliveries WHERE host_id=? AND thread_id=? AND mirror_key=?`, a.HostID, a.ThreadID, a.Key).Scan(&actualID, &stateJSON, &completed); err != nil {
		return "", err
	}
	if a.COTStateBefore != cotStateHash(stateJSON) || completed {
		return "", ErrConflict
	}
	if outcome != "confirmed" {
		return outcome, nil
	}
	state := "ready"
	var next cot.State
	if json.Unmarshal([]byte(a.COTState), &next) != nil {
		return "", ErrConflict
	}
	if a.COTStep == "create" {
		if actualID != "" || a.COTID != "" || cotID == "" || m.RemoteID != "" {
			return "", ErrConflict
		}
		actualID = cotID
	} else if actualID == "" || a.COTID != actualID || remoteID != a.TargetMessageID || remoteID != m.RemoteID || cotID != actualID {
		return "", ErrConflict
	}
	if a.COTStep == "complete" {
		completed = true
	}
	if err := mirrorWriteOne(ctx, tx, `UPDATE cot_deliveries SET cot_id=?,state_json=?,completed=? WHERE host_id=? AND thread_id=? AND mirror_key=?`, actualID, a.COTState, completed, a.HostID, a.ThreadID, a.Key); err != nil {
		return "", err
	}
	if err := mirrorWriteOne(ctx, tx, `UPDATE mirrors SET remote_id=? WHERE host_id=? AND thread_id=? AND mirror_key=?`, remoteID, a.HostID, a.ThreadID, a.Key); err != nil {
		return "", err
	}
	if a.COTStep == "append" && next.Finished && m.Card != a.Card {
		// A snapshot may have arrived while this terminal batch was in flight.
		// Freeze the exact terminal document whose events were acknowledged;
		// Complete must not borrow a newer status/reason or claim unseen text.
		if err := mirrorWriteOne(ctx, tx, `UPDATE mirrors SET card_json=?,terminal=1,revision=revision+1 WHERE host_id=? AND thread_id=? AND mirror_key=?`, a.Card, a.HostID, a.ThreadID, a.Key); err != nil {
			return "", err
		}
	}
	if completed || a.COTStep == "append" && a.COTCaughtUp && !next.Finished {
		if err := mirrorWriteOne(ctx, tx, `UPDATE mirrors SET delivered_revision=? WHERE host_id=? AND thread_id=? AND mirror_key=?`, a.Revision, a.HostID, a.ThreadID, a.Key); err != nil {
			return "", err
		}
		if m.Revision == a.Revision {
			state = "confirmed"
		}
	}
	return state, nil
}
