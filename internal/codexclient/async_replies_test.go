package codexclient

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func asyncReplyValue(callID string, index int, question, answer string) map[string]string {
	identity, _ := json.Marshal([]any{"request_user_input_async", callID, index})
	return map[string]string{"questionItemId": string(identity), "question": question, "answer": answer}
}

func asyncReplyEnvelope(values ...map[string]string) string {
	body, _ := json.Marshal(values)
	return "<send_user_message_question_reply>\n" + string(body) + "\n</send_user_message_question_reply>"
}

func TestAsyncQuestionRepliesPreserveIdentityOrderAndAnswerBytes(t *testing.T) {
	question, answer := "请描述环境？", "  合成环境\n\n```text\n  保留缩进 <example>\n```\n尾行\n"
	text := asyncReplyEnvelope(
		asyncReplyValue("call-first", 2, question, answer),
		asyncReplyValue("call-second", 0, "还需要什么？", "补充说明"),
	)
	want := []AsyncQuestionReply{
		{CallID: "call-first", QuestionIndex: 2, Question: question, Answer: answer},
		{CallID: "call-second", QuestionIndex: 0, Question: "还需要什么？", Answer: "补充说明"},
	}
	got, ok := ParseAsyncQuestionReplies("\r\n" + text + "\r\n")
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("reply identities or answer bytes changed: %#v", got)
	}
	item := presentationItem(map[string]string{"type": "text", "text": text}, map[string]string{"type": "localImage", "path": "/not-read/answer.png"})
	presentation := item.UserPresentation()
	wantText := "**" + question + "**\n\n" + answer + "\n\n**还需要什么？**\n\n补充说明"
	if presentation.Text != wantText || len(presentation.Attachments) != 1 || presentation.Attachments[0].Path != "/not-read/answer.png" {
		t.Fatal("async answer presentation lost text or its native attachment")
	}
	if item.PublicText() != text {
		t.Fatal("async answer presentation changed native history")
	}
}

func TestAsyncQuestionRepliesLeaveUnknownOrAmbiguousInputsIntact(t *testing.T) {
	value := asyncReplyValue("call-fixture", 0, "合成问题？", "合成回答")
	valid := asyncReplyEnvelope(value)
	cases := map[string]string{
		"ordinary":            "普通正文",
		"quoted":              "引用示例：\n" + valid,
		"fenced":              "```text\n" + valid + "\n```",
		"suffix":              valid + "\n补充正文",
		"missing close":       strings.TrimSuffix(valid, "</send_user_message_question_reply>"),
		"malformed JSON":      "<send_user_message_question_reply>[</send_user_message_question_reply>",
		"trailing JSON":       strings.Replace(valid, "\n</send_", "\n{}\n</send_", 1),
		"empty batch":         "<send_user_message_question_reply>[]</send_user_message_question_reply>",
		"null batch":          "<send_user_message_question_reply>null</send_user_message_question_reply>",
		"unknown field":       strings.Replace(valid, `"answer":`, `"extra":"preserve this","answer":`, 1),
		"case alias":          strings.Replace(valid, `"answer":`, `"Answer":`, 1),
		"case duplicate":      strings.Replace(valid, `"answer":`, `"Answer":"discarded","answer":`, 1),
		"duplicate key":       strings.Replace(valid, `"answer":`, `"answer":"discarded","answer":`, 1),
		"missing answer":      asyncReplyEnvelope(map[string]string{"questionItemId": value["questionItemId"], "question": value["question"]}),
		"blank answer":        asyncReplyEnvelope(asyncReplyValue("call-fixture", 0, "合成问题？", "\n ")),
		"blank question":      asyncReplyEnvelope(asyncReplyValue("call-fixture", 0, " ", "合成回答")),
		"duplicate identity":  asyncReplyEnvelope(value, asyncReplyValue("call-fixture", 0, "合成问题？", "另一回答")),
		"mixed invalid batch": asyncReplyEnvelope(value, asyncReplyValue("call-fixture", 1, "另一题？", "")),
	}
	for name, identity := range map[string]string{
		"wrong tool":       `["request_user_input","call-fixture",0]`,
		"missing call":     `["request_user_input_async","",0]`,
		"blank call":       `["request_user_input_async"," ",0]`,
		"null call":        `["request_user_input_async",null,0]`,
		"missing index":    `["request_user_input_async","call-fixture"]`,
		"extra identity":   `["request_user_input_async","call-fixture",0,"extra"]`,
		"negative index":   `["request_user_input_async","call-fixture",-1]`,
		"negative zero":    `["request_user_input_async","call-fixture",-0]`,
		"fractional index": `["request_user_input_async","call-fixture",0.5]`,
		"string index":     `["request_user_input_async","call-fixture","0"]`,
	} {
		cases[name] = asyncReplyEnvelope(map[string]string{"questionItemId": identity, "question": "合成问题？", "answer": "合成回答"})
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if replies, ok := ParseAsyncQuestionReplies(text); ok || replies != nil {
				t.Fatal("unknown or ambiguous input was accepted as an async answer")
			}
			if got := presentationItem(map[string]string{"type": "text", "text": text}).UserPresentation(); got.Text != text {
				t.Fatal("unknown or ambiguous input lost original text")
			}
		})
	}
}
