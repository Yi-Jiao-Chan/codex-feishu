package cot

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"codex-feishu/internal/feishu"
)

// State describes only confirmed event effects. Callers freeze Batch.Events and
// Batch.State together before dispatch, and install State only on confirmation.
type State struct {
	ThreadID        string               `json:"thread_id,omitempty"`
	TurnID          string               `json:"turn_id,omitempty"`
	Started         bool                 `json:"started"`
	Finished        bool                 `json:"finished"`
	LastTimestampMS int64                `json:"last_timestamp_ms,omitempty"`
	Items           map[string]ItemState `json:"items,omitempty"`
}

type ItemState struct {
	Kind    string       `json:"kind"`
	Message MessageState `json:"message,omitempty"`
	Note    MessageState `json:"note,omitempty"`
	Tool    ToolState    `json:"tool,omitempty"`
}

// Text is the confirmed public prefix for this generation. Generations make
// non-prefix corrections explicit; native reasoning events cannot replace text.
type MessageState struct {
	ReasoningStarted bool   `json:"reasoning_started"`
	ReasoningEnded   bool   `json:"reasoning_ended"`
	Generation       int    `json:"generation,omitempty"`
	Started          bool   `json:"started"`
	Ended            bool   `json:"ended"`
	Text             string `json:"text,omitempty"`
}

type ToolState struct {
	Started bool   `json:"started"`
	Args    bool   `json:"args"`
	Ended   bool   `json:"ended"`
	Title   string `json:"title,omitempty"`
	Result  string `json:"result,omitempty"`
}

type Batch struct {
	Events   []feishu.COTEvent `json:"events"`
	State    State             `json:"state"`
	CaughtUp bool              `json:"caught_up"`
}

type planner struct {
	doc Document
	now int64
	Batch
}

// Plan converts a complete safe snapshot to at most 50 ordered events. It is
// pure: the supplied confirmed state is never mutated. Timestamps are local
// observation/order metadata, not inferred native tool execution times.
func Plan(doc Document, confirmed State, nowMS int64) (Batch, error) {
	if err := validateDocument(doc); err != nil {
		return Batch{}, err
	}
	if nowMS <= 0 || confirmed.LastTimestampMS < 0 || confirmed.LastTimestampMS == math.MaxInt64 ||
		(confirmed.ThreadID != "" && confirmed.ThreadID != doc.ThreadID) ||
		(confirmed.TurnID != "" && confirmed.TurnID != doc.TurnID) ||
		(confirmed.Started && (confirmed.ThreadID == "" || confirmed.TurnID == "")) ||
		(confirmed.Finished && !confirmed.Started) {
		return Batch{}, errors.New("invalid confirmed COT state")
	}
	p := planner{doc: doc, now: nowMS, Batch: Batch{State: confirmed}}
	p.State.ThreadID, p.State.TurnID = doc.ThreadID, doc.TurnID
	p.State.Items = make(map[string]ItemState, len(confirmed.Items))
	for id, value := range confirmed.Items {
		p.State.Items[id] = value
	}
	// A completed platform lifecycle is never reopened by a corrected history
	// scan. Final answer corrections continue through the normal reply channel.
	if p.State.Finished {
		p.CaughtUp = true
		return p.Batch, nil
	}
	if !p.State.Started {
		ts := nowMS
		if len(doc.Items) > 0 {
			ts = doc.Items[0].TimestampMS
		}
		if !p.emit("RUN_STARTED", p.runContent(""), ts) {
			return Batch{}, errors.New("cannot encode COT run start")
		}
		p.State.Started = true
	}
	terminal := doc.Status != "inProgress"
	seen := make(map[string]bool, len(doc.Items))
	for index, item := range doc.Items {
		seen[item.ID] = true
		state := p.State.Items[item.ID]
		if state.Kind != "" && state.Kind != item.Kind {
			return Batch{}, errors.New("COT item changed kind")
		}
		state.Kind = item.Kind
		complete := false
		if item.Kind == "message" {
			complete = p.message(item.ID, item.Text, item.TimestampMS, terminal || index < len(doc.Items)-1, &state.Message)
		} else {
			complete = p.tool(item, terminal, &state)
		}
		p.State.Items[item.ID] = state
		if !complete {
			if len(p.Events) == 0 {
				return Batch{}, errors.New("cannot encode COT item event")
			}
			return p.Batch, nil
		}
	}
	if terminal {
		// Legacy phase-less messages may disappear from process history when
		// selected as the final reply. Close any already-visible open entities;
		// an absent tool is never invented as successful during this closure.
		if !p.closeAbsent(seen) {
			return p.Batch, nil
		}
		event, content := "RUN_FINISHED", p.runContent("done")
		if doc.Status == "interrupted" {
			content = p.runContent("interrupted")
		} else if doc.Status == "failed" {
			event, content = "RUN_ERROR", map[string]any{"message": "本轮执行失败，请在 Codex App 查看详情。", "code": "NATIVE_TURN_FAILED"}
		}
		if !p.emit(event, content, nowMS) {
			return p.Batch, nil
		}
		p.State.Finished = true
	}
	p.CaughtUp = true
	return p.Batch, nil
}

