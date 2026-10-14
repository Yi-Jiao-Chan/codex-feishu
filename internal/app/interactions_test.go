package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/interactions"
	"codex-feishu/internal/store"
)

type interactiveNative struct {
	connection string
	generation uint64
	writes     int
	result     json.RawMessage
	err        error
}

func (n *interactiveNative) ConnectionID() string { return n.connection }
func (n *interactiveNative) GenerationID() uint64 { return n.generation }
func (n *interactiveNative) RespondServerRequest(_ context.Context, _ codexclient.InteractiveRequest, b json.RawMessage) error {
	n.writes++
	n.result = append([]byte(nil), b...)
	return n.err
}
func interactiveFixture(t *testing.T) (*interactions.Controller, *RequestTracker, *interactiveNative, codexclient.Event) {
	t.Helper()
	tr, now, o := durableTrackerFixture(t)
	bindTracker(t, tr)
	stageTrackerProgress(t, tr, *now)
	n := &interactiveNative{connection: "connection", generation: 1}
	c := &interactions.Controller{Store: tr.Store, Native: n, HostID: "host", AppID: "app", StartedAtMS: now.Add(-time.Minute).UnixMilli(), EnabledTypes: []string{"command", "file", "network", "permissions", "question", "async_question"}, Options: o, Now: func() time.Time { return *now }}
	p := `{"threadId":"thread","turnId":"turn","itemId":"item","command":"printf safe","cwd":"/project","reason":"合成测试","availableDecisions":["accept","decline","cancel"],"questions":[{"id":"question","header":"选择","question":"选哪个？","options":[{"label":"选项一","description":"保持"},{"label":"选项二","description":"调整"}]}]}`
	e := codexclient.Event{Generation: 1, Method: "item/tool/requestUserInput", RequestID: json.RawMessage(`"request"`), Params: json.RawMessage(p)}
	return c, tr, n, e
}
func stageInteractive(t *testing.T, c *interactions.Controller, e codexclient.Event) store.InteractionRequest {
	t.Helper()
	ctx := context.Background()
	if err := c.Observe(ctx, e); err != nil {
		t.Fatal(err)
	}
	at := c.Now().Add(-20 * time.Second).Unix()
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", StartedAt: &at}
	if err := c.Stage(ctx, "thread", []codexclient.Turn{turn}); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Store.Interactions(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if err != nil || len(rows) != 1 {
		t.Fatal("request not staged", err, len(rows))
	}
	row := rows[0]
	if _, err = c.Store.DB.ExecContext(ctx, `UPDATE mirrors SET remote_id='request-card',delivered_revision=revision WHERE mirror_key=?`, store.InteractionCardKey(row.Key)); err != nil {
		t.Fatal(err)
	}
	row, err = c.Store.Interaction(ctx, row.Key)
	if err != nil {
		t.Fatal(err)
	}
	return row
}
func claimInteractive(c *interactions.Controller, row store.InteractionRequest, user, chat, message string) (string, error) {
	return c.Claim(context.Background(), store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}, user, []string{"ou_user"}, chat, message, row.Token, row.Revision, "", map[string]any{"q_0": "1", "q_0_notes": "完整补充"})
}

