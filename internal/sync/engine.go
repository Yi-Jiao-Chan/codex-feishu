package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type NativeHistory interface {
	Turns(context.Context, string) ([]codexclient.Turn, error)
}

// Engine converts complete public history into durable deliveries. A per-thread
// actor serializes Reconcile calls; events only trigger a new full reconciliation
// and never publish token deltas directly. No remote writes happen here.
type Engine struct {
	Store          *store.Store
	Native         NativeHistory
	HostID         string
	MaxCardBytes   int
	MentionTargets []string // Reserve the complete structured reminder before splitting final text.
	// ProgressRender is pure; the actor prepares its durable control state first.
	// An empty interrupted render suppresses a new historical progress card.
	ProgressRender func(thread, turn, nativeStatus, body string) string
}

// HistoryError keeps native payloads out of ordinary error text.
type HistoryError struct{ Cause error }

func (*HistoryError) Error() string   { return "message sync awaits complete native history" }
func (e *HistoryError) Unwrap() error { return e.Cause }

func (e Engine) Reconcile(ctx context.Context, thread string, replay bool, observedAtMS int64) error {
	if e.Native == nil {
		return errors.New("sync requires native history")
	}
	turns, err := e.Native.Turns(ctx, thread)
	if err != nil {
		return &HistoryError{Cause: err}
	}
	return e.Apply(ctx, thread, turns, replay, observedAtMS)
}

// Apply accepts only a complete, hydrated snapshot in native history order.
// Receipt links, all public documents and the scan checkpoint commit together.
func (e Engine) Apply(ctx context.Context, thread string, turns []codexclient.Turn, replay bool, observedAtMS int64) error {
	if e.Store == nil || e.HostID == "" || thread == "" {
		return errors.New("sync requires a store and thread identity")
	}
	receipts, err := e.Store.ReceiptsForDeployment(ctx, e.HostID, thread)
	if err != nil {
		return err
	}
	old, err := e.Store.Mirrors(ctx, e.HostID, thread)
	if err != nil {
		return err
	}
	plan, err := e.renderSnapshot(thread, turns, receipts, old, replay, observedAtMS)
	if err != nil {
		return err
	}
	return e.Store.StageSnapshot(ctx, e.HostID, thread, plan.Docs, plan.Links, plan.Mark)
}

type snapshotPlan struct {
	Docs  []store.Mirror
	Links []store.InputLink
	Mark  store.Watermark
}

