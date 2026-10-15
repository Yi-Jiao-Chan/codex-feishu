package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func asyncAnswerFixture(question, answer string) codexclient.Item {
	identity, _ := json.Marshal([]any{"request_user_input_async", "call-fixture", 0})
	values, _ := json.Marshal([]map[string]string{{"questionItemId": string(identity), "question": question, "answer": answer}})
	return input("app-answer", "<send_user_message_question_reply>\n"+string(values)+"\n</send_user_message_question_reply>")
}

func TestAsyncReplyMirrorsReadableAnswerOnceWithEitherQuestionCardSetting(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			ctx := context.Background()
			e, path := engineFixture(t)
			e.SuppressAsyncQuestions, e.AsyncQuestionsSinceMS = enabled, 150000
			at, delivery := int64(160), "async"
			question := agent("call-fixture", "原问题消息", "final_answer")
			question.Delivery, question.Questions = &delivery, []codexclient.AsyncQuestion{{Title: "合成问题？"}}
			answer := asyncAnswerFixture("合成问题？", "合成回答")
			turns := []codexclient.Turn{
				{ID: "question-turn", Status: "completed", StartedAt: &at, Items: []codexclient.Item{input("first", "初始输入"), question}},
				{ID: "answer-turn", Status: "completed", Items: []codexclient.Item{answer, agent("final", "收到回答后继续执行", "final_answer")}},
			}
			apply(t, e, 200000, false, turns...)
			bindEngine(t, e)
			g := &cardGateway{}
			drain(t, e, g, 201000)
			m, err := e.Store.Mirror(ctx, "host", "thread", mirrorKey("human", "answer-turn", answer.ID, "0"))
			if err != nil || m.RemoteID == "" || !strings.Contains(m.Card, "合成问题？") || !strings.Contains(m.Card, "合成回答") || !strings.Contains(m.Card, "人类发送") {
				t.Fatal("async answer was not mirrored as readable human context", err)
			}
			for _, marker := range []string{"send_user_message_question_reply", "questionItemId", "request_user_input_async", "call-fixture"} {
				if strings.Contains(m.Card, marker) {
					t.Fatal("internal async answer envelope was exposed")
				}
			}
			final, err := e.Store.Mirror(ctx, "host", "thread", mirrorKey("final", "answer-turn", "0"))
			if err != nil || final.ParentKey != m.Key {
				t.Fatal("answer formatting moved the following reply away from its human input", err)
			}
			count := len(g.writes)
			if err := e.Store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			e.Store = reopened
			apply(t, e, 202000, true, turns...)
			drain(t, e, g, 203000)
			if len(g.writes) != count {
				t.Fatal("reconnect duplicated the async answer or following result")
			}
		})
	}
}

func TestAsyncReplyFromFeishuKeepsReceiptLinkWithoutHumanEcho(t *testing.T) {
	ctx := context.Background()
	e, _ := engineFixture(t)
	answer := asyncAnswerFixture("合成问题？", "合成回答")
	r, _, err := e.Store.Claim(ctx, store.Receipt{AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: "feishu-answer", Body: answer.PublicText()})
	if err != nil {
		t.Fatal(err)
	}
	answer.ClientID = &r.ClientID
	turn := codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{answer, agent("final", "继续执行", "final_answer")}}
	apply(t, e, 200000, false, turn)
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 201000)
	if len(g.writes) != 1 || g.writes[0].Parent != "feishu-answer" || strings.Contains(g.writes[0].Card, "合成回答") {
		t.Fatal("Feishu answer was echoed or its final reply lost the original parent")
	}
	links, err := e.Store.InputLinks(ctx, "host", "thread")
	if err != nil || len(links) != 1 || links[0].ReceiptID != r.ID || links[0].ItemID != answer.ID {
		t.Fatal("async answer formatting lost the exact native receipt link", err)
	}
}

func TestAsyncReplyCorrectsPreviouslyDeliveredRawEnvelopeInPlace(t *testing.T) {
	ctx := context.Background()
	e, _ := engineFixture(t)
	answer := asyncAnswerFixture("合成问题？", "合成回答")
	key := mirrorKey("human", "turn", answer.ID, "0")
	old := store.Mirror{Key: key, TurnID: "turn", Kind: "human", Terminal: true, Card: cards.Human("Codex 端", answer.PublicText(), nil)}
	if err := e.Store.StageMirrors(ctx, "host", "thread", []store.Mirror{old}, store.Watermark{TurnID: "turn", ItemID: answer.ID, ObservedAtMS: 200000}); err != nil {
		t.Fatal(err)
	}
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 201000)
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", Items: []codexclient.Item{answer}}
	apply(t, e, 202000, true, turn)
	drain(t, e, g, 203000)
	if len(g.writes) != 3 || g.writes[1].Operation != "update" || g.writes[1].ID != g.writes[0].ID || strings.Contains(g.writes[1].Card, "send_user_message_question_reply") {
		t.Fatal("existing raw answer was reposted instead of corrected on its original card")
	}
}

func TestAsyncReplyLongAnswerUsesCompleteByteBoundedSegments(t *testing.T) {
	e, _ := engineFixture(t)
	e.MaxCardBytes = 1600
	answer := asyncAnswerFixture("合成问题？", "```text\n"+strings.Repeat("完整长回答 <example> 😀\n", 100)+"```\n末尾标记")
	turn := codexclient.Turn{ID: "turn", Status: "completed", Items: []codexclient.Item{answer, agent("final", "继续执行", "final_answer")}}
	apply(t, e, 200000, false, turn)
	bindEngine(t, e)
	g := &cardGateway{}
	drain(t, e, g, 201000)
	var humanCards string
	for _, write := range g.writes {
		if len(write.Card) > e.MaxCardBytes || !json.Valid([]byte(write.Card)) {
			t.Fatal("async answer exceeded the serialized card byte limit")
		}
		if strings.Contains(write.Card, "人类发送") {
			humanCards += write.Card
		}
	}
	if len(g.writes) < 3 || strings.Count(humanCards, "完整长回答") != 100 || !strings.Contains(humanCards, "末尾标记") || strings.Contains(humanCards, "questionItemId") {
		t.Fatal("long async answer lost text or exposed its envelope while splitting")
	}
}