func TestInteractiveActualAnswerReceiptOverridesLocalIntentAndSurvivesOffline(t *testing.T) {
	c, _, native, event := interactiveFixture(t)
	row := stageInteractive(t, c, event)
	if _, err := claimInteractive(c, row, "ou_user", "chat", "request-card"); err != nil {
		t.Fatal(err)
	}
	if err := c.Tick(context.Background(), "thread"); err != nil {
		t.Fatal(err)
	}
	resolved := codexclient.Event{Generation: 1, Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"thread","requestId":"request"}`)}
	if err := c.Observe(context.Background(), resolved); err != nil {
		t.Fatal(err)
	}
	reads := 0
	c.ReadQuestionResult = func(context.Context, codexclient.InteractiveRequest) (json.RawMessage, error) {
		reads++
		return json.RawMessage(`{"answers":{"question":{"answers":["选项一"]}}}`), nil
	}
	if err := c.StageCards(context.Background(), "thread", nil); err != nil {
		t.Fatal(err)
	}
	m, err := c.Store.Mirror(context.Background(), "host", "thread", store.InteractionCardKey(row.Key))
	if err != nil || !strings.Contains(m.Card, "已回答") || !strings.Contains(m.Card, "选项一") || strings.Contains(m.Card, "完整补充") || strings.Contains(m.Card, "interaction_submit") || native.writes != 1 {
		t.Fatal("canonical result replaced by submitted draft or extra response", err)
	}
	stored, err := c.Store.InteractionAnswer(context.Background(), row.Key)
	if err != nil || !strings.Contains(stored, "选项一") {
		t.Fatal("actual answer not persisted", err)
	}
	offline := &interactions.Controller{Store: c.Store, HostID: "host", AppID: "app"}
	if err := offline.StageCards(context.Background(), "thread", nil); err != nil {
		t.Fatal(err)
	}
	after, err := c.Store.Mirror(context.Background(), "host", "thread", m.Key)
	if err != nil || after.Card != m.Card || reads != 1 {
		t.Fatal("offline projection lost effective receipt", err)
	}
}

func TestInteractiveAppFirstReceiptReadsActualAnswerWithoutCreatingInput(t *testing.T) {
	c, _, native, event := interactiveFixture(t)
	row := stageInteractive(t, c, event)
	resolved := codexclient.Event{Generation: 1, Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"thread","requestId":"request"}`)}
	if err := c.Observe(context.Background(), resolved); err != nil {
		t.Fatal(err)
	}
	c.ReadQuestionResult = func(context.Context, codexclient.InteractiveRequest) (json.RawMessage, error) {
		return json.RawMessage(`{"answers":{"question":{"answers":["App 内的合成回答"]}}}`), nil
	}
	if err := c.StageCards(context.Background(), "thread", nil); err != nil {
		t.Fatal(err)
	}
	row, _ = c.Store.Interaction(context.Background(), row.Key)
	m, err := c.Store.Mirror(context.Background(), "host", "thread", store.InteractionCardKey(row.Key))
	if err != nil || !strings.Contains(m.Card, "App 内的合成回答") || row.AttemptID != "" || row.SubmissionID != "" || native.writes != 0 {
		t.Fatal("App-first receipt created a response or input", err)
	}
}
func TestInteractiveClaimValidationAndSingleDispatch(t *testing.T) {
	c, _, n, e := interactiveFixture(t)
	row := stageInteractive(t, c, e)
	for _, bad := range []struct{ user, chat, message string }{{"intruder", "chat", "request-card"}, {"ou_user", "forwarded", "request-card"}, {"ou_user", "chat", "forwarded-card"}} {
		if _, err := claimInteractive(c, row, bad.user, bad.chat, bad.message); err == nil {
			t.Fatal("unauthorized or forwarded action accepted")
		}
	}
	if _, err := c.Claim(context.Background(), store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}, "ou_user", []string{"ou_user"}, "chat", "request-card", row.Token, row.Revision, "", map[string]any{}); err == nil {
		t.Fatal("incomplete answer accepted")
	}
	if _, err := claimInteractive(c, row, "ou_user", "chat", "request-card"); err != nil {
		t.Fatal(err)
	}
	if _, err := claimInteractive(c, row, "ou_user", "chat", "request-card"); err == nil {
		t.Fatal("double click accepted")
	}
	for i := 0; i < 3; i++ {
		if err := c.Tick(context.Background(), "thread"); err != nil {
			t.Fatal(err)
		}
	}
	if n.writes != 1 || !strings.Contains(string(n.result), "选项二") {
		t.Fatal("native response missing or repeated", n.writes)
	}
	row, _ = c.Store.Interaction(context.Background(), row.Key)
	a, _ := c.Store.InteractionAttempt(context.Background(), row.AttemptID)
	if a.State != "submitted" {
		t.Fatal("write incorrectly reported", a.State)
	}
	resolved := codexclient.Event{Generation: 1, Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"thread","requestId":"request"}`)}
	if err := c.Observe(context.Background(), resolved); err != nil {
		t.Fatal(err)
	}
	if err := c.Stage(context.Background(), "thread", nil); err != nil {
		t.Fatal(err)
	}
	m, _ := c.Store.Mirror(context.Background(), "host", "thread", store.InteractionCardKey(row.Key))
	if strings.Contains(m.Card, "interaction_submit") || !strings.Contains(m.Card, "请求已处理") {
		t.Fatal("resolved controls survived")
	}
}
func TestInteractiveAppClosureBeforePersistenceNeverReopens(t *testing.T) {
	c, _, n, e := interactiveFixture(t)
	ctx := context.Background()
	if err := c.Observe(ctx, e); err != nil {
		t.Fatal(err)
	}
	closed := codexclient.Event{Generation: 1, Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"thread","requestId":"request"}`)}
	c.Observe(ctx, closed)
	c.Observe(ctx, e)
	if err := c.Stage(ctx, "thread", nil); err != nil {
		t.Fatal(err)
	}
	rows, _ := c.Store.Interactions(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if len(rows) != 0 || n.writes != 0 {
		t.Fatal("late replay resurrected closed callback")
	}
}
func TestInteractiveUnknownRecoveryAndNewConnectionDoNotResend(t *testing.T) {
	c, _, n, e := interactiveFixture(t)
	row := stageInteractive(t, c, e)
	if _, err := claimInteractive(c, row, "ou_user", "chat", "request-card"); err != nil {
		t.Fatal(err)
	}
	n.err = &codexclient.UnknownError{Method: e.Method, Cause: errors.New("fixture")}
	if err := c.Tick(context.Background(), "thread"); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.RecoverInteractions(context.Background(), "host"); err != nil {
		t.Fatal(err)
	}
	next := &interactiveNative{connection: "next", generation: 2}
	c.Native = next
	e.Generation = 2
	if err := c.Observe(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := c.Stage(context.Background(), "thread", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Tick(context.Background(), "thread"); err != nil {
		t.Fatal(err)
	}
	if n.writes != 1 || next.writes != 0 {
		t.Fatal("unknown answer resent")
	}
	r, _ := c.Store.Interaction(context.Background(), row.Key)
	a, _ := c.Store.InteractionAttempt(context.Background(), r.AttemptID)
	if a.State != "unknown" || r.Token == row.Token {
		t.Fatal("unknown or stale token lost")
	}
}
func TestInteractiveDisabledCardCannotBeForged(t *testing.T) {
	c, _, _, e := interactiveFixture(t)
	e.Method = "item/commandExecution/requestApproval"
	var p map[string]any
	json.Unmarshal(e.Params, &p)
	p["command"] = strings.Repeat("long command ", 1000)
	e.Params, _ = json.Marshal(p)
	row := stageInteractive(t, c, e)
	if row.Operable {
		t.Fatal("truncated command actionable")
	}
	if _, err := c.Claim(context.Background(), store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}, "ou_user", []string{"ou_user"}, "chat", "request-card", row.Token, row.Revision, "accept", nil); err == nil {
		t.Fatal("forged callback on read-only card accepted")
	}
}

func TestFileInteractionShowsMoveTargetAndUnknownKindIsReadOnly(t *testing.T) {
	for _, test := range []struct {
		kind     string
		operable bool
	}{{`{"type":"update","move_path":"/workspace/moved.go"}`, true}, {`{"type":"unknown"}`, false}} {
		c, _, _, event := interactiveFixture(t)
		event.Method = "item/fileChange/requestApproval"
		ctx := context.Background()
		if err := c.Observe(ctx, event); err != nil {
			t.Fatal(err)
		}
		turns := []codexclient.Turn{{ID: "turn", Status: "inProgress", Items: []codexclient.Item{{ID: "item", Type: "fileChange", Changes: []codexclient.FileChange{{Path: "/workspace/file.go", Kind: json.RawMessage(test.kind), Diff: "+complete diff"}}}}}}
		if err := c.Stage(ctx, "thread", turns); err != nil {
			t.Fatal(err)
		}
		rows, err := c.Store.Interactions(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
		if err != nil || len(rows) != 1 || rows[0].Operable != test.operable {
			t.Fatal("unknown file operation became actionable", err)
		}
		m, err := c.Store.Mirror(ctx, "host", "thread", store.InteractionCardKey(rows[0].Key))
		if err != nil || test.operable && !strings.Contains(m.Card, "/workspace/moved.go") {
			t.Fatal("file approval hid its move target", err)
		}
	}
}
func TestInteractiveNativeWritesRespectVerificationGuard(t *testing.T) {
	c, _, n, e := interactiveFixture(t)
	row := stageInteractive(t, c, e)
	if _, err := claimInteractive(c, row, "ou_user", "chat", "request-card"); err != nil {
		t.Fatal(err)
	}
	c.BeginWrite = func(context.Context, string) (context.Context, func(), error) { return nil, nil, store.ErrIneligible }
	if err := c.Tick(context.Background(), "thread"); err == nil || n.writes != 0 {
		t.Fatal("expired fixture guard bypassed")
	}
}

func TestInteractiveDisabledRequestTypeNeverClaimsOrSends(t *testing.T) {
	c, _, n, e := interactiveFixture(t)
	row := stageInteractive(t, c, e)
	if _, err := claimInteractive(c, row, "ou_user", "chat", "request-card"); err != nil {
		t.Fatal(err)
	}
	c.EnabledTypes = []string{"command"}
	if err := c.Tick(context.Background(), "thread"); err != nil || n.writes != 0 {
		t.Fatal("disabled request type sent a prior ready answer", err)
	}
	if _, err := claimInteractive(c, row, "ou_user", "chat", "request-card"); err == nil {
		t.Fatal("disabled question accepted a forged callback")
	}
}

func TestNativeReadyAnswerDoesNotPretendToSendAfterReconnect(t *testing.T) {
	c, _, _, e := interactiveFixture(t)
	row := stageInteractive(t, c, e)
	if _, err := claimInteractive(c, row, "ou_user", "chat", "request-card"); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.RecoverInteractions(context.Background(), "host"); err != nil {
		t.Fatal(err)
	}
	next := &interactiveNative{connection: "next", generation: 2}
	c.Native, e.Generation = next, 2
	if err := c.Observe(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := c.Stage(context.Background(), "thread", []codexclient.Turn{{ID: "turn", Status: "inProgress"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Tick(context.Background(), "thread"); err != nil || next.writes != 0 {
		t.Fatal("ready answer was automatically rebound and sent", err)
	}
	m, err := c.Store.Mirror(context.Background(), "host", "thread", store.InteractionCardKey(row.Key))
	if err != nil || strings.Contains(m.Card, "正在提交") || !strings.Contains(m.Card, "上次操作尚未发送") {
		t.Fatal("unsent old intent claimed to be submitting", err)
	}
}
func TestInteractiveAsyncAnswerUsesDurableQueueAndNoCallback(t *testing.T) {
	c, _, n, _ := interactiveFixture(t)
	ctx := context.Background()
	at := c.Now().Unix()
	delivery := "async"
	phase := "final_answer"
	turns := []codexclient.Turn{{ID: "turn", Status: "completed", StartedAt: &at, Items: []codexclient.Item{{ID: "async-call", Type: "agentMessage", Text: "choose", Delivery: &delivery, Phase: &phase, Questions: []codexclient.AsyncQuestion{{Title: "选哪个？", Options: []string{"一", "二"}}}}}}}
	if err := c.Stage(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	rows, _ := c.Store.Interactions(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if len(rows) != 1 {
		t.Fatal("async missing")
	}
	row := rows[0]
	c.Store.DB.ExecContext(ctx, `UPDATE mirrors SET remote_id='request-card' WHERE mirror_key=?`, store.InteractionCardKey(row.Key))
	if _, err := claimInteractive(c, row, "ou_user", "chat", "request-card"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := c.Tick(ctx, "thread"); err != nil {
			t.Fatal(err)
		}
	}
	if n.writes != 0 {
		t.Fatal("async answered via callback")
	}
	receipts, err := c.Store.ReceiptsForDeployment(ctx, "host", "thread")
	if err != nil || len(receipts) != 1 || !strings.Contains(receipts[0].Body, "send_user_message_question_reply") || !strings.Contains(receipts[0].Body, "二") {
		t.Fatal("async answer did not become one durable input", err)
	}
}

func TestAsyncQuestionClosureUsesExactNativeAnswerEnvelope(t *testing.T) {
	for _, test := range []struct {
		name, callID, prompt, prefix, want string
		all                                bool
	}{
		{"complete", "async-call", "选哪个？", "", "resolved", true},
		{"partial", "async-call", "选哪个？", "", "unsupported", false},
		{"foreign-item", "other-call", "选哪个？", "", "pending", true},
		{"wrong-question", "async-call", "another prompt", "", "pending", false},
		{"ordinary-quoted-text", "async-call", "选哪个？", "引用示例：", "pending", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _, n, _ := interactiveFixture(t)
			ctx := context.Background()
			at, delivery := c.Now().Unix(), "async"
			turns := []codexclient.Turn{{ID: "turn", Status: "completed", StartedAt: &at, Items: []codexclient.Item{{ID: "async-call", Type: "agentMessage", Delivery: &delivery, Questions: []codexclient.AsyncQuestion{{Title: "选哪个？"}, {Title: "还有什么？"}}}}}}
			if err := c.Stage(ctx, "thread", turns); err != nil {
				t.Fatal(err)
			}
			answers := []map[string]string{}
			for i, question := range []string{test.prompt, "还有什么？"} {
				if i == 1 && !test.all {
					break
				}
				identity, _ := json.Marshal([]any{"request_user_input_async", test.callID, i})
				answers = append(answers, map[string]string{"questionItemId": string(identity), "question": question, "answer": "合成回答"})
			}
			encoded, _ := json.Marshal(answers)
			input, _ := json.Marshal(map[string]string{"type": "text", "text": test.prefix + "<send_user_message_question_reply>\n" + string(encoded) + "\n</send_user_message_question_reply>"})
			turns = append(turns, codexclient.Turn{ID: "answer-turn", Status: "completed", Items: []codexclient.Item{{ID: "app-answer", Type: "userMessage", Content: []json.RawMessage{input}}}})
			if err := c.StageCards(ctx, "thread", turns); err != nil {
				t.Fatal(err)
			}
			rows, _ := c.Store.Interactions(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
			if len(rows) != 1 || rows[0].State != test.want {
				t.Fatal("answer closure did not match the exact original questions")
			}
			if err := c.Tick(ctx, "thread"); err != nil || n.writes != 0 {
				t.Fatal("App answer produced a second native write", err)
			}
		})
	}
}
func TestInteractionCardsStillDeliverWithMentionsDisabled(t *testing.T) {
	c, _, _, e := interactiveFixture(t)
	c.Options.Enabled = false
	row := stageInteractive(t, c, e)
	m, err := c.Store.Mirror(context.Background(), "host", "thread", store.InteractionCardKey(row.Key))
	if err != nil || !strings.Contains(m.Card, "interaction_submit") {
		t.Fatal("off hid card", err)
	}
	// The generic outbox still replies to the original human message.
	a, claimed, err := c.Store.ClaimMirror(context.Background(), "host", "thread", m.Key, m.Revision, c.Now().UnixMilli())
	if err != nil || !claimed || a.ParentMessageID != "original-input" {
		t.Fatal("request lost its reply parent", err)
	}
	var v map[string]any
	if json.Unmarshal([]byte(a.Card), &v) != nil {
		t.Fatal("invalid card")
	}
	_ = cards.DefaultMaxCardBytes
}