// renderSnapshot shares the live renderer with reviewed legacy adoption. It does
// no database or native work and preserves the caller's existing reply anchors.
func (e Engine) renderSnapshot(thread string, turns []codexclient.Turn, receipts []store.Receipt, old []store.Mirror, replay bool, observedAtMS int64) (snapshotPlan, error) {
	byClient := make(map[string]store.Receipt, len(receipts))
	for _, r := range receipts {
		byClient[r.ClientID] = r
	}
	existing := make(map[string]store.Mirror, len(old))
	byTurn := make(map[string][]store.Mirror)
	for _, m := range old {
		existing[m.Key] = m
		byTurn[m.TurnID] = append(byTurn[m.TurnID], m)
	}
	mark := store.Watermark{ObservedAtMS: observedAtMS}
	var docs []store.Mirror
	var links []store.InputLink
	seenTurns := make(map[string]bool, len(turns))
	for _, turn := range turns {
		if turn.ID == "" || seenTurns[turn.ID] || turn.ItemsView != "" && turn.ItemsView != "full" {
			return snapshotPlan{}, errors.New("sync requires unique full turns")
		}
		seenTurns[turn.ID] = true
		switch turn.Status {
		case "inProgress", "completed", "failed", "interrupted":
		default:
			return snapshotPlan{}, errors.New("unsupported native turn status")
		}
		items, err := uniqueItems(turn.Items)
		if err != nil {
			return snapshotPlan{}, err
		}
		turn.Items = items
		mark.TurnID = turn.ID
		var parentKey, parentID string
		firstInput := true
		active := map[string]bool{}
		for _, i := range turn.Items {
			mark.ItemID = i.ID
			if i.Type != "userMessage" {
				continue
			}
			if i.ClientID != nil {
				if r, ok := byClient[*i.ClientID]; ok {
					links = append(links, store.InputLink{ReceiptID: r.ID, TurnID: turn.ID, ItemID: i.ID})
					if firstInput {
						parentID, firstInput = r.MessageID, false
					}
					continue // Feishu's original message is already human context.
				}
			}
			text := i.PublicText()
			if strings.TrimSpace(text) == "" {
				text = "此条输入没有可同步的文本，请在 Codex 中查看。"
			}
			parts, err := cards.SplitMessage("人类发送", "Codex 端", text, markerCardBudget(e.MaxCardBytes))
			if err != nil {
				return snapshotPlan{}, err
			}
			for n, part := range parts {
				key := mirrorKey("human", turn.ID, i.ID, fmt.Sprint(n))
				docs = append(docs, store.Mirror{Key: key, TurnID: turn.ID, Kind: "human", Card: part.Card, Terminal: true, Replay: replay})
				active[key] = true
				if firstInput {
					parentKey, firstInput = key, false
				}
			}
		}
		// Once created, a turn's native reply anchor cannot move when the
		// history includes a subsequent steer input or changes projections.
		for _, m := range byTurn[turn.ID] {
			if m.Kind != "human" {
				parentKey, parentID = m.ParentKey, m.ParentMessageID
				break
			}
		}
		if parentKey == "" && parentID == "" {
			// An empty native turn may be a draft; public assistant content
			// without its initiating input needs recovery, not a plain send.
			if len(turn.Items) != 0 {
				return snapshotPlan{}, errors.New("public turn has no initiating human input")
			}
			continue
		}
		key := mirrorKey("progress", turn.ID)
		_, hadProgress := existing[key]
		if turn.Status == "inProgress" || hadProgress || turn.Status == "interrupted" && e.ProgressRender != nil {
			render := func(body string) string { return cards.Progress(turnLabel(turn.Status), body) }
			if e.ProgressRender != nil {
				render = func(body string) string { return e.ProgressRender(thread, turn.ID, turn.Status, body) }
			}
			card, err := boundedRenderedProgress(progressBody(turn), markerCardBudget(e.MaxCardBytes), render)
			if err != nil {
				return snapshotPlan{}, err
			}
			if card != "" {
				docs = append(docs, store.Mirror{Key: key, TurnID: turn.ID, Kind: "progress", Card: card, Terminal: turn.Status != "inProgress", Replay: replay, ParentKey: parentKey, ParentMessageID: parentID})
			}
		}
		if turn.Status != "inProgress" {
			body := finalBody(turn)
			if strings.TrimSpace(body) == "" {
				body = emptyResult(turn.Status)
			}
			budget, err := finalCardBudget(e.MaxCardBytes, e.MentionTargets)
			if err != nil {
				return snapshotPlan{}, err
			}
			parts, err := cards.SplitMessage("AI 回复", "Codex", body, budget)
			if err != nil {
				return snapshotPlan{}, err
			}
			for n, part := range parts {
				key := mirrorKey("final", turn.ID, fmt.Sprint(n))
				doc := store.Mirror{Key: key, TurnID: turn.ID, Kind: "final", Card: part.Card, Terminal: true, Replay: replay, ParentKey: parentKey, ParentMessageID: parentID}
				if n == 0 && (turn.Status == "completed" || turn.Status == "failed") {
					doc.EventType = turn.Status
					if turn.CompletedAt != nil && *turn.CompletedAt > 0 && *turn.CompletedAt <= observedAtMS/1000 {
						doc.EventAt = *turn.CompletedAt // Native 0.162.0 uses seconds, not milliseconds.
					}
				}
				docs = append(docs, doc)
				active[key] = true
			}
		}
		// A corrected full answer may use fewer segments. Unsent tails are
		// suppressed locally; existing cards lose the obsolete text via PATCH.
		for _, m := range byTurn[turn.ID] {
			if m.Kind == "progress" || active[m.Key] || m.Kind == "final" && turn.Status == "inProgress" {
				continue
			}
			m.Retired, m.Terminal = true, true
			m.Card = cards.Message(role(m.Kind), source(m.Kind), "此分片内容已更新并合并到前文。")
			docs = append(docs, m)
		}
	}
	return snapshotPlan{Docs: docs, Links: links, Mark: mark}, nil
}

func mirrorKey(kind string, parts ...string) string {
	encoded, _ := json.Marshal(parts)
	sum := sha256.Sum256(encoded)
	return kind + ":" + hex.EncodeToString(sum[:])
}

// Reject conflicting duplicates from an inconsistent paginated snapshot; exact
// duplicates are harmless. Legacy IDs may repeat across turns, not within one.
func uniqueItems(items []codexclient.Item) ([]codexclient.Item, error) {
	seen := make(map[string]codexclient.Item, len(items))
	result := make([]codexclient.Item, 0, len(items))
	for _, i := range items {
		if i.ID == "" {
			return nil, errors.New("native history item identity absent")
		}
		if old, ok := seen[i.ID]; ok {
			if !reflect.DeepEqual(old, i) {
				return nil, errors.New("conflicting duplicate native history item")
			}
			continue
		}
		seen[i.ID] = i
		result = append(result, i)
	}
	return result, nil
}

