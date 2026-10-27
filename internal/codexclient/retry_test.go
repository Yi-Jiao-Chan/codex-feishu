package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
)

func TestRetryableFailureUsesOnlyNativeClassification(t *testing.T) {
	for _, tc := range []struct {
		info string
		want bool
	}{
		{`"serverOverloaded"`, true}, {`"rateLimitExceeded"`, true}, {`"internalServerError"`, true}, {`"flexUnavailable"`, true},
		{`{"httpConnectionFailed":{"httpStatusCode":503}}`, true}, {`{"responseStreamConnectionFailed":{"httpStatusCode":null}}`, true},
		{`{"responseStreamDisconnected":{"httpStatusCode":408}}`, true}, {`{"responseTooManyFailedAttempts":{}}`, true},
		{`"usageLimitExceeded"`, false}, {`"contextWindowExceeded"`, false}, {`"sessionBudgetExceeded"`, false},
		{`"cyberPolicy"`, false}, {`"misalignmentPolicyViolation"`, false}, {`"unauthorized"`, false}, {`"badRequest"`, false}, {`"sandboxError"`, false}, {`"other"`, false},
		{`{"httpConnectionFailed":{"httpStatusCode":401}}`, false}, {`{"httpConnectionFailed":{"httpStatusCode":429}}`, false},
		{`{"responseStreamDisconnected":null}`, false}, {`{"unknown":{}}`, false}, {`null`, false},
		{`{"httpConnectionFailed":{},"responseStreamDisconnected":{}}`, false},
	} {
		t.Run(tc.info, func(t *testing.T) {
			var turn Turn
			wire := `{"id":"failed","status":"failed","items":[],"error":{"codexErrorInfo":` + tc.info + `,"message":"PRIVATE_provider_message","additionalDetails":"PRIVATE_details"}}`
			if err := json.Unmarshal([]byte(wire), &turn); err != nil {
				t.Fatal(err)
			}
			if turn.RetryableFailure() != tc.want {
				t.Fatal("incorrect retryability")
			}
			encoded, _ := json.Marshal(turn)
			if strings.Contains(string(encoded), "PRIVATE_") {
				t.Fatal("native error bodies were retained")
			}
			turn.Status = "inProgress"
			if turn.RetryableFailure() {
				t.Fatal("an active native retry offered a second retry")
			}
		})
	}
}

func TestRetryRequestPreservesThreadSettingsAndSendsOnce(t *testing.T) {
	attempt := strings.Repeat("a", 32)
	for _, outcome := range []string{"confirmed", "rejected", "dropped", "empty"} {
		t.Run(outcome, func(t *testing.T) {
			var calls atomic.Int32
			c, ctx := creationClient(t, func(ctx context.Context, connection *websocket.Conn, w wire) {
				calls.Add(1)
				var params map[string]json.RawMessage
				if json.Unmarshal(w.Params, &params) != nil || w.Method != "turn/start" || len(params) != 3 || string(params["threadId"]) != `"thread"` || string(params["input"]) != `[]` || string(params["turnTrigger"]) != `"`+ManualRetryTriggerPrefix+attempt+`"` {
					t.Error("retry changed its thread, input, origin, or inherited settings")
				}
				switch outcome {
				case "confirmed":
					send(ctx, connection, w.ID, map[string]any{"turn": map[string]any{"id": "retried", "status": "inProgress", "items": []any{}}})
				case "empty":
					send(ctx, connection, w.ID, map[string]any{})
				case "rejected":
					b, _ := json.Marshal(map[string]any{"id": w.ID, "error": map[string]any{"code": -32600, "message": "synthetic rejection"}})
					connection.Write(ctx, websocket.MessageText, b)
				case "dropped":
					connection.CloseNow()
				}
			})
			turn, err := c.Retry(ctx, "thread", attempt)
			var rejected *RPCError
			var unknown *UnknownError
			switch outcome {
			case "confirmed":
				if err != nil || turn.ID != "retried" {
					t.Fatal("confirmed retry lost its native identity", err)
				}
			case "rejected":
				if !errors.As(err, &rejected) {
					t.Fatal("definite retry rejection was lost")
				}
			default:
				if !errors.As(err, &unknown) {
					t.Fatal("ambiguous retry became a definite outcome")
				}
			}
			if calls.Load() != 1 {
				t.Fatal("retry RPC was repeated")
			}
		})
	}
}

func TestRetryOriginRequiresExactUniqueAttempt(t *testing.T) {
	attempt := strings.Repeat("b", 32)
	for _, scenario := range []string{"exact", "foreign-attempt", "duplicate-turn", "duplicate-attempt", "missing-identity", "embedded-output"} {
		t.Run(scenario, func(t *testing.T) {
			records := goalRecords()
			start := records[1]["payload"].(map[string]any)
			attribution := start["turn_attribution"].(map[string]any)
			attribution["turn_trigger"] = ManualRetryTriggerPrefix + attempt
			switch scenario {
			case "foreign-attempt":
				attribution["turn_trigger"] = ManualRetryTriggerPrefix + strings.Repeat("c", 32)
			case "duplicate-turn":
				records = append(records, records[1])
			case "duplicate-attempt":
				records = append(records, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "other", "turn_attribution": map[string]any{"turn_id": "other", "turn_trigger": ManualRetryTriggerPrefix + attempt}}})
			case "missing-identity":
				delete(attribution, "turn_id")
			case "embedded-output":
				embedded, _ := json.Marshal(records[1])
				records[1] = map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "output": string(embedded)}}
			}
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "sessions"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "sessions", "rollout.jsonl")
			if err := os.WriteFile(path, []byte(receiptLines(t, records)), 0600); err != nil {
				t.Fatal(err)
			}
			reader := NewUnanchoredTurnReader(func(context.Context, string) (Thread, error) { return Thread{ID: "thread", Path: &path}, nil }, root)
			proof, err := reader.RetryTurns(context.Background(), "thread", []string{attempt})
			if err != nil || (proof[attempt] == "goal-turn") != (scenario == "exact") {
				t.Fatal("retry origin was not uniquely tied to its journaled attempt", err)
			}
			allowed, err := reader.Read(context.Background(), "thread", []string{"goal-turn"})
			if err != nil || scenario == "exact" && !allowed["goal-turn"] {
				t.Fatal("verified manual retry could not synchronize without new input", err)
			}
		})
	}
}
