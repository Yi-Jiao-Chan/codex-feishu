package main

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/codexclient"
)

func TestServeCreationRequiresItsVerifiedProtocolBeforeConnect(t *testing.T) {
	for _, version := range []string{"0.162.0", "0.162.1"} {
		t.Run(version, func(t *testing.T) {
			connections := 0
			connect := serveNativeConnect("auto", func(context.Context, string) (codexclient.Daemon, error) {
				return codexclient.Daemon{Status: "running", AppServerVersion: version, SocketPath: "/unused/existing.sock"}, nil
			}, func(context.Context, string) (*codexclient.Client, error) {
				connections++
				return nil, errors.New("fixture dial stopped")
			}, true)
			if _, err := connect(context.Background()); err == nil {
				t.Fatal("fixture unexpectedly connected")
			}
			want := 0
			if version == "0.162.1" {
				want = 1
			}
			if connections != want {
				t.Fatalf("creation dialed unverified protocol or rejected verified protocol: %d", connections)
			}
		})
	}
}
