package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
)

func TestRetryRejectsAnUnverifiedDiscoveredVersionBeforeRPC(t *testing.T) {
	for _, version := range []string{"", "0.162.0", "future"} {
		native := &discoveredNative{serverVersion: version}
		_, err := native.Retry(context.Background(), "thread", strings.Repeat("a", 32))
		var rejected *codexclient.RPCError
		if !errors.As(err, &rejected) || rejected.Code != -32601 {
			t.Fatal("unverified native retry version reached its client")
		}
	}
}