func (p *planner) runContent(status string) map[string]any {
	threadScope := p.doc
	threadScope.TurnID = ""
	c := map[string]any{"threadId": StableID(threadScope, "thread", "", 0), "runId": StableID(p.doc, "run", "", 0)}
	if status != "" {
		c["status"] = status
	}
	return c
}

func (p *planner) closeAbsent(seen map[string]bool) bool {
	var ids []string
	for id := range p.State.Items {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		state := p.State.Items[id]
		if state.Note.ReasoningStarted && !state.Note.ReasoningEnded {
			complete := p.closeMessage("tool-note:"+id, p.now, &state.Note)
			p.State.Items[id] = state
			if !complete {
				return false
			}
		}
		if state.Kind == "message" && state.Message.ReasoningStarted && !state.Message.ReasoningEnded {
			complete := p.closeMessage(id, p.now, &state.Message)
			p.State.Items[id] = state
			if !complete {
				return false
			}
		} else if state.Kind == "tool" && state.Tool.Started && state.Tool.Result == "" {
			item := Item{ID: id, Kind: "tool", Title: state.Tool.Title, Status: "unknown", TimestampMS: p.now}
			complete := p.tool(item, true, &state)
			p.State.Items[id] = state
			if !complete {
				return false
			}
		}
		p.State.Items[id] = state
	}
	return true
}

func (p *planner) emit(kind string, content map[string]any, observedMS int64) bool {
	if len(p.Events) >= feishu.MaxCOTEvents || p.State.LastTimestampMS == math.MaxInt64 {
		return false
	}
	ts := max(observedMS, p.State.LastTimestampMS+1)
	encoded, err := json.Marshal(content)
	if err != nil {
		return false
	}
	e := feishu.COTEvent{EventType: kind, Content: string(encoded), Timestamp: ts}
	wire, err := json.Marshal(e)
	if err != nil || len(wire) > feishu.MaxCOTEventBytes {
		return false
	}
	p.Events = append(p.Events, e)
	p.State.LastTimestampMS = ts
	return true
}

func (p *planner) message(itemID, text string, ts int64, final bool, state *MessageState) bool {
	if state.Generation < 0 || !utf8.ValidString(state.Text) || state.Ended && !state.Started {
		return false
	}
	target := messageTarget(text, state.Generation)
	if state.Started && (!strings.HasPrefix(target, state.Text) || state.Ended && target != state.Text) {
		if !p.closeMessage(itemID, ts, state) {
			return false
		}
		*state = MessageState{Generation: state.Generation + 1}
		target = messageTarget(text, state.Generation)
	}
	id := StableID(p.doc, "message", itemID, state.Generation)
	if !state.ReasoningStarted {
		if !p.emit("REASONING_START", map[string]any{"messageId": id}, ts) {
			return false
		}
		state.ReasoningStarted = true
	}
	if !state.Started {
		if !p.emit("REASONING_MESSAGE_START", map[string]any{"messageId": id, "role": "reasoning"}, ts) {
			return false
		}
		state.Started = true
	}
	for len(state.Text) < len(target) {
		rest := target[len(state.Text):]
		chunk := p.textChunk("REASONING_MESSAGE_CONTENT", "messageId", id, "delta", rest, ts)
		if chunk == "" {
			return false
		}
		state.Text += chunk
	}
	if final || state.Ended {
		return p.closeMessage(itemID, ts, state)
	}
	return true
}

func (p *planner) closeMessage(itemID string, ts int64, state *MessageState) bool {
	id := StableID(p.doc, "message", itemID, state.Generation)
	if state.Started && !state.Ended {
		if !p.emit("REASONING_MESSAGE_END", map[string]any{"messageId": id}, ts) {
			return false
		}
		state.Ended = true
	}
	if state.ReasoningStarted && !state.ReasoningEnded {
		if !p.emit("REASONING_END", map[string]any{"messageId": id}, ts) {
			return false
		}
		state.ReasoningEnded = true
	}
	return true
}

