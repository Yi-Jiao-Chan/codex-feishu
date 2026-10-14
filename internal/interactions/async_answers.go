package interactions

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type asyncReply struct {
	QuestionItemID string `json:"questionItemId"`
	Question       string `json:"question"`
	Answer         string `json:"answer"`
}

type observedAsyncReply struct {
	asyncReply
	position int
}

func (c *Controller) closeAnsweredAsync(ctx context.Context, rows []store.InteractionRequest, turns []codexclient.Turn) error {
	answers := map[string]map[int][]observedAsyncReply{}
	questionPositions := map[string]int{}
	questionCounts := map[string]int{}
	questionProfiles := map[string]string{}
	questionBodies := map[string][]codexclient.AsyncQuestion{}
	ambiguous := map[string]bool{}
	position := 0
	for _, turn := range turns {
		for _, item := range turn.Items {
			position++
			if item.Type == "agentMessage" && item.Delivery != nil && *item.Delivery == "async" && len(item.Questions) > 0 {
				identity := key(turn.ID, item.ID)
				body, _ := json.Marshal(item.Questions)
				if _, seen := questionPositions[identity]; !seen {
					questionPositions[identity] = position
					questionCounts[item.ID]++
					questionProfiles[identity] = string(body)
					questionBodies[identity] = item.Questions
				} else if questionProfiles[identity] != string(body) {
					ambiguous[item.ID] = true
				}
			}
			if item.Type != "userMessage" {
				continue
			}
			text := strings.TrimSpace(item.PublicText())
			const open, close = "<send_user_message_question_reply>", "</send_user_message_question_reply>"
			if !strings.HasPrefix(text, open) || !strings.HasSuffix(text, close) {
				continue
			}
			body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, open), close))
			var replies []asyncReply
			if json.Unmarshal([]byte(body), &replies) != nil {
				continue
			}
			for _, reply := range replies {
				var identity []json.RawMessage
				var tool, callID string
				if strings.TrimSpace(reply.Answer) == "" || json.Unmarshal([]byte(reply.QuestionItemID), &identity) != nil || len(identity) != 3 || json.Unmarshal(identity[0], &tool) != nil || tool != "request_user_input_async" || json.Unmarshal(identity[1], &callID) != nil || callID == "" {
					continue
				}
				indexText := strings.TrimSpace(string(identity[2]))
				index, err := strconv.Atoi(indexText)
				if err != nil || index < 0 || strconv.Itoa(index) != indexText {
					continue
				}
				if answers[callID] == nil {
					answers[callID] = map[int][]observedAsyncReply{}
				}
				answers[callID][index] = append(answers[callID][index], observedAsyncReply{reply, position})
			}
		}
	}
	for _, row := range rows {
		if row.Mode != "async" || row.State == "resolved" {
			continue
		}
		if questionCounts[row.ItemID] > 1 || ambiguous[row.ItemID] {
			if err := c.Store.CloseAsyncInteraction(ctx, row.Key, row.ConnectionID, false); err != nil {
				return err
			}
			continue
		}
		questionPosition, present := questionPositions[key(row.TurnID, row.ItemID)]
		if !present {
			continue // Absence in a projection does not prove an answer.
		}
		var request codexclient.InteractiveRequest
		if json.Unmarshal([]byte(row.Payload), &request) != nil {
			return store.ErrConflict
		}
		if !sameAsyncQuestions(request.Questions, questionBodies[key(row.TurnID, row.ItemID)]) {
			if err := c.Store.CloseAsyncInteraction(ctx, row.Key, row.ConnectionID, false); err != nil {
				return err
			}
			continue
		}
		matched := 0
		for i, q := range request.Questions {
			for _, reply := range answers[row.ItemID][i] {
				if reply.Question == q.Question && reply.position > questionPosition {
					matched++
					break
				}
			}
		}
		if matched == 0 {
			continue
		}
		complete := matched == len(request.Questions)
		if err := c.Store.CloseAsyncInteraction(ctx, row.Key, row.ConnectionID, complete); err != nil {
			return err
		}
	}
	return nil
}

func sameAsyncQuestions(stored []codexclient.InputQuestion, observed []codexclient.AsyncQuestion) bool {
	if len(stored) != len(observed) {
		return false
	}
	for i, question := range stored {
		if question.Question != observed[i].Title || len(question.Options) != len(observed[i].Options) {
			return false
		}
		for n, option := range question.Options {
			if option.Label != observed[i].Options[n] {
				return false
			}
		}
	}
	return true
}
