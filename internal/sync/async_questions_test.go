package sync

import (
	"context"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func TestAsyncQuestionProjectionKeepsOldHistoryAndDoesNotInventFinal(t *testing.T) {
	ctx := context.Background()
	e, _ := engineFixture(t)
	e.SuppressAsyncQuestions, e.AsyncQuestionsSinceMS = true, 150000
	delivery := "async"
	oldAt, newAt := int64(120), int64(160)
	question := agent("question", "ASYNC_QUESTION_BODY", "final_answer")
	question.Delivery = &delivery
	question.Questions = []codexclient.AsyncQuestion{{Title: "选哪一个？", Options: []string{"A", "B"}}}
	old := codexclient.Turn{ID: "old", Status: "completed", StartedAt: &oldAt, Items: []codexclient.Item{input("old-input", "old input"), question}}
	r, _, err := e.Store.Claim(ctx, store.Receipt{AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: "feishu-new", Body: "new input"})
	if err != nil {
		t.Fatal(err)
	}
	u := input("new-input", "new input")
	u.ClientID = &r.ClientID
	current := codexclient.Turn{ID: "new", Status: "completed", StartedAt: &newAt, Items: []codexclient.Item{u, question}}
	apply(t, e, 200000, false, old, current)
	mirrors, err := e.Store.Mirrors(ctx, "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	oldFinal := false
	for _, m := range mirrors {
		if m.TurnID == "new" {
			t.Fatal("async-only new turn mirrored a question or fabricated an empty final")
		}
		oldFinal = oldFinal || m.Kind == "final" && strings.Contains(m.Card, "ASYNC_QUESTION_BODY")
	}
	if !oldFinal {
		t.Fatal("unrecorded historical async question was silently hidden")
	}
	links, err := e.Store.InputLinks(ctx, "host", "thread")
	if err != nil || len(links) != 1 || links[0].ReceiptID != r.ID || links[0].TurnID != "new" {
		t.Fatal("suppression discarded the exact originating Feishu input link", err)
	}
	current.Items = append(current.Items, agent("answer", "NORMAL_FINAL", "final_answer"))
	apply(t, e, 201000, false, old, current)
	final, err := e.Store.Mirror(ctx, "host", "thread", mirrorKey("final", "new", "0"))
	if err != nil || !strings.Contains(final.Card, "NORMAL_FINAL") || strings.Contains(final.Card, "ASYNC_QUESTION_BODY") || final.ParentMessageID != "feishu-new" {
		t.Fatal("question suppression lost or duplicated the real final result", err)
	}
}

func TestAsyncQuestionSuppressionStillReportsFailureAndRejectsConflicts(t *testing.T) {
	for _, status := range []string{"failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			e, _ := engineFixture(t)
			e.SuppressAsyncQuestions, e.AsyncQuestionsSinceMS = true, 150000
			delivery, at := "async", int64(160)
			q := agent("question", "ASYNC_QUESTION", "final_answer")
			q.Delivery, q.Questions = &delivery, []codexclient.AsyncQuestion{{Title: "请选择"}}
			turn := codexclient.Turn{ID: "turn", Status: status, StartedAt: &at, Items: []codexclient.Item{input("input", "original"), q}}
			apply(t, e, 200000, false, turn)
			m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
			if err != nil || !strings.Contains(m.Card, emptyResult(status)) || strings.Contains(m.Card, "ASYNC_QUESTION") {
				t.Fatal("async question masked the terminal failure/stop", err)
			}
			conflict := q
			conflict.Text = "contradictory question"
			turn.Items = append(turn.Items, conflict)
			if err := e.Apply(context.Background(), "thread", []codexclient.Turn{turn}, false, 201000); err == nil {
				t.Fatal("suppression hid a conflicting duplicate native question")
			}
			mark, _ := e.Store.Watermark(context.Background(), "host", "thread")
			if mark.ObservedAtMS != 200000 {
				t.Fatal("invalid async snapshot advanced the checkpoint")
			}
		})
	}
}
