package codexclient

import (
	"context"
	"encoding/json"
)

const ManualRetryTriggerPrefix = "feishu_retry_manual:"
const ManualRetrySchemaVersion = "0.162.1"

// TurnError intentionally drops the provider's message and additional details.
// Retry decisions use only the typed native error classification.
type TurnError struct {
	Info json.RawMessage `json:"codexErrorInfo"`
}

func (t Turn) RetryableFailure() bool {
	return t.Status == "failed" && t.RetryReason() != ""
}

func (t Turn) RetryReason() string {
	if t.Error == nil || !receiptJSONUnique(t.Error.Info) {
		return ""
	}
	var code string
	if json.Unmarshal(t.Error.Info, &code) == nil {
		switch code {
		case "serverOverloaded":
			return "capacity"
		case "rateLimitExceeded":
			return "rate_limit"
		case "internalServerError", "flexUnavailable":
			return "temporary"
		}
		return ""
	}
	var variants map[string]json.RawMessage
	if json.Unmarshal(t.Error.Info, &variants) != nil || len(variants) != 1 {
		return ""
	}
	for name, raw := range variants {
		var detail *struct {
			HTTPStatus *int `json:"httpStatusCode"`
		}
		if json.Unmarshal(raw, &detail) != nil || detail == nil {
			return ""
		}
		switch name {
		case "httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected", "responseTooManyFailedAttempts":
			if detail.HTTPStatus == nil || *detail.HTTPStatus == 408 || *detail.HTTPStatus >= 500 && *detail.HTTPStatus <= 599 {
				return "connection"
			}
		}
	}
	return ""
}

// Retry continues the original thread without duplicating the user's input or
// overriding its settings. Callers journal this write and recheck idle history.
// Empty input cannot inject a steer message if App becomes active concurrently.
func (c *Client) Retry(ctx context.Context, thread, attemptID string) (Turn, error) {
	if thread == "" || !retryAttemptID(attemptID) {
		return Turn{}, ErrInvalidInput
	}
	var result struct {
		Turn Turn `json:"turn"`
	}
	err := c.Call(ctx, "turn/start", map[string]any{"threadId": thread, "input": []any{}, "turnTrigger": ManualRetryTriggerPrefix + attemptID}, &result)
	if err != nil {
		return Turn{}, err
	}
	if result.Turn.ID == "" {
		return Turn{}, &UnknownError{Method: "turn/start"}
	}
	return result.Turn, nil
}

func retryAttemptID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
