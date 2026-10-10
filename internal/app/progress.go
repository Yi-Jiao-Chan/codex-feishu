package app

import (
	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type messageGateway interface {
	SendCard(context.Context, string, string, string, string) (string, error)
	UpdateCard(context.Context, string, string) error
	ReplyCard(context.Context, string, string, string) (string, error)
	AddReaction(context.Context, string, string) (string, error)
	DeleteReaction(context.Context, string, string) error
}
type progressState struct {
	body        string
	updated     time.Time
	unavailable bool
}

// Never treat commentary as a final answer when the native phase is available.
// Legacy phase-less messages use the last public message after the turn ends.
func finalItems(t codexclient.Turn) []codexclient.Item {
	var finals []codexclient.Item
	var legacy *codexclient.Item
	for _, i := range t.Items {
		if i.Type != "agentMessage" || strings.TrimSpace(i.Text) == "" {
			continue
		}
		if i.Phase != nil && *i.Phase == "final_answer" {
			finals = append(finals, i)
		}
		if i.Phase == nil {
			copy := i
			legacy = &copy
		}
	}
	if len(finals) == 0 && legacy != nil {
		finals = append(finals, *legacy)
	}
	return finals
}
func progressBody(t codexclient.Turn) string {
	latest := ""
	running, finished := 0, 0
	for _, i := range t.Items {
		if i.Type == "agentMessage" && (i.Phase == nil || (*i.Phase == "commentary" || *i.Phase == "partial_answer")) && i.Text != "" {
			latest = i.Text
		}
		switch i.Type {
		case "commandExecution", "mcpToolCall", "dynamicToolCall", "fileChange":
			if i.Status == "inProgress" {
				running++
			} else {
				finished++
			}
		}
	}
	if t.Status != "inProgress" {
		latest = ""
	}
	r := []rune(latest)
	if len(r) > 800 {
		latest = string(r[:800]) + "…"
	}
	if running+finished > 0 {
		detail := fmt.Sprintf("工具：%d 项执行中 · %d 项已结束", running, finished)
		if latest != "" {
			latest += "\n\n"
		}
		latest += detail
	}
	return latest
}
func turnLabel(t codexclient.Turn) string {
	switch t.Status {
	case "completed":
		return "已完成"
	case "failed":
		return "执行失败 · 请查看 Codex"
	case "interrupted":
		return "已停止"
	default:
		return "执行中"
	}
}
func uuid(key string) string { sum := sha256.Sum256([]byte(key)); return hex.EncodeToString(sum[:16]) }

// One journalled create per turn; subsequent progress replaces that message.
// Completed history never creates a fresh progress card.
func (m *M0) syncTurn(ctx context.Context, t codexclient.Turn) error {
	parent, anchorErr := m.turnParent(ctx, t)
	if anchorErr != nil {
		return anchorErr
	}
	if parent == "" {
		return nil
	}
	key := "m0.progress." + m.thread + "." + t.ID
	state, id, err := m.db.Operation(ctx, key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if t.Status == "inProgress" || id != "" {
		card := cards.Progress(turnLabel(t), progressBody(t))
		if m.progress == nil {
			m.progress = map[string]progressState{}
		}
		prev := m.progress[t.ID]
		if !prev.unavailable && card != prev.body && (t.Status != "inProgress" || time.Since(prev.updated) >= 2*time.Second) {
			if errors.Is(err, sql.ErrNoRows) && t.Status == "inProgress" {
				fresh, e := m.db.Begin(ctx, key, "feishu.progress")
				if e != nil {
					return e
				}
				if fresh {
					id, e = m.g.ReplyCard(ctx, parent, card, uuid(key))
					if e != nil {
						_ = m.db.Finish(ctx, key, resultState(e), "")
						m.evidence("m0.progress_unavailable", map[string]any{"turn_id": t.ID, "state": resultState(e)})
						prev.unavailable = true
					}
					if !prev.unavailable {
						if e = m.db.Finish(ctx, key, "confirmed", id); e != nil {
							return e
						}
						m.evidence("m0.progress_created", map[string]any{"turn_id": t.ID, "message_id": id, "parent_message_id": parent})
					}
				}
			} else if state == "confirmed" && id != "" {
				if e := m.g.UpdateCard(ctx, id, card); e != nil {
					prev.unavailable = true
					m.evidence("m0.progress_unavailable", map[string]any{"turn_id": t.ID, "state": resultState(e)})
				} else {
					m.evidence("m0.progress_updated", map[string]any{"turn_id": t.ID, "message_id": id, "turn_input_message_id": parent})
				}
			}
			m.progress[t.ID] = progressState{body: card, updated: time.Now(), unavailable: prev.unavailable}
		}
	}
	if t.Status == "inProgress" {
		return nil
	}
	var texts, itemIDs []string
	for _, i := range finalItems(t) {
		// Migrate earlier M0 mirrors without replaying final answers after restart.
		oldState, _, e := m.db.Operation(ctx, fmt.Sprintf("m0.mirror.%s.%s.0", m.thread, i.ID))
		if e == nil && oldState == "confirmed" {
			continue
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		texts = append(texts, i.Text)
		itemIDs = append(itemIDs, i.ID)
	}
	// Merge terminal messages from this turn into one result; split only long results.
	runes := []rune(strings.Join(texts, "\n\n"))
	for start := 0; start < len(runes); start += 3500 {
		end := start + 3500
		if end > len(runes) {
			end = len(runes)
		}
		finalKey := fmt.Sprintf("m0.final.%s.%s.%d", m.thread, t.ID, start)
		fresh, e := m.db.Begin(ctx, finalKey, "feishu.final")
		if e != nil {
			return e
		}
		if !fresh {
			continue
		}
		message, e := m.g.ReplyCard(ctx, parent, cards.Message("AI 回复", "Codex", string(runes[start:end])), uuid(finalKey))
		if e != nil {
			_ = m.db.Finish(ctx, finalKey, resultState(e), "")
			m.evidence("m0.final_unavailable", map[string]any{"turn_id": t.ID, "state": resultState(e)})
			return nil
		}
		if e = m.db.Finish(ctx, finalKey, "confirmed", message); e != nil {
			return e
		}
		m.evidence("m0.final", map[string]any{"turn_id": t.ID, "item_ids": itemIDs, "message_id": message, "parent_message_id": parent})
	}
	return nil
}

// Final delivery confirmation is independent from native turn completion.
func (m *M0) finalDelivered(ctx context.Context, t codexclient.Turn) bool {
	var texts []string
	for _, i := range finalItems(t) {
		state, _, e := m.db.Operation(ctx, fmt.Sprintf("m0.mirror.%s.%s.0", m.thread, i.ID))
		if e == nil && state == "confirmed" {
			continue
		}
		texts = append(texts, i.Text)
	}
	for start := 0; start < len([]rune(strings.Join(texts, "\n\n"))); start += 3500 {
		state, _, e := m.db.Operation(ctx, fmt.Sprintf("m0.final.%s.%s.%d", m.thread, t.ID, start))
		if e != nil || state != "confirmed" {
			return false
		}
	}
	return true
}

// A turn replies to its initiating human input. Steer controls reply to their own
// receipt; they do not move the existing turn's reply anchor.
func (m *M0) turnParent(ctx context.Context, t codexclient.Turn) (string, error) {
	if parent := m.turnParents[t.ID]; parent != "" {
		return parent, nil
	}
	receipts, e := m.db.Receipts(ctx, m.thread)
	if e != nil {
		return "", e
	}
	for _, i := range t.Items {
		if i.Type != "userMessage" {
			continue
		}
		parent := ""
		if i.ClientID != nil {
			for _, r := range receipts {
				if r.ClientID == *i.ClientID {
					parent = r.MessageID
					break
				}
			}
		}
		if parent == "" {
			state, id, e := m.db.Operation(ctx, fmt.Sprintf("m0.mirror.%s.%s.0", m.thread, i.ID))
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return "", e
			}
			if state == "confirmed" {
				parent = id
			}
		}
		if parent != "" {
			if m.turnParents == nil {
				m.turnParents = map[string]string{}
			}
			m.turnParents[t.ID] = parent
		}
		return parent, nil
	}
	return "", nil
}
