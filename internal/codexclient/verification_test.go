package codexclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func verifierSpec(t *testing.T) LocalVerificationSpec {
	t.Helper()
	cwd := filepath.Join(t.TempDir(), "cwd")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	return LocalVerificationSpec{RunID: strings.Repeat("a", 32), CWD: cwd, ModelURL: "http://127.0.0.1:12345/v1"}
}
func verifierReply(s LocalVerificationSpec) map[string]any {
	provider := "cfb_verify_" + s.RunID
	return map[string]any{"thread": map[string]any{"id": "owned-test-thread", "cwd": s.CWD, "ephemeral": false, "modelProvider": provider, "threadSource": verificationSource, "turns": []any{}, "preview": ""}, "cwd": s.CWD, "model": "fixture-model", "modelProvider": provider, "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}}
}

func TestVerifierBirth(t *testing.T) {
	s := verifierSpec(t)
	var calls atomic.Int32
	socket := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		calls.Add(1)
		if w.Method != "thread/start" {
			t.Error("verification opened another native action", w.Method)
		}
		var args map[string]any
		if err := json.Unmarshal(w.Params, &args); err != nil {
			t.Error(err)
		}
		provider := "cfb_verify_" + s.RunID
		if args["cwd"] != s.CWD || args["modelProvider"] != provider || args["model"] != "fixture-model" || args["approvalPolicy"] != "never" || args["sandbox"] != "read-only" || args["threadSource"] != verificationSource || args["ephemeral"] != false || len(args) != 8 {
			t.Error("unsafe creation params", args)
		}
		config := args["config"].(map[string]any)
		model := config["model_providers."+provider].(map[string]any)
		if len(config) != 2 || model["base_url"] != s.ModelURL || model["requires_openai_auth"] != false || model["supports_websockets"] != false || model["request_max_retries"] != float64(0) {
			t.Error("production or extra config override", config)
		}
		send(ctx, c, w.ID, verifierReply(s))
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Connect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	thread, err := c.StartLocalVerification(ctx, s)
	if err != nil || thread.ID != "owned-test-thread" || thread.InteractiveMain() || calls.Load() != 1 {
		t.Fatal(thread, err, calls.Load())
	}
}

func TestVerifierScope(t *testing.T) {
	s := verifierSpec(t)
	var calls atomic.Int32
	socket := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		calls.Add(1)
		send(ctx, c, w.ID, verifierReply(s))
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Connect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, address := range []string{"https://127.0.0.1:12345/v1", "http://8.8.8.8:12345/v1", "http://localhost:12345/v1", "http://127.0.0.1/v1", "http://127.0.0.1:0/v1", "http://user:secret@127.0.0.1:12345/v1", "http://127.0.0.1:12345/v1?key=x", "http://127.0.0.1:12345/v1#x", "http://127.0.0.1:12345/other"} {
		bad := s
		bad.ModelURL = address
		if _, err := c.StartLocalVerification(ctx, bad); err == nil {
			t.Fatal("unsafe address accepted", address)
		}
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(s.CWD, alias); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"relative", s.CWD + "/../other", alias} {
		bad := s
		bad.CWD = dir
		if _, err := c.StartLocalVerification(ctx, bad); err == nil {
			t.Fatal("unsafe cwd accepted", dir)
		}
	}
	if err := os.Chmod(s.CWD, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.StartLocalVerification(ctx, s); err == nil {
		t.Fatal("shared directory accepted")
	}
	os.Chmod(s.CWD, 0700)
	bad := s
	bad.RunID = "wrong"
	if _, err := c.StartLocalVerification(ctx, bad); err == nil {
		t.Fatal("invalid namespace accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid scope reached native RPC", calls.Load())
	}
}

func TestVerifierPolicy(t *testing.T) {
	for _, variant := range []string{"missing-label", "foreign-label", "foreign-cwd", "wrong-model", "wrong-provider", "approval", "writable", "network", "ephemeral", "missing-ephemeral", "parent", "fork", "old-input"} {
		t.Run(variant, func(t *testing.T) {
			s := verifierSpec(t)
			var calls atomic.Int32
			socket := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				calls.Add(1)
				r := verifierReply(s)
				th := r["thread"].(map[string]any)
				switch variant {
				case "missing-label":
					delete(th, "threadSource")
				case "foreign-label":
					th["threadSource"] = "user"
				case "foreign-cwd":
					th["cwd"] = "/production"
				case "wrong-model":
					r["model"] = "production-model"
				case "wrong-provider":
					r["modelProvider"] = "production"
				case "approval":
					r["approvalPolicy"] = "on-request"
				case "writable":
					r["sandbox"] = map[string]any{"type": "workspaceWrite"}
				case "network":
					r["sandbox"] = map[string]any{"type": "readOnly", "networkAccess": true}
				case "ephemeral":
					th["ephemeral"] = true
				case "missing-ephemeral":
					delete(th, "ephemeral")
				case "parent":
					th["parentThreadId"] = "foreign"
				case "fork":
					th["forkedFromId"] = "foreign"
				case "old-input":
					th["turns"] = []any{map[string]any{"id": "old-turn"}}
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
			thread, err := c.StartLocalVerification(ctx, s)
			var unknown *UnknownError
			if !errors.As(err, &unknown) || thread.ID != "" || calls.Load() != 1 {
				t.Fatal("unproven birth accepted/retried", thread, err, calls.Load())
			}
		})
	}
}

func TestVerifierLostResponse(t *testing.T) {
	s := verifierSpec(t)
	var calls atomic.Int32
	socket := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) { calls.Add(1); c.CloseNow() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Connect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.StartLocalVerification(ctx, s)
	var unknown *UnknownError
	if !errors.As(err, &unknown) || calls.Load() != 1 {
		t.Fatal("lost creation retried", err, calls.Load())
	}
}

func TestConnectionIdentityIsUniqueForInitializedClients(t *testing.T) {
	socket := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) { send(ctx, c, w.ID, map[string]any{}) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, err := Connect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Connect(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, c := range []*Client{first, second} {
		b, err := hex.DecodeString(c.ConnectionID())
		if err != nil || len(b) != 16 || c.GenerationID() == 0 {
			t.Fatal("invalid initialized identity")
		}
	}
	if first.ConnectionID() == second.ConnectionID() {
		t.Fatal("connection nonce reused")
	}
}
