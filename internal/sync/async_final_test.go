package sync

import (
	"context"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
)

func TestAsyncQuestionsStayOutOfActualFinalAcrossHistoryAndInteractionSettings(t *testing.T) {
	oldAt, freshAt := int64(120), int64(160)
	for _, suppress := range []bool{false, true} {
		setting := "without-question-cards"
		if suppress {
			setting = "with-question-cards"
		}
		for _, clock := range []struct {
			name string
			at   *int64
		}{{"historical", &oldAt}, {"fresh", &freshAt}, {"unknown-clock", nil}} {
			for _, phase := range []string{"final_answer", ""} {
				name := phase
				if name == "" {
					name = "legacy-answer"
				}
				t.Run(setting+"/"+clock.name+"/"+name, func(t *testing.T) {
					e, _ := engineFixture(t)
					e.SuppressAsyncQuestions, e.AsyncQuestionsSinceMS = suppress, 150000
					bindEngine(t, e)
					ctx := context.Background()
					delivery := "async"
					question := agent("question", "ASYNC_QUESTION_BODY", "final_answer")
					question.Delivery, question.Questions = &delivery, []codexclient.AsyncQuestion{{Title: "合成问题？"}}
					turn := codexclient.Turn{ID: "turn", Status: "completed", StartedAt: clock.at, Items: []codexclient.Item{input("request", "合成任务"), question}}
					apply(t, e, 200000, false, turn)
					g := &cardGateway{}
					drain(t, e, g, 1000)
					key := mirrorKey("final", turn.ID, "0")
					previous, _ := e.Store.Mirror(ctx, "host", "thread", key)
					turn.Items = append(turn.Items, input("answer", "合成回答"), agent("result", "ACTUAL_FINAL_BODY", phase), agent("comment", "PUBLIC_COMMENTARY", "commentary"))
					apply(t, e, 201000, true, turn)
					final, err := e.Store.Mirror(ctx, "host", "thread", key)
					if err != nil || !strings.Contains(final.Card, "ACTUAL_FINAL_BODY") || strings.Contains(final.Card, "ASYNC_QUESTION_BODY") || strings.Contains(final.Card, "PUBLIC_COMMENTARY") {
						t.Fatal("question or commentary contaminated the actual final answer", err)
					}
					if previous.RemoteID != "" && final.RemoteID != previous.RemoteID {
						t.Fatal("history correction replaced the original final message")
					}
					drain(t, e, g, 2000)
					if previous.RemoteID != "" && strings.Contains(g.cards[previous.RemoteID], "ASYNC_QUESTION_BODY") {
						t.Fatal("original remote final still contains the question after correction")
					}
					writes := len(g.writes)
					apply(t, e, 202000, true, turn)
					drain(t, e, g, 3000)
					if len(g.writes) != writes || turn.Items[1].Text != "ASYNC_QUESTION_BODY" || len(turn.Items[1].Questions) != 1 {
						t.Fatal("replay resent cards or projection changed native question history")
					}
				})
			}
		}
	}
}

func TestAsyncDeliveryWithoutQuestionsRemainsInFinalAnswer(t *testing.T) {
	e, _ := engineFixture(t)
	e.SuppressAsyncQuestions, e.AsyncQuestionsSinceMS = true, 150000
	delivery := "async"
	message := agent("async-message", "ASYNC_PUBLIC_RESULT", "final_answer")
	message.Delivery = &delivery
	turn := codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{input("request", "合成任务"), message, agent("result", "NORMAL_PUBLIC_RESULT", "final_answer")}}
	apply(t, e, 200000, true, turn)
	final, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", turn.ID, "0"))
	if err != nil || !strings.Contains(final.Card, "ASYNC_PUBLIC_RESULT") || !strings.Contains(final.Card, "NORMAL_PUBLIC_RESULT") {
		t.Fatal("async delivery alone hid an ordinary public answer", err)
	}
}

func TestAsyncQuestionWithoutLaterAnswerKeepsHistoricalQuestion(t *testing.T) {
	for _, suppress := range []bool{false, true} {
		for _, phase := range []string{"", "final_answer"} {
			e, _ := engineFixture(t)
			e.SuppressAsyncQuestions, e.AsyncQuestionsSinceMS = suppress, 150000
			delivery, at := "async", int64(120)
			question := agent("question", "UNANSWERED_QUESTION", "final_answer")
			question.Delivery, question.Questions = &delivery, []codexclient.AsyncQuestion{{Title: "合成问题？"}}
			turn := codexclient.Turn{ID: "turn", Status: "completed", StartedAt: &at, Items: []codexclient.Item{input("request", "合成任务"), agent("preface", "EARLIER_PUBLIC_MESSAGE", phase), question}}
			apply(t, e, 200000, true, turn)
			final, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", turn.ID, "0"))
			if err != nil || !strings.Contains(final.Card, "UNANSWERED_QUESTION") {
				t.Fatal("earlier public message hid a historical question with no later answer", err)
			}
		}
	}
}
