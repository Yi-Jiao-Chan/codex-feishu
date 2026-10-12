package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestVerifierAttachChecksPolicyWithoutOverridesAndAllowsRunningHistory(t *testing.T) {
	s := verifierSpec(t)
	var calls atomic.Int32
	socket := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		calls.Add(1)
		var params map[string]any
		if json.Unmarshal(w.Params, &params) != nil || w.Method != "thread/resume" || len(params) != 2 || params["threadId"] != "owned-test-thread" || params["excludeTurns"] != true {
			t.Error("verification attach sent extra/creation overrides", w.Method)
		}
		reply := verifierReply(s)
		thread := reply["thread"].(map[string]any)
		thread["preview"] = "fixture input"
		thread["turns"] = []any{map[string]any{"id": "existing-turn", "status": "inProgress"}}
		send(ctx, c, w.ID, reply)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Connect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	thread, err := c.AttachLocalVerification(ctx, "owned-test-thread", s)
	if err != nil || thread.ID != "owned-test-thread" || len(thread.Turns) != 1 || thread.Preview == "" || calls.Load() != 1 || thread.InteractiveMain() {
		t.Fatal(thread, err, calls.Load())
	}
}

func TestVerifierAttachRejectsUnprovenIdentityProviderAndPolicy(t *testing.T) {
	for _, variant := range []string{"other-id", "cwd", "root-cwd", "model", "provider", "thread-provider", "label", "approval", "writable", "network", "null-network", "ephemeral", "missing-ephemeral", "parent", "fork"} {
		t.Run(variant, func(t *testing.T) {
			s := verifierSpec(t)
			var calls atomic.Int32
			socket := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				calls.Add(1)
				r := verifierReply(s)
				th := r["thread"].(map[string]any)
				switch variant {
				case "other-id":
					th["id"] = "foreign-thread"
				case "cwd":
					th["cwd"] = "/foreign"
				case "root-cwd":
					r["cwd"] = "/foreign"
				case "model":
					r["model"] = "production"
				case "provider":
					r["modelProvider"] = "production"
				case "thread-provider":
					th["modelProvider"] = "production"
				case "label":
					th["threadSource"] = "user"
				case "approval":
					r["approvalPolicy"] = "on-request"
				case "writable":
					r["sandbox"] = map[string]any{"type": "workspaceWrite"}
				case "network":
					r["sandbox"] = map[string]any{"type": "readOnly", "networkAccess": true}
				case "null-network":
					r["sandbox"] = map[string]any{"type": "readOnly", "networkAccess": nil}
				case "ephemeral":
					th["ephemeral"] = true
				case "missing-ephemeral":
					delete(th, "ephemeral")
				case "parent":
					th["parentThreadId"] = "parent"
				case "fork":
					th["forkedFromId"] = "parent"
				}
				send(ctx, c, w.ID, r)
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c, err := Connect(ctx, socket)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			thread, err := c.AttachLocalVerification(ctx, "owned-test-thread", s)
			var unknown *UnknownError
			if !errors.As(err, &unknown) || unknown.Method != "thread/resume" || thread.ID != "" || calls.Load() != 1 {
				t.Fatal("unproven attach retried/accepted", thread, err)
			}
		})
	}
}

func TestReadOnlyNetworkAccessSchemaDefault(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`false`)} {
		if !readOnlyNetworkDisabled(raw) {
			t.Fatal("valid no-network policy rejected", string(raw))
		}
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`true`), json.RawMessage(`"false"`)} {
		if readOnlyNetworkDisabled(raw) {
			t.Fatal("unproven no-network policy accepted", string(raw))
		}
	}
}

func TestVerifierAttachInvalidScopeDoesNotReachRPC(t *testing.T) {
	s := verifierSpec(t)
	var calls atomic.Int32
	socket := fixture(t, func(context.Context, *websocket.Conn, wire) { calls.Add(1) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Connect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.AttachLocalVerification(ctx, "", s); err == nil {
		t.Fatal("empty owned ID accepted")
	}
	s.ModelURL = "https://production.example/v1"
	if _, err := c.AttachLocalVerification(ctx, "owned-test-thread", s); err == nil || calls.Load() != 0 {
		t.Fatal("invalid model reached native", err)
	}
}
