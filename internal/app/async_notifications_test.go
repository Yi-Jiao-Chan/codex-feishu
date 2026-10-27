package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/interactions"
	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

func asyncNotificationHistory(c *interactions.Controller) []codexclient.Turn {
	at, delivery := c.Now().Unix(), "async"
	user, _ := json.Marshal(map[string]string{"type": "text", "text": "合成任务"})
	return []codexclient.Turn{{ID: "turn", Status: "inProgress", StartedAt: &at, Items: []codexclient.Item{
		{ID: "input", Type: "userMessage", Content: []json.RawMessage{user}},
		{ID: "async-notification", Type: "agentMessage", Delivery: &delivery, Questions: []codexclient.AsyncQuestion{{Title: "选择如何继续？", Options: []string{"继续", "暂停"}}}},
	}}}
}

func appendAsyncNotificationAnswer(turns []codexclient.Turn) []codexclient.Turn {
	identity, _ := json.Marshal([]any{"request_user_input_async", "async-notification", 0})
	replies, _ := json.Marshal([]map[string]string{{"questionItemId": string(identity), "question": "选择如何继续？", "answer": "合成回答"}})
	text, _ := json.Marshal(map[string]string{"type": "text", "text": "<send_user_message_question_reply>\n" + string(replies) + "\n</send_user_message_question_reply>"})
	turns[0].Items = append(turns[0].Items, codexclient.Item{ID: "app-answer", Type: "userMessage", Content: []json.RawMessage{text}})
	return turns
}

