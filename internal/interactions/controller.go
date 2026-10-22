// Package interactions handles explicit human responses on the existing daemon.
package interactions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type Native interface {
	ConnectionID() string
	GenerationID() uint64
	RespondServerRequest(context.Context, codexclient.InteractiveRequest, json.RawMessage) error
}
type Controller struct {
	closed             map[string]bool
	terminal           map[string]bool
	BeginWrite         func(context.Context, string) (context.Context, func(), error)
	ReadQuestionResult func(context.Context, codexclient.InteractiveRequest) (json.RawMessage, error)
	Store              *store.Store
	Native             Native
	HostID, AppID      string
	StartedAtMS        int64
	EnabledTypes       []string
	Options            store.MentionOptions
	Now                func() time.Time
	mu                 sync.Mutex
	requests           map[string]codexclient.InteractiveRequest
	asyncObserved      map[string]int64
}

func (c *Controller) enabled(kind string) bool {
	for _, enabled := range c.EnabledTypes {
		if enabled == kind {
			return true
		}
	}
	return false
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
func (c *Controller) scope(thread string) store.SubmissionScope {
	return store.SubmissionScope{HostID: c.HostID, AppID: c.AppID, ThreadID: thread}
}
func key(parts ...string) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (c *Controller) requestKey(r codexclient.InteractiveRequest) string {
	return key(c.HostID, c.AppID, r.ThreadID, r.TurnID, r.Method, codexclient.NativeRequestID(r.ID), r.Fingerprint)
}
func (c *Controller) Observe(ctx context.Context, e codexclient.Event) error {
	if c.Native == nil || e.Generation != c.Native.GenerationID() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, parseErr := codexclient.ParseInteractiveRequest(e)
	if parseErr != nil && r.ThreadID != "" && r.TurnID != "" && r.ItemID != "" && r.Fingerprint != "" && (e.Method == "item/commandExecution/requestApproval" || e.Method == "item/fileChange/requestApproval" || e.Method == "item/permissions/requestApproval" || e.Method == "item/tool/requestUserInput") {
		r.Kind = "unsupported"
		r.Questions = nil
		r.Decisions = nil
		r.Permissions = nil
		parseErr = nil
	}
	if parseErr == nil {
		if c.closed[key(r.ThreadID, codexclient.NativeRequestID(r.ID))] || c.terminal[key(r.ThreadID, r.TurnID)] {
			return nil
		}
		if c.requests == nil {
			c.requests = map[string]codexclient.InteractiveRequest{}
		}
		c.requests[c.requestKey(r)] = r
		return nil
	}
	var p struct {
		ThreadID  string           `json:"threadId"`
		TurnID    string           `json:"turnId"`
		Item      codexclient.Item `json:"item"`
		RequestID json.RawMessage  `json:"requestId"`
		Turn      struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(e.Params, &p) != nil {
		return nil
	}
	if (e.Method == "item/started" || e.Method == "item/completed") && p.ThreadID != "" && p.TurnID != "" && p.Item.ID != "" && p.Item.Type == "agentMessage" && p.Item.Delivery != nil && *p.Item.Delivery == "async" && len(p.Item.Questions) > 0 {
		if _, err := c.Store.Binding(ctx, c.HostID, p.ThreadID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if c.asyncObserved == nil {
			c.asyncObserved = map[string]int64{}
		}
		identity := key(p.ThreadID, p.TurnID, p.Item.ID)
		if _, seen := c.asyncObserved[identity]; !seen {
			c.asyncObserved[identity] = c.now().UnixMilli()
		}
	}
	if e.Method == "serverRequest/resolved" && codexclient.NativeRequestID(p.RequestID) != "" {
		if c.closed == nil {
			c.closed = map[string]bool{}
		}
		c.closed[key(p.ThreadID, codexclient.NativeRequestID(p.RequestID))] = true
	}
	if e.Method == "turn/completed" && p.Turn.ID != "" {
		if c.terminal == nil {
			c.terminal = map[string]bool{}
		}
		c.terminal[key(p.ThreadID, p.Turn.ID)] = true
	}
	for k, r := range c.requests {
		resolved := e.Method == "serverRequest/resolved" && r.ThreadID == p.ThreadID && codexclient.NativeRequestID(r.ID) == codexclient.NativeRequestID(p.RequestID)
		terminal := e.Method == "turn/completed" && r.ThreadID == p.ThreadID && r.TurnID == p.Turn.ID
		if resolved || terminal {
			if err := c.Store.ResolveInteraction(ctx, k, c.Native.ConnectionID()); err != nil {
				return err
			}
			delete(c.requests, k)
		}
	}
	return nil
}
func (c *Controller) Stage(ctx context.Context, thread string, turns []codexclient.Turn) error {
	if err := c.Prepare(ctx, thread, turns); err != nil {
		return err
	}
	return c.StageCards(ctx, thread, turns)
}

// Prepare records question identities before ordinary message projection. The
// enabled type list controls answering in Feishu, not question visibility.
func (c *Controller) Prepare(ctx context.Context, thread string, turns []codexclient.Turn) error {
	if _, err := c.Store.Binding(ctx, c.HostID, thread); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if c.Native == nil {
		return nil
	}
	// Observer and history preparation serialize durable closure against first
	// insertion. Network responses never run with this mutex held.
	c.mu.Lock()
	for _, r := range c.requests {
		if r.ThreadID != thread {
			continue
		}
		terminal := false
		for _, t := range turns {
			if t.ID == r.TurnID && t.Status != "inProgress" {
				terminal = true
			}
		}
		if terminal {
			continue
		}
		snapshot := r
		snapshot.Generation = 0
		b, _ := json.Marshal(snapshot)
		state := "pending"
		if r.Kind == "sensitive" || r.Kind == "unsupported" {
			state = "unsupported"
		}
		eventAt, replay := int64(0), true
		routing := key(r.TurnID, "0:"+codexclient.NativeRequestID(r.ID))
		err := c.Store.DB.QueryRowContext(ctx, `SELECT event_at,replay FROM app_requests WHERE host_id=? AND app_id=? AND thread_id=? AND request_key=?`, c.HostID, c.AppID, thread, routing).Scan(&eventAt, &replay)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			c.mu.Unlock()
			return err
		}
		row := store.InteractionRequest{Key: c.requestKey(r), HostID: c.HostID, AppID: c.AppID, ThreadID: thread, TurnID: r.TurnID, ItemID: r.ItemID, Mode: "native", Method: r.Method, NativeID: codexclient.NativeRequestID(r.ID), Payload: string(b), ConnectionID: c.Native.ConnectionID(), Generation: int64(c.Native.GenerationID()), State: state, CreatedAtMS: c.now().UnixMilli(), EventAt: eventAt, Replay: replay}
		if err = c.Store.ObserveInteraction(ctx, row); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	c.mu.Unlock()
	for _, t := range turns {
		for _, item := range t.Items {
			if item.Type != "agentMessage" || item.Delivery == nil || *item.Delivery != "async" || len(item.Questions) == 0 {
				continue
			}
			k := key(c.HostID, c.AppID, thread, t.ID, "async", item.ID)
			_, existingErr := c.Store.Interaction(ctx, k)
			fresh := t.StartedAt != nil && *t.StartedAt >= c.StartedAtMS/1000
			identity := key(thread, t.ID, item.ID)
			c.mu.Lock()
			observedAt, observed := c.asyncObserved[identity]
			c.mu.Unlock()
			fresh = fresh || observed
			if !fresh && errors.Is(existingErr, sql.ErrNoRows) {
				continue
			}
			if existingErr != nil && !errors.Is(existingErr, sql.ErrNoRows) {
				return existingErr
			}
			r := codexclient.InteractiveRequest{ThreadID: thread, TurnID: t.ID, ItemID: item.ID, Kind: "question", Method: "async"}
			for i, q := range item.Questions {
				question := codexclient.InputQuestion{ID: fmt.Sprintf("q_%d", i), Question: q.Title, Other: true}
				for _, opt := range q.Options {
					question.Options = append(question.Options, codexclient.InputOption{Label: opt})
				}
				r.Questions = append(r.Questions, question)
			}
			payload, _ := json.Marshal(r)
			row := store.InteractionRequest{Key: k, HostID: c.HostID, AppID: c.AppID, ThreadID: thread, TurnID: t.ID, ItemID: item.ID, Mode: "async", Method: "async", Payload: string(payload), ConnectionID: c.Native.ConnectionID(), Generation: int64(c.Native.GenerationID()), State: "pending", CreatedAtMS: c.now().UnixMilli(), Replay: !fresh}
			if fresh {
				row.EventAt = c.now().Unix()
				if observed {
					row.EventAt = observedAt / 1000
				}
			}
			if err := c.Store.ObserveInteraction(ctx, row); err != nil {
				return err
			}
			c.mu.Lock()
			delete(c.asyncObserved, identity)
			c.mu.Unlock()
		}
	}
	rows, err := c.Store.Interactions(ctx, c.scope(thread))
	if err != nil {
		return err
	}
	return c.closeAnsweredAsync(ctx, rows, turns)
}
func (c *Controller) StageCards(ctx context.Context, thread string, turns []codexclient.Turn) error {
	rows, err := c.Store.Interactions(ctx, c.scope(thread))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if err = c.closeAnsweredAsync(ctx, rows, turns); err != nil {
		return err
	}
	rows, err = c.Store.Interactions(ctx, c.scope(thread))
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Mode == "async" && row.State == "resolved" {
			if _, err := c.Store.Mirror(ctx, c.HostID, thread, store.InteractionCardKey(row.Key)); errors.Is(err, sql.ErrNoRows) {
				continue // Already answered before first observation: no late question or @.
			} else if err != nil {
				return err
			}
		}
		if row.Mode == "native" && row.State != "resolved" {
			for _, t := range turns {
				if t.ID == row.TurnID && t.Status != "inProgress" {
					if err := c.Store.ResolveInteraction(ctx, row.Key, row.ConnectionID); err != nil {
						return err
					}
					row, err = c.Store.Interaction(ctx, row.Key)
					if err != nil {
						return err
					}
				}
			}
		}
		v, err := c.view(ctx, row, turns)
		if err != nil {
			return err
		}
		v.MaxCardBytes = cards.DefaultMaxCardBytes - cards.MaxRequestMarkerBytes
		if len(c.Options.Targets) > 0 {
			base := cards.Message("", "", "")
			withMentions, mentionErr := cards.WithMentions(base, c.Options.Targets)
			if mentionErr == nil && len(withMentions)-len(base)+cards.MaxMentionBlockExtraBytes < v.MaxCardBytes-2048 {
				v.MaxCardBytes -= len(withMentions) - len(base) + cards.MaxMentionBlockExtraBytes
			}
		}
		card := cards.Interaction(v)
		var submissionRevision int64
		if v.Queue != nil {
			submissionRevision = v.Queue.Revision
		}
		if err = c.Store.StageInteractionSubmissionCard(ctx, row, card, actionableCard(card), submissionRevision); err != nil && !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return c.Store.ScheduleMentions(ctx, c.HostID, thread, c.Options)
}
func (c *Controller) view(ctx context.Context, row store.InteractionRequest, turns []codexclient.Turn) (cards.InteractionView, error) {
	var r codexclient.InteractiveRequest
	if json.Unmarshal([]byte(row.Payload), &r) != nil {
		return cards.InteractionView{}, store.ErrConflict
	}
	v := cards.InteractionView{Kind: r.Kind, State: row.State, Reason: r.Reason, CWD: r.CWD, Command: r.Command, Token: row.Token, Revision: row.Revision, Enabled: row.State == "pending" && row.AttemptID == "" && c.Native != nil && row.ConnectionID == c.Native.ConnectionID()}
	if r.Kind == "sensitive" {
		v.Kind = "question"
		v.Reason = "此请求包含私密问题，请在 Codex App 中回答。"
	} else if r.Kind == "unsupported" {
		v.Reason = "此类型的原生审批尚未验证，请在 Codex App 中处理。"
	}
	if row.Mode == "async" {
		v.Kind = "async_question"
		if row.State == "unsupported" {
			v.Reason = "问题已有部分回答或身份存在歧义，请回 Codex App 核验。"
		}
	}
	if !c.enabled(v.Kind) {
		v.Enabled = false
		if c.Native != nil && row.State == "pending" && v.Kind != "async_question" {
			v.Reason += "\n此类型尚未开启飞书操作，请在 Codex App 中处理。"
		}
	}
	for _, decision := range r.Decisions {
		label := map[string]string{"accept": "允许本次", "decline": "拒绝", "cancel": "停止本轮", "acceptForSession": "本会话允许同类请求"}[decision]
		v.Decisions = append(v.Decisions, cards.InteractionDecision{Value: decision, Label: label})
	}
	if r.Kind == "network" {
		var p struct{ Host, Protocol string }
		json.Unmarshal(r.Permissions, &p)
		v.Reason = strings.TrimSpace(r.Reason + "\n访问 " + p.Host + "（" + p.Protocol + "）")
	}
	if r.Kind == "command" && len(r.Permissions) > 0 && string(r.Permissions) != "null" {
		v.Details = append(v.Details, cards.InteractionDetail{Title: "请求的额外权限", Content: string(r.Permissions)})
	}
	if r.Kind == "permissions" {
		v.Details = append(v.Details, cards.InteractionDetail{Title: "完整权限范围与保留限制", Content: string(r.Permissions)})
		choices, err := r.PermissionChoices()
		if err != nil {
			return v, err
		}
		for _, p := range choices {
			v.Permissions = append(v.Permissions, cards.InteractionOption{Value: p.Value, Label: p.Label})
		}
	}
	for i, q := range r.Questions {
		if r.Kind == "sensitive" {
			break
		}
		question := cards.InteractionQuestion{Field: fmt.Sprintf("q_%d", i), Header: q.Header, Question: q.Question, Other: q.Other}
		for n, o := range q.Options {
			question.Options = append(question.Options, cards.InteractionOption{Value: strconv.Itoa(n), Label: o.Label, Description: o.Description})
		}
		v.Questions = append(v.Questions, question)
	}
	if r.Kind == "file" {
		if r.GrantRoot != "" {
			v.Enabled = false
			v.Reason += "\n请求写入根目录：" + r.GrantRoot + "。该授权作用域尚未核验，请在 Codex App 中处理。"
		}
		for _, t := range turns {
			if t.ID != row.TurnID {
				continue
			}
			for _, item := range t.Items {
				if item.ID != row.ItemID {
					continue
				}
				for _, f := range item.Changes {
					var change struct {
						Type     string  `json:"type"`
						MovePath *string `json:"move_path"`
					}
					operation := ""
					if json.Unmarshal(f.Kind, &change) == nil {
						operation = map[string]string{"add": "新增", "delete": "删除", "update": "修改"}[change.Type]
					}
					if operation == "" || f.Path == "" {
						v.Enabled = false
						operation = "操作详情尚未核验，请回 Codex App 查看"
					}
					if change.MovePath != nil {
						operation += "\n移动到：" + *change.MovePath
					}
					v.Details = append(v.Details, cards.InteractionDetail{Title: f.Path, Content: "操作：" + operation + "\n\n" + f.Diff})
					if f.Diff == "" {
						v.Enabled = false
						v.Reason += "\n文件差异不完整，请在 Codex App 中查看。"
					}
				}
			}
		}
		if len(v.Details) == 0 {
			v.Enabled = false
			v.Reason += "\n尚未取得文件修改详情，请在 Codex App 中查看。"
		}
	}
	if r.Kind == "command" && r.Command == "" {
		v.Enabled = false
		v.Reason += "\n尚未取得完整命令，请在 Codex App 中查看。"
	}
	if row.AttemptID != "" {
		a, err := c.Store.InteractionAttempt(ctx, row.AttemptID)
		if err != nil {
			return v, err
		}
		v.Summary = a.Summary
		if row.State == "pending" || row.State == "disconnected" {
			v.State = map[string]string{"ready": "claimed", "dispatching": "submitted", "submitted": "submitted", "unknown": "unknown", "rejected": "failed"}[a.State]
			if row.Mode == "native" && a.State == "ready" && (a.ConnectionID != row.ConnectionID || a.Generation != row.Generation || c.Native == nil) {
				v.State = "disconnected"
				v.Reason += "\n上次操作尚未发送，连接已变化，请在 Codex App 中处理。"
			}
		}
	}
	if row.SubmissionID != "" {
		scope := c.scope(row.ThreadID)
		offline := c.Native == nil || row.ConnectionID != c.Native.ConnectionID()
		queue, err := c.Store.SubmissionQueueView(ctx, scope, row.SubmissionID, store.QueueCardOptions{LocalPending: offline, Offline: offline})
		if err != nil {
			return v, err
		}
		if row.State != "resolved" {
			v.State = queue.State
		} else if queue.State == "queued" {
			v.Reason += "\n飞书提交的回答仍在队列中，可移除。"
		}
		// Existing separate queue journals keep ownership, including unknown deliveries.
		m, err := c.Store.SubmissionCard(ctx, scope, row.SubmissionID)
		if err != nil {
			return v, err
		}
		if m.Key == store.InteractionCardKey(row.Key) {
			v.Queue = &queue
		}
	}
	if r.Kind == "question" {
		if err := c.questionReceipt(ctx, row, r, &v); err != nil {
			return v, err
		}
	}
	return v, nil
}

func (c *Controller) questionReceipt(ctx context.Context, row store.InteractionRequest, request codexclient.InteractiveRequest, view *cards.InteractionView) error {
	var result []byte
	if row.State == "resolved" {
		stored, err := c.Store.InteractionAnswer(ctx, row.Key)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			result = []byte(stored)
		} else if row.Mode == "native" && c.ReadQuestionResult != nil {
			if recorded, readErr := c.ReadQuestionResult(ctx, request); readErr == nil {
				if _, validationErr := codexclient.ParseQuestionAnswers(request, recorded); validationErr == nil {
					if err := c.Store.RecordInteractionAnswer(ctx, row.Key, string(recorded)); err != nil {
						return err
					}
					result = recorded
				}
			}
		}
	}
	if len(result) > 0 {
		view.AnswerStatus = "recorded"
		view.Summary = ""
	} else if row.AttemptID != "" {
		attempt, err := c.Store.InteractionAttempt(ctx, row.AttemptID)
		if err != nil {
			return err
		}
		result = []byte(attempt.Result)
		view.AnswerStatus = "submitted"
	}
	if len(result) == 0 {
		return nil
	}
	answers, err := codexclient.ParseQuestionAnswers(request, result)
	if err != nil {
		view.AnswerStatus = ""
		return nil
	}
	for i, q := range request.Questions {
		if i < len(view.Questions) {
			view.Questions[i].Answers = append([]string(nil), answers[q.ID].Answers...)
		}
	}
	return nil
}

// Claim does only durable local work inside the three-second callback budget.
func (c *Controller) Claim(ctx context.Context, scope store.SubmissionScope, user string, operators []string, chat, message, token string, revision int64, decision string, form map[string]any) (string, error) {
	if c.Native == nil {
		return "", store.ErrSubmissionStale
	}
	row, err := c.Store.InteractionByToken(ctx, token)
	if err != nil {
		return "", err
	}
	var r codexclient.InteractiveRequest
	if json.Unmarshal([]byte(row.Payload), &r) != nil {
		return "", store.ErrConflict
	}
	if row.State != "pending" {
		return "", store.ErrSubmissionStale
	}
	kind := r.Kind
	if row.Mode == "async" {
		kind = "async_question"
	}
	if !c.enabled(kind) {
		return "", store.ErrSubmissionStale
	}
	appendAnswer := row.Mode == "async" && decision == "append_answer"
	nativeDecision := decision
	if appendAnswer {
		nativeDecision = ""
	}
	result, err := codexclient.BuildInteractiveResult(r, nativeDecision, form)
	if err != nil {
		return "", err
	}
	summary := "已收到你的回答"
	if row.Mode == "native" && r.Kind != "question" {
		summary = "决定：" + map[string]string{"accept": "允许本次", "decline": "拒绝", "cancel": "停止本轮", "acceptForSession": "本会话允许同类请求", "": "授予所选权限"}[decision]
	}
	if appendAnswer {
		body, buildErr := asyncReplyBody(row, r, result)
		if buildErr != nil {
			return "", buildErr
		}
		_, err = c.Store.ClaimAsyncInteraction(ctx, scope, user, operators, chat, message, token, revision, c.Native.ConnectionID(), int64(c.Native.GenerationID()), string(result), summary, body, c.now().UnixMilli())
	} else {
		_, err = c.Store.ClaimInteraction(ctx, scope, user, operators, chat, message, token, revision, c.Native.ConnectionID(), int64(c.Native.GenerationID()), string(result), summary, c.now().UnixMilli())
	}
	return row.ThreadID, err
}
func (c *Controller) Tick(ctx context.Context, thread string) error {
	rows, err := c.Store.Interactions(ctx, c.scope(thread))
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.AttemptID == "" || row.State != "pending" {
			continue
		}
		var request codexclient.InteractiveRequest
		if json.Unmarshal([]byte(row.Payload), &request) != nil {
			return store.ErrConflict
		}
		kind := request.Kind
		if row.Mode == "async" {
			kind = "async_question"
		}
		if !c.enabled(kind) {
			continue
		}
		opctx, release := ctx, func() {}
		if c.BeginWrite != nil {
			var guardErr error
			opctx, release, guardErr = c.BeginWrite(ctx, thread)
			if guardErr != nil {
				return guardErr
			}
			if opctx == nil || release == nil {
				return store.ErrIneligible
			}
		}
		if row.Mode == "async" {
			if err = c.submitAsync(opctx, row); err != nil {
				release()
				return err
			}
			release()
			continue
		}
		if c.Native == nil {
			release()
			continue
		}
		a, claimed, err := c.Store.DispatchInteraction(opctx, row.Key, c.Native.ConnectionID(), int64(c.Native.GenerationID()))
		if err != nil {
			release()
			return err
		}
		if !claimed {
			release()
			continue
		}
		var req codexclient.InteractiveRequest
		if json.Unmarshal([]byte(row.Payload), &req) != nil {
			release()
			return store.ErrConflict
		}
		req.Generation = uint64(row.Generation)
		sendCtx, cancel := context.WithTimeout(opctx, 10*time.Second)
		sendErr := c.Native.RespondServerRequest(sendCtx, req, json.RawMessage(a.Result))
		cancel()
		release()
		outcome := "submitted"
		if sendErr != nil {
			outcome = "unknown"
			if errors.Is(sendErr, codexclient.ErrRequestStale) || errors.Is(sendErr, codexclient.ErrInvalidAnswer) {
				outcome = "rejected"
			}
		}
		if err = c.Store.FinishInteraction(ctx, a.ID, outcome); err != nil {
			return err
		}
	}
	return nil
}
func (c *Controller) submitAsync(ctx context.Context, row store.InteractionRequest) error {
	a, err := c.Store.InteractionAttempt(ctx, row.AttemptID)
	if err != nil {
		return err
	}
	if a.State != "ready" {
		return nil
	}
	var req codexclient.InteractiveRequest
	if json.Unmarshal([]byte(row.Payload), &req) != nil {
		return store.ErrConflict
	}
	body, err := asyncReplyBody(row, req, json.RawMessage(a.Result))
	if err != nil {
		return err
	}
	return c.Store.SubmitAsyncInteraction(ctx, row.Key, body)
}

func asyncReplyBody(row store.InteractionRequest, req codexclient.InteractiveRequest, encoded json.RawMessage) (string, error) {
	var result struct {
		Answers map[string]struct {
			Answers []string `json:"answers"`
		} `json:"answers"`
	}
	if json.Unmarshal(encoded, &result) != nil {
		return "", store.ErrConflict
	}
	replies := []map[string]string{}
	for i, q := range req.Questions {
		identity, _ := json.Marshal([]any{"request_user_input_async", row.ItemID, i})
		replies = append(replies, map[string]string{"questionItemId": string(identity), "question": q.Question, "answer": strings.Join(result.Answers[q.ID].Answers, "\n")})
	}
	text, _ := json.Marshal(replies)
	return "<send_user_message_question_reply>\n" + string(text) + "\n</send_user_message_question_reply>", nil
}

func actionableCard(card string) bool {
	var value any
	if json.Unmarshal([]byte(card), &value) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			if x["tag"] == "button" {
				if behaviors, ok := x["behaviors"].([]any); ok {
					for _, b := range behaviors {
						m, _ := b.(map[string]any)
						val, _ := m["value"].(map[string]any)
						if val["action"] == "interaction_submit" {
							return true
						}
					}
				}
			}
			for _, child := range x {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range x {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}
