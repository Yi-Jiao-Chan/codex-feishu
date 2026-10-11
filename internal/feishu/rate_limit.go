package feishu

import (
	"strconv"
	"strings"
	"time"
)

// RateLimited is a definite rejection, never proof that an ambiguous mutation
// failed. Callers may persist a delayed retry only for this explicit error.
// Unwrap preserves existing rejection handling for operations without recovery.
type RateLimited struct {
	Rejection  *Rejected
	RetryAfter time.Duration
}

func (e *RateLimited) Error() string { return e.Rejection.Error() }
func (e *RateLimited) Unwrap() error { return e.Rejection }

func definiteRateLimit(method, path string, code, status int) bool {
	// Older API gateways return 400 for the documented generic rate rejection.
	if code == 99991400 && (status == 400 || status == 429) {
		return true
	}
	if code != 230020 || status != 400 {
		return false
	}
	const base = "/open-apis/im/v1/messages"
	if method == "POST" && path == base {
		return true
	}
	if !strings.HasPrefix(path, base+"/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, base+"/"), "/")
	return len(parts) == 1 && parts[0] != "" && method == "PATCH" || len(parts) == 2 && parts[0] != "" && parts[1] == "reply" && method == "POST"
}

// Feishu documents x-ogw-ratelimit-reset as an interval in seconds, not an
// epoch. Invalid/unrepresentable hints fall back to the persisted backoff.
func rateLimitDelay(header string) time.Duration {
	seconds, err := strconv.ParseUint(strings.TrimSpace(header), 10, 64)
	if err != nil || seconds == 0 || seconds > uint64((1<<63-1)/int64(time.Second)) {
		return time.Second
	}
	return time.Duration(seconds) * time.Second
}
