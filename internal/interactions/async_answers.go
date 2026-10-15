package interactions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type observedAsyncReply struct {
	codexclient.AsyncQuestionReply
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
			replies, ok := codexclient.ParseAsyncQuestionReplies(item.PublicText())
			if !ok {
				continue
			}
			for _, reply := range replies {
				if answers[reply.CallID] == nil {
					answers[reply.CallID] = map[int][]observedAsyncReply{}
				}
				answers[reply.CallID][reply.QuestionIndex] = append(answers[reply.CallID][reply.QuestionIndex], observedAsyncReply{reply, position})
			}
		}
	}
	for _, row := range rows {
		if row.Mode != "async" {
			continue
		}
		if row.State == "resolved" {
			if _, err := c.Store.InteractionAnswer(ctx, row.Key); err == nil {
				continue
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
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
		actual := map[string]codexclient.QuestionAnswer{}
		conflict := false
		for i, q := range request.Questions {
			for _, reply := range answers[row.ItemID][i] {
				if reply.Question == q.Question && reply.position > questionPosition {
					if previous, found := actual[q.ID]; found {
						conflict = conflict || previous.Answers[0] != reply.Answer
						continue
					}
					actual[q.ID] = codexclient.QuestionAnswer{Answers: []string{reply.Answer}}
					matched++
				}
			}
		}
		if matched == 0 {
			continue
		}
		complete := !conflict && matched == len(request.Questions)
		if err := c.Store.CloseAsyncInteraction(ctx, row.Key, row.ConnectionID, complete); err != nil {
			return err
		}
		if complete {
			result, err := json.Marshal(struct {
				Answers map[string]codexclient.QuestionAnswer `json:"answers"`
			}{actual})
			if err != nil {
				return err
			}
			if err := c.Store.RecordInteractionAnswer(ctx, row.Key, string(result)); err != nil {
				return err
			}
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
