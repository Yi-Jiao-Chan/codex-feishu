package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/interactions"
	"codex-feishu/internal/store"
)

func asyncAnswerSubmissionFixture(t *testing.T, queued bool) (*interactions.Controller, store.InteractionRequest, store.Submission, []codexclient.Turn) {
	t.Helper()
	c, _, _, _ := interactiveFixture(t)
	ctx := context.Background()
	turns := asyncNotificationHistory(c)
	if err := c.Stage(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	rows, err := c.Store.Interactions(ctx, scope)
	if err != nil || len(rows) != 1 {
		t.Fatal("question missing", err)
	}
	row := rows[0]
	m, err := c.Store.Mirror(ctx, "host", "thread", store.InteractionCardKey(row.Key))
	if err != nil {
		t.Fatal(err)
	}
	a, claimed, err := c.Store.ClaimMirror(ctx, "host", "thread", m.Key, m.Revision, c.Now().UnixMilli())
	if err != nil || !claimed || a.Operation != "reply" {
		t.Fatal("initial question lost reply", err)
	}
	if err = c.Store.FinishMirrorAttempt(ctx, a.ID, "confirmed", "request-card"); err != nil {
		t.Fatal(err)
	}
	if _, err = claimInteractive(c, row, "ou_user", "chat", "request-card"); err != nil {
		t.Fatal(err)
	}
	if err = c.Tick(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	if err = c.Store.StageSubmissions(ctx, scope); err != nil {
		t.Fatal(err)
	}
	row, err = c.Store.Interaction(ctx, row.Key)
	if err != nil {
		t.Fatal(err)
	}
	if queued {
		if err = c.Store.ObserveSubmission(ctx, scope, row.SubmissionID, "queued", "async-answer-queue", 2, "", ""); err != nil {
			t.Fatal(err)
		}
		if err = c.Store.StageTaskControls(ctx, scope, []store.TaskControl{{TurnID: "active-turn", NativeStatus: "inProgress", Kind: "stop", State: "available", Fingerprint: "active"}}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := c.Store.Submission(ctx, scope, row.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	return c, row, n, turns
}

func TestAsyncAnswerQueueUpdatesOriginalQuestionAndAuthenticatesActions(t *testing.T) {
	for _, action := range []string{"steer", "remove"} {
		t.Run(action, func(t *testing.T) {
			c, row, n, turns := asyncAnswerSubmissionFixture(t, true)
			ctx := context.Background()
			scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
			before, err := c.Store.Mirror(ctx, "host", "thread", store.InteractionCardKey(row.Key))
			if err != nil {
				t.Fatal(err)
			}
			if err = c.StageCards(ctx, "thread", turns); err != nil {
				t.Fatal(err)
			}
			feedback := Feedback{Store: c.Store, HostID: "host", AppID: "app"}
			if err = feedback.Stage(ctx, "thread"); err != nil {
				t.Fatal(err)
			}
			m, err := c.Store.SubmissionCard(ctx, scope, n.ID)
			if err != nil || m.Key != before.Key || m.RemoteID != before.RemoteID || m.ParentKey != before.ParentKey || m.ParentMessageID != before.ParentMessageID || !strings.Contains(m.Card, "第 2 条") || !strings.Contains(m.Card, "追加引导") || !strings.Contains(m.Card, "移除") || !strings.Contains(m.Card, n.ActionToken) || strings.Contains(m.Card, "interaction_submit") {
				t.Fatal("queue lost question identity or controls", err)
			}
			if _, err = c.Store.Mirror(ctx, "host", "thread", store.QueueCardKey(n.ID)); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("separate queue card created", err)
			}
			a, claimed, err := c.Store.ClaimMirror(ctx, "host", "thread", m.Key, m.Revision, c.Now().UnixMilli()+5000)
			if err != nil || !claimed || a.Operation != "update" || a.TargetMessageID != "request-card" {
				t.Fatal("queue created new message", err)
			}
			if err = c.Store.FinishMirrorAttempt(ctx, a.ID, "confirmed", "request-card"); err != nil {
				t.Fatal(err)
			}
			r := &Receiver{Store: c.Store}
			r.Config.Host.ID, r.Config.Feishu.AppID = "host", "app"
			r.Config.Access.OperatorOpenIDs = []string{"ou_user"}
			for _, message := range []string{"foreign-card", "request-card", "request-card"} {
				e := receiverAction(t, "ou_user", message, n.ActionToken, n.QueueID, n.Revision)
				e.Event.Action.Value["action"] = action
				if _, err = r.Action(ctx, e); err != nil {
					t.Fatal(err)
				}
				actions, err := c.Store.SubmissionActions(ctx, scope)
				want := 1
				if message == "foreign-card" {
					want = 0
				}
				if err != nil || len(actions) != want || want == 1 && (actions[0].Kind != action || actions[0].QueueID != n.QueueID) {
					t.Fatal("callback lost identity or repeated action", err)
				}
			}
			if err = c.Store.StageInteractionSubmissionCard(ctx, row, m.Card, false, n.Revision); !errors.Is(err, store.ErrConflict) {
				t.Fatal("stale queue projection accepted", err)
			}
		})
	}
}

func TestAsyncAnswerQueueOfflineAndRemovedKeepOriginalQuestion(t *testing.T) {
	c, row, n, turns := asyncAnswerSubmissionFixture(t, true)
	ctx := context.Background()
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	c.Native = nil
	if err := c.StageCards(ctx, "thread", turns); err != nil {
		t.Fatal("offline interaction", err)
	}
	f := Feedback{Store: c.Store, HostID: "host", AppID: "app"}
	if err := f.StageReceived(ctx, "thread"); err != nil {
		t.Fatal("offline receipts", err)
	}
	if err := c.Store.StageDisconnectedCards(ctx, scope, "offline"); err != nil {
		t.Fatal("offline overlay", err)
	}
	m, err := c.Store.SubmissionCard(ctx, scope, n.ID)
	if err != nil || m.RemoteID != "request-card" || !strings.Contains(m.Card, "选择如何继续？") || strings.Contains(m.Card, "callback") {
		t.Fatal("offline question lost detail or kept controls", err)
	}
	op, err := c.Store.ClaimSubmissionAction(ctx, scope, n.ID, "remove", n.QueueID, n.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Store.PrepareSubmissionAction(ctx, scope, op.ID, "delete", "", n.QueueID, n.ClientID); err != nil {
		t.Fatal(err)
	}
	attempt, claimed, err := c.Store.ClaimSubmissionAttempt(ctx, scope, n.ID, op.ID, "delete")
	if err != nil || !claimed {
		t.Fatal("delete attempt missing", err)
	}
	if err = c.Store.FinishSubmissionAttempt(ctx, scope, attempt.ID, "confirmed", "", 1); err != nil {
		t.Fatal(err)
	}
	if err = c.StageCards(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	m, err = c.Store.Mirror(ctx, "host", "thread", store.InteractionCardKey(row.Key))
	if err != nil || !strings.Contains(m.Card, "已移除") || strings.Contains(m.Card, "提交失败") || strings.Contains(m.Card, "callback") {
		t.Fatal("removed answer appears failed or actionable", err)
	}
}

func TestAsyncAnswerConsumedHistoryShowsVerifiedReceipt(t *testing.T) {
	c, row, n, turns := asyncAnswerSubmissionFixture(t, true)
	ctx := context.Background()
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	content, err := json.Marshal(map[string]string{"type": "text", "text": n.Body})
	if err != nil {
		t.Fatal(err)
	}
	turns = append(turns, codexclient.Turn{ID: "answer-turn", Status: "completed", Items: []codexclient.Item{{ID: "answer", Type: "userMessage", ClientID: &n.ClientID, Content: []json.RawMessage{content}}}})
	if err = c.Store.ObserveSubmission(ctx, scope, n.ID, "consumed", n.QueueID, 0, "answer-turn", "answer"); err != nil {
		t.Fatal(err)
	}
	if err = c.StageCards(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	m, err := c.Store.Mirror(ctx, "host", "thread", store.InteractionCardKey(row.Key))
	if err != nil || !strings.Contains(m.Card, "已回答") || strings.Contains(m.Card, "callback") {
		t.Fatal("verified answer did not close original question", err)
	}
}

func TestAsyncAnswerLegacyUnknownQueueJournalRemainsAuthoritative(t *testing.T) {
	c, row, n, turns := asyncAnswerSubmissionFixture(t, true)
	ctx := context.Background()
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	key := store.QueueCardKey(n.ID)
	card := cards.QueueInputView(cards.QueueView{State: "queued", Token: n.ActionToken, QueueID: n.QueueID, Revision: n.Revision})
	if _, err := c.Store.DB.ExecContext(ctx, `INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json,parent_message_id) VALUES(?,?,?,?,'progress',?,?)`, "host", "thread", key, key, card, n.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.DB.ExecContext(ctx, `INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,?,'ready')`, "host", "thread", key); err != nil {
		t.Fatal(err)
	}
	m, err := c.Store.Mirror(ctx, "host", "thread", key)
	if err != nil {
		t.Fatal(err)
	}
	a, claimed, err := c.Store.ClaimMirror(ctx, "host", "thread", key, m.Revision, c.Now().UnixMilli())
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err = c.Store.FinishMirrorAttempt(ctx, a.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	if err = c.StageCards(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	m, err = c.Store.SubmissionCard(ctx, scope, n.ID)
	if err != nil || m.Key != key || m.State != "unknown" {
		t.Fatal("unknown legacy delivery replaced", err)
	}
	question, err := c.Store.Mirror(ctx, "host", "thread", store.InteractionCardKey(row.Key))
	if err != nil || strings.Contains(question.Card, "callback") {
		t.Fatal("legacy answer gained second queue controls", err)
	}
}

func TestAsyncAnswerSavedOfflineCanCancelOriginalQuestion(t *testing.T) {
	c, row, n, turns := asyncAnswerSubmissionFixture(t, false)
	ctx := context.Background()
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	c.Native = nil
	if err := c.StageCards(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	f := Feedback{Store: c.Store, HostID: "host", AppID: "app"}
	if err := f.StageReceived(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	m, err := c.Store.SubmissionCard(ctx, scope, n.ID)
	if err != nil || m.RemoteID != "request-card" || !strings.Contains(m.Card, "cancel_pending") || !strings.Contains(m.Card, n.ActionToken) {
		t.Fatal("saved offline answer cannot be cancelled", err)
	}
	r := &Receiver{Store: c.Store}
	r.Config.Host.ID, r.Config.Feishu.AppID = "host", "app"
	r.Config.Access.OperatorOpenIDs = []string{"ou_user"}
	e := receiverAction(t, "ou_user", m.RemoteID, n.ActionToken, "", n.Revision)
	e.Event.Action.Value["action"] = "cancel_pending"
	for i := 0; i < 2; i++ {
		if _, err = r.Action(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	n, err = c.Store.Submission(ctx, scope, n.ID)
	if err != nil || n.State != "removed" {
		t.Fatal("offline cancellation was not persisted", err)
	}
	if err = c.StageCards(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	m, err = c.Store.Mirror(ctx, "host", "thread", store.InteractionCardKey(row.Key))
	if err != nil || m.RemoteID != "request-card" || !strings.Contains(m.Card, "已移除") || strings.Contains(m.Card, "callback") {
		t.Fatal("cancellation lost original card or kept controls", err)
	}
}

func TestAsyncAnswerAppAnswerRetainsRemovalOfQueuedFeishuAnswer(t *testing.T) {
	c, row, n, turns := asyncAnswerSubmissionFixture(t, true)
	ctx := context.Background()
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	// A second client answers before the already-saved Feishu answer is consumed.
	turns = appendAsyncNotificationAnswer(turns)
	if err := c.StageCards(ctx, "thread", turns); err != nil {
		t.Fatal(err)
	}
	m, err := c.Store.SubmissionCard(ctx, scope, n.ID)
	if err != nil || !strings.Contains(m.Card, "已回答") || !strings.Contains(m.Card, "仍在队列") || !strings.Contains(m.Card, "移除") || !strings.Contains(m.Card, n.ActionToken) {
		t.Fatal("App answer hid the still-queued Feishu answer", err)
	}
	row, err = c.Store.Interaction(ctx, row.Key)
	if err != nil || row.State != "resolved" {
		t.Fatal("App answer was not recorded", err)
	}
}