func messageTarget(text string, generation int) string {
	if generation > 0 {
		return "公开进度已更正：\n" + text
	}
	return text
}

// textChunk budgets the actual doubly encoded wire event, including JSON HTML
// escaping and UTF-8. Only the accepted prefix is recorded in confirmed state.
func (p *planner) textChunk(kind, idField, id, textField, text string, ts int64) string {
	if len(p.Events) >= feishu.MaxCOTEvents {
		return ""
	}
	runes := []rune(text)
	lo, hi := 0, min(len(runes), feishu.MaxCOTEventBytes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		content, _ := json.Marshal(map[string]any{idField: id, textField: string(runes[:mid])})
		event, _ := json.Marshal(feishu.COTEvent{EventType: kind, Content: string(content), Timestamp: max(ts, p.State.LastTimestampMS+1)})
		if len(event) <= feishu.MaxCOTEventBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 {
		return ""
	}
	chunk := string(runes[:lo])
	if !p.emit(kind, map[string]any{idField: id, textField: chunk}, ts) {
		return ""
	}
	return chunk
}

func (p *planner) tool(item Item, terminal bool, state *ItemState) bool {
	status := item.Status
	if terminal && status == "inProgress" || state.Tool.Result != "" && status == "inProgress" {
		status = "unknown"
	}
	// An unavailable or declined item is not evidence that an invocation began.
	// Keep it visible as a public note without inflating native tool-call counts.
	if !state.Tool.Started && (status == "unknown" || status == "declined") {
		return p.message("tool-note:"+item.ID, toolResult(item.Title, status), item.TimestampMS, true, &state.Note)
	}
	const summaryPrefix = "工具动作摘要更新："
	// Note.Text is the acknowledged public summary, including any correction
	// generation prefix. Keep reconciling it when the title returns to the
	// original TOOL_CALL_START title; that immutable label is not the latest note.
	// A later generation also retains earlier note history when a batch ends
	// before any correction text is acknowledged. Reconcile that empty note too.
	hasSummary := state.Note.Generation > 0 || strings.HasPrefix(strings.TrimPrefix(state.Note.Text, messageTarget("", 1)), summaryPrefix)
	syncSummary := status == "inProgress" && (hasSummary || state.Tool.Started && state.Tool.Title != item.Title)
	if state.Note.ReasoningStarted && !state.Note.ReasoningEnded && !syncSummary {
		if !p.closeMessage("tool-note:"+item.ID, item.TimestampMS, &state.Note) {
			return false
		}
	}
	id := StableID(p.doc, "tool", item.ID, 0)
	if !state.Tool.Started {
		icon := item.Icon
		if icon == "" {
			icon = "default"
		}
		title := item.Title
		if status == "failed" {
			title += " · 执行失败"
		}
		if !p.emit("TOOL_CALL_START", map[string]any{"toolCallId": id, "toolCallName": title, "title": title, "icon": icon}, item.TimestampMS) {
			return false
		}
		state.Tool.Started, state.Tool.Title = true, item.Title
	}
	if !state.Tool.Args {
		// This is intentionally an empty display payload, never native inputs.
		if !p.emit("TOOL_CALL_ARGS", map[string]any{"toolCallId": id, "delta": "{}"}, item.TimestampMS) {
			return false
		}
		state.Tool.Args = true
	}
	if !state.Tool.Ended {
		// END ends parameter streaming; RESULT alone confirms terminal display.
		if !p.emit("TOOL_CALL_END", map[string]any{"toolCallId": id}, item.TimestampMS) {
			return false
		}
		state.Tool.Ended = true
	}
	if status == "inProgress" {
		if state.Tool.Title != item.Title || hasSummary {
			return p.message("tool-note:"+item.ID, summaryPrefix+item.Title, item.TimestampMS, true, &state.Note)
		}
		return true
	}
	result := toolResult(item.Title, status)
	if state.Tool.Result != result {
		if !p.emit("TOOL_CALL_RESULT", map[string]any{"messageId": StableID(p.doc, "result", item.ID, 0), "toolCallId": id, "role": "tool", "content": result}, item.TimestampMS) {
			return false
		}
		state.Tool.Result = result
	}
	return true
}

func toolResult(title, status string) string {
	switch status {
	case "completed":
		return title + " · 已完成（仅同步状态，原始输出未同步）"
	case "failed":
		return title + " · 执行失败，请在 Codex App 查看详情（原始输出未同步）"
	case "declined":
		return title + " · 已拒绝，未确认执行"
	default:
		return title + " · 状态待核验，未确认执行或成功"
	}
}
