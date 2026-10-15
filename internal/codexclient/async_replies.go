package codexclient

import (
	"encoding/json"
	"strconv"
	"strings"
)

// AsyncQuestionReply is one answer from the App's asynchronous question input.
// Parsing this input is read-only; it does not submit or resolve a request.
type AsyncQuestionReply struct {
	CallID        string
	QuestionIndex int
	Question      string
	Answer        string
}

// ParseAsyncQuestionReplies accepts only the complete, known App envelope.
// Unknown or ambiguous inputs remain ordinary user text, with no partial parse.
func ParseAsyncQuestionReplies(text string) ([]AsyncQuestionReply, bool) {
	const open, close = "<send_user_message_question_reply>", "</send_user_message_question_reply>"
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, open) || !strings.HasSuffix(text, close) {
		return nil, false
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, open), close))
	if !receiptJSONUnique([]byte(body)) {
		return nil, false
	}
	var values []map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &values) != nil || len(values) == 0 {
		return nil, false
	}
	type questionIdentity struct {
		callID string
		index  int
	}
	seen := map[questionIdentity]bool{}
	replies := make([]AsyncQuestionReply, 0, len(values))
	for _, value := range values {
		var questionItemID, question, answer string
		// Exact keys avoid encoding/json's case-insensitive struct matching,
		// which could silently discard a second, differently cased answer.
		if len(value) != 3 || json.Unmarshal(value["questionItemId"], &questionItemID) != nil ||
			json.Unmarshal(value["question"], &question) != nil || json.Unmarshal(value["answer"], &answer) != nil {
			return nil, false
		}
		var identity []json.RawMessage
		var tool, callID string
		if strings.TrimSpace(question) == "" || strings.TrimSpace(answer) == "" ||
			json.Unmarshal([]byte(questionItemID), &identity) != nil || len(identity) != 3 ||
			json.Unmarshal(identity[0], &tool) != nil || tool != "request_user_input_async" ||
			json.Unmarshal(identity[1], &callID) != nil || strings.TrimSpace(callID) == "" {
			return nil, false
		}
		indexText := strings.TrimSpace(string(identity[2]))
		index, err := strconv.Atoi(indexText)
		key := questionIdentity{callID, index}
		if err != nil || index < 0 || strconv.Itoa(index) != indexText || seen[key] {
			return nil, false
		}
		seen[key] = true
		replies = append(replies, AsyncQuestionReply{CallID: callID, QuestionIndex: index, Question: question, Answer: answer})
	}
	return replies, true
}
