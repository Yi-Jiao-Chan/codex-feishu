package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/interactions"
	"codex-feishu/internal/store"
)

func asyncAppendQuestionFixture(t *testing.T) (*interactions.Controller, store.SubmissionScope, store.InteractionRequest) {
	t.Helper()
	c, _, _, _ := interactiveFixture(t)
	ctx := context.Background()
	if err := c.Stage(ctx, "thread", asyncNotificationHistory(c)); err != nil {
		t.Fatal(err)
	}
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	rows, err := c.Store.Interactions(ctx, scope)
	if err != nil || len(rows) != 1 {
		t.Fatal("async question missing", err)
	}
	row := rows[0]
	if _, err = c.Store.DB.ExecContext(ctx, `UPDATE mirrors SET remote_id='request-card',delivered_revision=revision WHERE mirror_key=?`, store.InteractionCardKey(row.Key)); err != nil {
		t.Fatal(err)
	}
	return c, scope, row
}

func claimAsyncAppend(c *interactions.Controller, scope store.SubmissionScope, row store.InteractionRequest) (string, error) {
	return c.Claim(context.Background(), scope, "ou_user", []string{"ou_user"}, "chat", "request-card", row.Token, row.Revision, "append_answer", map[string]any{"q_0": "1", "q_0_notes": "合成补充"})
}

func TestAsyncAppendClaimFreezesAnswerAndExplicitDeliveryAtomically(t *testing.T) {
	c, scope, row := asyncAppendQuestionFixture(t)
	ctx := context.Background()
	thread, err := claimAsyncAppend(c, scope, row)
	if err != nil || thread != "thread" {
		t.Fatal("explicit answer rejected", err)
	}
	row, err = c.Store.Interaction(ctx, row.Key)
	if err != nil || row.SubmissionID == "" || row.AttemptID == "" {
		t.Fatal("answer intent was not durable at callback return", err)
	}
	n, err := c.Store.Submission(ctx, scope, row.SubmissionID)
	if err != nil || n.State != "steering" {
		t.Fatal("answer did not retain append intent", err)
	}
	answers, ok := codexclient.ParseAsyncQuestionReplies(n.Body)
	if !ok || len(answers) != 1 || answers[0].CallID != row.ItemID || answers[0].QuestionIndex != 0 || !strings.Contains(answers[0].Answer, "合成补充") {
		t.Fatal("answer lost original question identity or text")
	}
	actions, err := c.Store.SubmissionActions(ctx, scope)
	if err != nil || len(actions) != 1 || actions[0].Kind != "steer" || actions[0].Stage != "prepare" || actions[0].State != "ready" || actions[0].QueueID != "" || actions[0].ClientID != n.ClientID || actions[0].Token != n.ActionToken {
		t.Fatal("callback did not create exactly one initial append action", err)
	}
	for i := 0; i < 2; i++ {
		if err = c.Tick(ctx, "thread"); err != nil {
			t.Fatal(err)
		}
	}
	receipts, err := c.Store.ReceiptsForDeployment(ctx, "host", "thread")
	if err != nil || len(receipts) != 1 {
		t.Fatal("actor duplicated an already-claimed answer", err)
	}
	if _, err = claimAsyncAppend(c, scope, row); !errors.Is(err, store.ErrSubmissionBusy) {
		t.Fatal("double answer claimed another intent", err)
	}
	native := c.Native.(*interactiveNative)
	if native.writes != 0 {
		t.Fatal("async answer used a synchronous server callback")
	}
}

func TestAsyncAppendCallbackKeepsLegacyAnswersQueued(t *testing.T) {
	c, scope, row := asyncAppendQuestionFixture(t)
	ctx := context.Background()
	if _, err := c.Claim(ctx, scope, "ou_user", []string{"ou_user"}, "chat", "request-card", row.Token, row.Revision, "", map[string]any{"q_0": "0"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Tick(ctx, "thread"); err != nil {
		t.Fatal(err)
	}
	row, err := c.Store.Interaction(ctx, row.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Store.StageSubmissions(ctx, scope); err != nil {
		t.Fatal(err)
	}
	n, err := c.Store.Submission(ctx, scope, row.SubmissionID)
	if err != nil || n.State != "received" {
		t.Fatal("legacy callback was silently upgraded to append", err)
	}
	actions, err := c.Store.SubmissionActions(ctx, scope)
	if err != nil || len(actions) != 0 {
		t.Fatal("legacy answer gained a new append action", err)
	}
}

func TestAsyncAppendClaimRollbackLeavesQuestionAnswerable(t *testing.T) {
	c, scope, row := asyncAppendQuestionFixture(t)
	ctx := context.Background()
	if _, err := c.Store.DB.ExecContext(ctx, `CREATE TRIGGER fail_append BEFORE INSERT ON submission_actions BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := claimAsyncAppend(c, scope, row); err == nil {
		t.Fatal("failed append journal was accepted")
	}
	after, err := c.Store.Interaction(ctx, row.Key)
	if err != nil || after.AttemptID != "" || after.SubmissionID != "" || after.Revision != row.Revision {
		t.Fatal("partial claim survived failed action persistence", err)
	}
	for _, table := range []string{"interaction_attempts", "m0_receipts", "submission_inputs", "submission_actions", "submission_order"} {
		var count int
		if err = c.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatal("failed callback left durable partial work", table, err)
		}
	}
}

func TestAsyncAppendClaimPreservesEarlierUnadoptedReceiptOrder(t *testing.T) {
	c, scope, row := asyncAppendQuestionFixture(t)
	ctx := context.Background()
	earlier, _, err := c.Store.Claim(ctx, store.Receipt{AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: "earlier-text", Body: "合成普通消息"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = claimAsyncAppend(c, scope, row); err != nil {
		t.Fatal(err)
	}
	if err = c.Store.StageSubmissions(ctx, scope); err != nil {
		t.Fatal(err)
	}
	inputs, err := c.Store.Submissions(ctx, scope)
	if err != nil || len(inputs) != 2 || inputs[0].ID != earlier.ID || inputs[0].State != "received" || inputs[1].State != "steering" {
		t.Fatal("answer insertion moved ahead of an older unadopted input", err)
	}
}

func TestAsyncAppendDecisionCannotSteerSynchronousQuestion(t *testing.T) {
	c, _, _, event := interactiveFixture(t)
	row := stageInteractive(t, c, event)
	if _, err := claimAsyncAppend(c, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}, row); !errors.Is(err, codexclient.ErrInvalidAnswer) {
		t.Fatal("native question accepted an async append decision", err)
	}
}

func TestAsyncAppendConcurrentCallbacksClaimOneIntent(t *testing.T) {
	c, scope, row := asyncAppendQuestionFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; _, err := claimAsyncAppend(c, scope, row); results <- err }()
	}
	close(start)
	succeeded := 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			succeeded++
		} else if !errors.Is(err, store.ErrSubmissionStale) && !errors.Is(err, store.ErrSubmissionBusy) {
			t.Fatal("unexpected callback failure", err)
		}
	}
	ctx := context.Background()
	actions, err := c.Store.SubmissionActions(ctx, scope)
	if err != nil || succeeded != 1 || len(actions) != 1 {
		t.Fatal("concurrent callbacks created more than one append intent", err)
	}
	receipts, err := c.Store.ReceiptsForDeployment(ctx, "host", "thread")
	if err != nil || len(receipts) != 1 || actions[0].ReceiptID != receipts[0].ID {
		t.Fatal("concurrent claim split answer/action ownership", err)
	}
}
