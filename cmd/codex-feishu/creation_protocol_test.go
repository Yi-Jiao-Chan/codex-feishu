package main

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/app"
	"codex-feishu/internal/codexclient"
)

func TestServeClassifiesNewVersionHandshakeErrors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		incompatible bool
	}{
		{"transient", errors.New("fixture transport unavailable"), false},
		{"missing initialize", &codexclient.RPCError{Code: -32601}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connect := serveNativeConnect("auto", func(context.Context, string) (codexclient.Daemon, error) {
				return codexclient.Daemon{Status: "running", AppServerVersion: "0.169.1", SocketPath: "/unused/existing.sock"}, nil
			}, func(context.Context, string) (*codexclient.Client, error) {
				return nil, tc.err
			})
			_, err := connect(context.Background())
			var mismatch *app.NativeHandshakeIncompatible
			if err == nil || errors.As(err, &mismatch) != tc.incompatible || mismatch != nil && mismatch.Version != "0.169.1" {
				t.Fatalf("unexpected classification: %v", err)
			}
		})
	}
}
