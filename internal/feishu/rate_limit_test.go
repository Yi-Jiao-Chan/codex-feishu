package feishu

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimitClassificationUsesDocumentedResponseAndOperation(t *testing.T) {
	const base = "/open-apis/im/v1/messages"
	for _, tc := range []struct {
		name, method, path, body, state string
		status                          int
	}{
		{"generic", "POST", base, `{"code":99991400}`, "limited", 429},
		{"old-gateway", "POST", "/open-apis/im/v1/chats", `{"code":99991400}`, "limited", 400},
		{"send", "POST", base, `{"code":230020}`, "limited", 400},
		{"reply", "POST", base + "/om_one/reply", `{"code":230020}`, "limited", 400},
		{"update", "PATCH", base + "/om_one", `{"code":230020}`, "limited", 400},
		{"reaction-not-allowlisted", "POST", base + "/om_one/reactions", `{"code":230020}`, "rejected", 400},
		{"quota", "POST", base, `{"code":99991403}`, "rejected", 429},
		{"sending-in-flight", "POST", base, `{"code":230049}`, "unknown", 400},
		{"server-failure", "POST", base, `{"code":99991400}`, "unknown", 503},
		{"unexplained429", "POST", base, `{"code":1234}`, "unknown", 429},
		{"429-code-zero", "POST", base, `{"code":0}`, "unknown", 429},
		{"malformed", "POST", base, `not-json`, "unknown", 429},
		{"no-code", "PATCH", base + "/om_one", `{}`, "unknown", 200},
		{"null-code", "PATCH", base + "/om_one", `{"code":null}`, "unknown", 200},
		{"permission", "POST", base, `{"code":99991672}`, "rejected", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mutations atomic.Int32
			g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
					return
				}
				mutations.Add(1)
				w.Header().Set("x-ogw-ratelimit-reset", "7")
				w.Header().Set("X-Tt-Logid", "rate-fixture")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})
			err := g.request(context.Background(), tc.method, tc.path, nil, nil, nil)
			var limited *RateLimited
			var rejected *Rejected
			var unknown *Unknown
			if mutations.Load() != 1 {
				t.Fatal("gateway retried mutation", mutations.Load())
			}
			switch tc.state {
			case "limited":
				if !errors.As(err, &limited) || !errors.As(err, &rejected) || limited.RetryAfter != 7*time.Second || rejected.RequestID != "rate-fixture" {
					t.Fatal("limit response lost", err)
				}
			case "unknown":
				if !errors.As(err, &unknown) || errors.As(err, &limited) {
					t.Fatal("ambiguous response marked retryable", err)
				}
			case "rejected":
				if !errors.As(err, &rejected) || errors.As(err, &limited) {
					t.Fatal("permanent failure marked retryable", err)
				}
			}
			if err == nil || strings.Contains(err.Error(), g.secret) || strings.Contains(err.Error(), "fixture-token") {
				t.Fatal("invalid or unsafe error")
			}
		})
	}
}

func TestRateLimitResetIsPositiveSecondsWithoutDurationOverflow(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"", time.Second}, {"0", time.Second}, {"-1", time.Second}, {"1.5", time.Second},
		{"18446744073709551615", time.Second}, {"9223372037", time.Second},
		{" 60 ", time.Minute}, {"86400", 24 * time.Hour}, {"9223372036", 9223372036 * time.Second},
	} {
		if got := rateLimitDelay(tc.header); got != tc.want {
			t.Fatalf("header %q: %s want %s", tc.header, got, tc.want)
		}
	}
}