func finalBody(t codexclient.Turn) string {
	var texts []string
	legacy := ""
	for _, i := range t.Items {
		if i.Type != "agentMessage" || strings.TrimSpace(i.Text) == "" {
			continue
		}
		if i.Phase != nil && *i.Phase == "final_answer" {
			texts = append(texts, i.Text)
		} else if i.Phase == nil {
			legacy = i.Text
		}
	}
	if len(texts) == 0 {
		return legacy
	}
	return strings.Join(texts, "\n\n")
}

func progressBody(t codexclient.Turn) string {
	latest := ""
	running, finished := 0, 0
	for _, i := range t.Items {
		if i.Type == "agentMessage" && (i.Phase == nil || *i.Phase == "commentary" || *i.Phase == "partial_answer") {
			latest = i.Text
		}
		switch i.Type {
		case "commandExecution", "mcpToolCall", "dynamicToolCall", "fileChange":
			if i.Status == "inProgress" {
				running++
			} else if i.Status == "completed" || i.Status == "failed" || i.Status == "declined" {
				finished++
			}
		}
	}
	if t.Status != "inProgress" {
		return ""
	}
	runes := []rune(latest)
	if len(runes) > 800 {
		latest = string(runes[:800]) + "…"
	}
	if running+finished != 0 {
		if latest != "" {
			latest += "\n\n"
		}
		latest += fmt.Sprintf("工具：%d 项执行中 · %d 项已结束", running, finished)
	}
	return latest
}

func boundedProgress(label, body string, limit int) (string, error) {
	return boundedRenderedProgress(body, limit, func(body string) string { return cards.Progress(label, body) })
}

func boundedRenderedProgress(body string, limit int, render func(string) string) (string, error) {
	if limit == 0 {
		limit = cards.DefaultMaxCardBytes
	}
	card := render(body)
	if card == "" {
		return "", nil
	}
	if !json.Valid([]byte(card)) {
		return "", errors.New("progress renderer returned invalid card")
	}
	if len(card) <= limit {
		return card, nil
	}
	if limit < len(render("")) {
		return "", errors.New("card limit cannot hold progress status")
	}
	// Progress is a compact latest-action summary. Only this summary may be
	// shortened; public human text and the final answer always use full splits.
	runes := []rune(body)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if len(render(string(runes[:mid])+"…")) <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 {
		return render(""), nil
	}
	return render(string(runes[:lo]) + "…"), nil
}

func turnLabel(status string) string {
	switch status {
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

func emptyResult(status string) string {
	switch status {
	case "failed":
		return "本轮执行失败，请在 Codex App 中查看详情。"
	case "interrupted":
		return "本轮已停止。"
	default:
		return "本轮已结束，未返回公开回复。"
	}
}

func role(kind string) string {
	if kind == "human" {
		return "人类发送"
	}
	return "AI 回复"
}
func source(kind string) string {
	if kind == "human" {
		return "Codex 端"
	}
	return "Codex"
}

// FinalDelivered only proves all current result cards were confirmed. Native
// completion alone must not turn the originating input's reaction into a check.
func (e Engine) FinalDelivered(ctx context.Context, thread, turn string) (bool, error) {
	mirrors, err := e.Store.Mirrors(ctx, e.HostID, thread)
	if err != nil {
		return false, err
	}
	found := false
	for _, m := range mirrors {
		if m.TurnID != turn || m.Kind != "final" {
			continue
		}
		if m.Retired && m.RemoteID == "" && m.State == "confirmed" {
			continue
		}
		if !m.Retired {
			found = true
		}
		if m.State != "confirmed" || m.RemoteID == "" || m.DeliveredRevision != m.Revision {
			return false, nil
		}
	}
	return found, nil
}

// The persisted wire card later adds one invisible request-marker field.
func markerCardBudget(limit int) int {
	if limit == 0 {
		limit = cards.DefaultMaxCardBytes
	}
	if limit <= cards.MaxRequestMarkerBytes {
		return -1
	}
	return limit - cards.MaxRequestMarkerBytes
}

func finalCardBudget(limit int, targets []string) (int, error) {
	budget := markerCardBudget(limit)
	if len(targets) == 0 {
		return budget, nil
	}
	base := cards.Message("", "", "")
	mentioned, err := cards.WithMentions(base, targets)
	if err != nil {
		return 0, errors.New("notification targets exceed card budget")
	}
	budget -= len(mentioned) - len(base)
	if budget <= 0 {
		return 0, errors.New("card budget cannot hold notification targets")
	}
	return budget, nil
}