func TestAsyncNotificationWithoutFeishuAnsweringMentionsOnceAndUpdatesReceipt(t *testing.T) {
	c, _, native, _ := interactiveFixture(t)
	c.EnabledTypes = []string{"command", "question"}
	ctx := context.Background()
	turns := asyncNotificationHistory(c)
	if err := c.Stage(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Store.Interactions(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if err != nil || len(rows) != 1 || rows[0].Operable {
		t.Fatal("disabled answering hid the question or enabled its controls", err)
	}
	row := rows[0]
	key := store.InteractionCardKey(row.Key)
	m, err := c.Store.Mirror(ctx, "host", "thread", key)
	if err != nil || !strings.Contains(m.Card, "选择如何继续？") || !strings.Contains(m.Card, "请在 Codex App 中回答") || strings.Contains(m.Card, "interaction_submit") {
		t.Fatal("async question was not a useful read-only card", err)
	}
	decisions, err := c.Store.MentionDecisions(ctx, "host", "thread", key)
	if err != nil || len(decisions) != 1 || decisions[0].State != "eligible" {
		t.Fatal("fresh async question did not schedule needs_user", err)
	}
	options := c.Options
	options.ChatID, options.MemberIDs = "chat", []string{"ou_user"}
	a, claimed, err := c.Store.ClaimMirrorWithMentions(ctx, "host", "thread", key, m.Revision, c.Now().UnixMilli(), options)
	if err != nil || !claimed || !strings.Contains(a.Card, "ou_user") {
		t.Fatal("first async question delivery did not carry its allowed @", err)
	}
	if err := c.Store.FinishMirrorAttempt(ctx, a.ID, "confirmed", "question-card"); err != nil {
		t.Fatal(err)
	}
	turns = appendAsyncNotificationAnswer(turns)
	for i := 0; i < 2; i++ {
		if err := c.Stage(ctx, "thread", turns); err != nil {
			t.Fatal(err)
		}
	}
	updated, err := c.Store.Mirror(ctx, "host", "thread", key)
	if err != nil || updated.RemoteID != "question-card" || !strings.Contains(updated.Card, "已回答") || !strings.Contains(updated.Card, "合成回答") || strings.Contains(updated.Card, "interaction_submit") {
		t.Fatal("App answer did not update the original compact receipt", err)
	}
	if err := c.Tick(ctx, "thread"); err != nil || native.writes != 0 {
		t.Fatal("observing an App answer wrote another native response", err)
	}
	a, claimed, err = c.Store.ClaimMirrorWithMentions(ctx, "host", "thread", key, updated.Revision, c.Now().UnixMilli(), options)
	if err != nil || !claimed || a.Operation != "update" || a.TargetMessageID != "question-card" || strings.Contains(a.Card, "ou_user") {
		t.Fatal("answer update created a second question or repeated @", err)
	}
	if err := c.Store.FinishMirrorAttempt(ctx, a.ID, "confirmed", "question-card"); err != nil {
		t.Fatal(err)
	}
	offline := &interactions.Controller{Store: c.Store, HostID: "host", AppID: "app"}
	if err := offline.StageCards(ctx, "thread", nil); err != nil {
		t.Fatal(err)
	}
	after, err := c.Store.Mirror(ctx, "host", "thread", key)
	if err != nil || after.Revision != updated.Revision || after.Card != "" || after.CardHash != updated.CardHash {
		t.Fatal("offline projection lost the recorded async answer", err)
	}
	decisions, err = c.Store.MentionDecisions(ctx, "host", "thread", key)
	if err != nil || len(decisions) != 1 || decisions[0].State != "confirmed" {
		t.Fatal("answer or replay changed the original reminder decision", err)
	}
}

func TestAsyncNotificationInTurnPredatingBridgeUsesObservedItem(t *testing.T) {
	c, _, _, _ := interactiveFixture(t)
	c.EnabledTypes = []string{"command", "question"}
	ctx := context.Background()
	turns := asyncNotificationHistory(c)
	oldAt := c.StartedAtMS/1000 - 100
	turns[0].StartedAt = &oldAt
	if err := c.Prepare(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	rows, _ := c.Store.Interactions(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if len(rows) != 0 {
		t.Fatal("unobserved historical question was treated as fresh")
	}
	params, _ := json.Marshal(map[string]any{"threadId": "thread", "turnId": "turn", "item": turns[0].Items[1]})
	if err := c.Observe(ctx, codexclient.Event{Generation: 1, Method: "item/completed", Params: params}); err != nil {
		t.Fatal(err)
	}
	if err := c.Prepare(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	rows, _ = c.Store.Interactions(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if len(rows) != 1 || rows[0].Replay || rows[0].EventAt != c.Now().Unix() {
		t.Fatal("new question in an older turn was lost")
	}
	turns[0].Status = "completed"
	engine := messages.Engine{Store: c.Store, HostID: "host", SuppressAsyncQuestions: true, AsyncQuestionsSinceMS: c.StartedAtMS}
	if err := engine.Apply(ctx, "thread", turns, false, c.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := c.StageCards(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	var finals int
	if err := c.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM mirrors WHERE kind='final'`).Scan(&finals); err != nil || finals != 0 {
		t.Fatal("async question was also sent as a final answer", err)
	}
	decisions, err := c.Store.MentionDecisions(ctx, "host", "thread", store.InteractionCardKey(rows[0].Key))
	if err != nil || len(decisions) != 1 || decisions[0].State != "eligible" {
		t.Fatal("observed fresh question in an older turn did not notify", err)
	}
}

func TestAsyncNotificationAlreadyAnsweredBeforeFirstObservationDoesNotPing(t *testing.T) {
	c, _, _, _ := interactiveFixture(t)
	c.EnabledTypes = []string{"command", "question"}
	ctx := context.Background()
	turns := appendAsyncNotificationAnswer(asyncNotificationHistory(c))
	if err := c.Stage(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Store.Interactions(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if err != nil || len(rows) != 1 || rows[0].State != "resolved" {
		t.Fatal("complete native answer did not close the async request", err)
	}
	_, err = c.Store.Mirror(ctx, "host", "thread", store.InteractionCardKey(rows[0].Key))
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("already answered unseen question created a late card and reminder", err)
	}
	if _, err := c.Store.InteractionAnswer(ctx, rows[0].Key); err != nil {
		t.Fatal("actual native answer was not retained", err)
	}
}
