package store

import (
	"context"
	"encoding/json"
	"strings"

	"codex-feishu/internal/cards"
)

func cardHasCallback(raw string) bool {
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return false
	}
	var contains func(any) bool
	contains = func(value any) bool {
		switch value := value.(type) {
		case map[string]any:
			if value["type"] == "callback" {
				return true
			}
			for _, child := range value {
				if contains(child) {
					return true
				}
			}
		case []any:
			for _, child := range value {
				if contains(child) {
					return true
				}
			}
		}
		return false
	}
	return contains(value)
}

// StageDisconnectedCards disables stale controls after every worker from the
// old native generation has exited. It changes only bridge UI documents, never
// native status, bindings, receipt state, watermarks or ambiguous journals.
func (s *Store) StageDisconnectedCards(ctx context.Context, scope SubmissionScope, status string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := submissionBinding(ctx, tx, scope); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+mirrorColumns+` FROM mirrors m JOIN outbound o USING(host_id,thread_id,mirror_key) WHERE m.host_id=? AND m.thread_id=? AND m.kind='progress' AND m.retired=0`, scope.HostID, scope.ThreadID)
	if err != nil {
		return err
	}
	var selected []Mirror
	for rows.Next() {
		m, err := scanMirror(rows)
		if err != nil {
			rows.Close()
			return err
		}
		if !m.Terminal || cardHasCallback(m.Card) {
			selected = append(selected, m)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	card := cards.Progress(status, "重新连接后恢复同步；当前操作暂不可用。")
	for _, m := range selected {
		overlay := card
		if strings.HasPrefix(m.Key, "ui:queue:") {
			n, err := readSubmission(ctx, tx, scope, strings.TrimPrefix(m.Key, "ui:queue:"))
			if err != nil {
				return err
			}
			localPending, err := localPendingSubmission(ctx, tx, scope, n)
			if err != nil {
				return err
			}
			if localPending {
				overlay = cards.QueueInputView(cards.QueueView{State: n.State, Token: n.ActionToken, Revision: n.Revision, LocalPending: true})
			}
		}
		if m.Card == overlay {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mirrors SET card_json=?,revision=revision+1 WHERE host_id=? AND thread_id=? AND mirror_key=?`, overlay, scope.HostID, scope.ThreadID, m.Key); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbound SET state='ready' WHERE host_id=? AND thread_id=? AND mirror_key=? AND state='confirmed'`, scope.HostID, scope.ThreadID, m.Key); err != nil {
			return err
		}
	}
	return tx.Commit()
}
