package store

import (
	"context"
	"encoding/json"
	"strings"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/cot"
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
	chat, err := submissionBinding(ctx, tx, scope)
	if err != nil {
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
		// Interaction requests and settings forms own their UI projections;
		// generic task feedback must not erase their scope or persisted state.
		if strings.HasPrefix(m.Key, "ui:interaction:") || strings.HasPrefix(m.Key, "ui:session-settings:") {
			continue
		}
		if _, nativeCOT, parseErr := cot.Parse(m.Card); parseErr != nil {
			rows.Close()
			return parseErr
		} else if nativeCOT {
			// A transport interruption is not a native task failure. Preserve
			// the CoT journal; never PATCH its post-compatible message as a card.
			continue
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
		var input *Submission
		if strings.HasPrefix(m.Key, "ui:queue:") {
			n, err := readSubmission(ctx, tx, scope, strings.TrimPrefix(m.Key, "ui:queue:"))
			if err != nil {
				return err
			}
			input = &n
		} else if strings.HasPrefix(m.Key, "ui:attachments:") {
			b, err := scanAttachmentBatch(tx.QueryRowContext(ctx, `SELECT `+attachmentBatchColumns+` FROM attachment_batches WHERE batch_id=? AND host_id=? AND app_id=? AND thread_id=? AND chat_id=?`, strings.TrimPrefix(m.Key, "ui:attachments:"), scope.HostID, scope.AppID, scope.ThreadID, chat))
			if err != nil {
				return err
			}
			if b.State != "sealed" {
				continue
			} // Local draft/download actions remain usable offline.
			n, err := readSubmission(ctx, tx, scope, b.ReceiptID)
			if err != nil {
				return err
			}
			if n.State == "collecting" || n.State == "draft" {
				continue
			}
			identity, err := submissionCardIdentityTx(ctx, tx, scope, n)
			if err != nil {
				return err
			}
			if identity.key != m.Key {
				continue
			} // A historical separate queue card still owns native controls.
			if !validSubmissionCard(m, identity) {
				return ErrConflict
			}
			input = &n
		}
		if input != nil {
			localPending, err := localPendingSubmission(ctx, tx, scope, *input)
			if err != nil {
				return err
			}
			if localPending {
				overlay = cards.QueueInputView(cards.QueueView{State: input.State, Token: input.ActionToken, Revision: input.Revision, LocalPending: true})
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
