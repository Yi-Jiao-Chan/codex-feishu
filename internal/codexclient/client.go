// Package codexclient connects to the daemon already used by Codex App.
package codexclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Daemon struct {
	Status           string `json:"status"`
	SocketPath       string `json:"socketPath"`
	CLIVersion       string `json:"cliVersion"`
	AppServerVersion string `json:"appServerVersion"`
}

func Discover(ctx context.Context, cli string) (Daemon, error) {
	var d Daemon
	if cli == "" || cli == "auto" {
		cli = "codex"
	}
	out, err := exec.CommandContext(ctx, cli, "app-server", "daemon", "version").Output()
	if err != nil {
		return d, fmt.Errorf("daemon discovery failed: %w", err)
	}
	if json.Unmarshal(out, &d) != nil {
		return d, errors.New("invalid daemon response")
	}
	if d.Status != "running" || !filepath.IsAbs(d.SocketPath) {
		return d, errors.New("existing shared daemon unavailable")
	}
	info, err := os.Stat(d.SocketPath)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return d, errors.New("shared socket unavailable")
	}
	return d, nil
}

// RPCError is a definite rejection; UnknownError must never trigger blind retries.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("Codex RPC rejected (%d)", e.Code) }

type UnknownError struct {
	Method string
	Cause  error
}

func (e *UnknownError) Error() string { return "Codex RPC result unknown: " + e.Method }
func (e *UnknownError) Unwrap() error { return e.Cause }

type Event struct {
	Generation uint64
	Method     string
	Params     json.RawMessage
	RequestID  json.RawMessage
}
type wire struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}
type reply struct {
	data json.RawMessage
	err  error
}
type Client struct {
	conn           *websocket.Conn
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	pending        map[string]chan reply
	serverRequests map[string]InteractiveRequest
	serverAnswered map[string]string
	serverTerminal map[string]bool
	writeGate      chan struct{}
	seq            atomic.Uint64
	events         chan Event
	done           chan struct{}
	closeOnce      sync.Once
	generation     uint64
	connectionID   string
	terminal       error
}

var generation atomic.Uint64

func Connect(ctx context.Context, path string) (*Client, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("socket must be absolute")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, errors.New("cannot create connection identity")
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	// Bound only the handshake. The initialized connection retains the
	// caller's lifetime, so cancelling this setup deadline cannot disconnect
	// a healthy long-running service generation.
	setupCtx, stopSetup := context.WithTimeout(ctx, 30*time.Second)
	defer stopSetup()
	conn, _, err := websocket.Dial(setupCtx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: tr}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		tr.CloseIdleConnections()
		return nil, errors.New("cannot connect to shared Codex Unix WebSocket")
	}
	conn.SetReadLimit(32 << 20)
	life, cancel := context.WithCancel(ctx)
	c := &Client{conn: conn, ctx: life, cancel: cancel, pending: map[string]chan reply{}, serverRequests: map[string]InteractiveRequest{}, serverAnswered: map[string]string{}, serverTerminal: map[string]bool{}, writeGate: make(chan struct{}, 1), events: make(chan Event, 512), done: make(chan struct{}), generation: generation.Add(1), connectionID: hex.EncodeToString(nonce[:])}
	go c.read()
	err = c.Call(setupCtx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "codex-feishu", "title": "Codex Feishu Bridge", "version": "0.0.1-dev"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil)
	if err == nil {
		b, _ := json.Marshal(map[string]string{"method": "initialized"})
		err = c.writeMessage(setupCtx, b, nil)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func (c *Client) Events() <-chan Event  { return c.events }
func (c *Client) Done() <-chan struct{} { return c.done }
func (c *Client) GenerationID() uint64  { return c.generation }

// ConnectionID remains unique across process restarts, unlike GenerationID.
func (c *Client) ConnectionID() string { return c.connectionID }
func (c *Client) Err() error           { c.mu.Lock(); defer c.mu.Unlock(); return c.terminal }
func (c *Client) Close()               { c.closeOnce.Do(func() { c.cancel(); c.conn.CloseNow() }); <-c.done }

// Call sends exactly once. Ambiguity after write begins is unknown, even on timeout.
func (c *Client) Call(ctx context.Context, method string, params, dst any) error {
	id := strconv.FormatUint(c.seq.Add(1), 10)
	b, err := json.Marshal(map[string]any{"id": json.RawMessage(id), "method": method, "params": params})
	if err != nil {
		return err
	}
	wait := make(chan reply, 1)
	c.mu.Lock()
	if c.terminal != nil {
		c.mu.Unlock()
		return &UnknownError{method, errors.New("connection closed")}
	}
	c.pending[id] = wait
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err = c.writeMessage(ctx, b, nil); err != nil {
		return &UnknownError{method, err}
	}
	select {
	case r := <-wait:
		if r.err != nil {
			var re *RPCError
			if errors.As(r.err, &re) {
				return re
			}
			return &UnknownError{method, r.err}
		}
		if dst != nil {
			if err = json.Unmarshal(r.data, dst); err != nil {
				return &UnknownError{method, errors.New("invalid result shape")}
			}
		}
		return nil
	case <-ctx.Done():
		return &UnknownError{method, ctx.Err()}
	case <-c.done:
		return &UnknownError{method, c.Err()}
	}
}
func (c *Client) read() {
	var terminal error
	defer func() {
		if terminal == nil {
			terminal = errors.New("connection ended")
		}
		c.mu.Lock()
		c.terminal = terminal
		for _, p := range c.pending {
			select {
			case p <- reply{err: terminal}:
			default:
			}
		}
		c.mu.Unlock()
		c.cancel()
		c.conn.CloseNow()
		close(c.events)
		close(c.done)
	}()
	for {
		_, b, err := c.conn.Read(c.ctx)
		if err != nil {
			terminal = err
			return
		}
		var w wire
		if json.Unmarshal(b, &w) != nil {
			terminal = errors.New("invalid RPC frame")
			return
		}
		if w.Method == "" && len(w.ID) > 0 {
			c.mu.Lock()
			p := c.pending[string(w.ID)]
			c.mu.Unlock()
			if p != nil {
				r := reply{data: w.Result}
				if w.Error != nil {
					r.err = w.Error
				}
				select {
				case p <- r:
				default:
				}
			}
			continue
		}
		if w.Method == "" {
			continue
		}
		// Keep reply capabilities inside this live connection. The public event
		// stream still carries unsupported requests without auto-responding.
		event := Event{c.generation, w.Method, w.Params, w.ID}
		c.mu.Lock()
		if req, parseErr := ParseInteractiveRequest(event); parseErr == nil {
			if _, closed := c.serverAnswered[NativeRequestID(req.ID)]; !closed && !c.serverTerminal[serverTurnKey(req.ThreadID, req.TurnID)] {
				c.serverRequests[NativeRequestID(req.ID)] = req
			}
		} else if w.Method == "serverRequest/resolved" {
			var p struct {
				ThreadID  string          `json:"threadId"`
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(w.Params, &p) == nil {
				key := NativeRequestID(p.RequestID)
				if req, ok := c.serverRequests[key]; ok && req.ThreadID == p.ThreadID {
					c.serverAnswered[key] = req.Fingerprint
					delete(c.serverRequests, key)
				} else if key != "" {
					c.serverAnswered[key] = "resolved"
				}

			}
		} else if w.Method == "turn/completed" {
			var p struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID string `json:"id"`
				} `json:"turn"`
			}
			if json.Unmarshal(w.Params, &p) == nil {
				if p.ThreadID != "" && p.Turn.ID != "" {
					c.serverTerminal[serverTurnKey(p.ThreadID, p.Turn.ID)] = true
				}
				for key, req := range c.serverRequests {
					if req.ThreadID == p.ThreadID && req.TurnID == p.Turn.ID {
						c.serverAnswered[key] = req.Fingerprint
						delete(c.serverRequests, key)
					}
				}
			}
		}
		c.mu.Unlock()
		select {
		case c.events <- event:
		default:
			terminal = errors.New("event overflow; history reconciliation required")
			return
		}
	}
}

// All application messages share a context-bounded gate. Final guards execute
// after waiting for other writers, while the reader remains free to resolve
// requests. The gate is released before Call awaits its RPC response.
func (c *Client) writeMessage(ctx context.Context, b []byte, before func() error) error {
	select {
	case c.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrRequestStale
	}
	defer func() { <-c.writeGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if before != nil {
		if err := before(); err != nil {
			return err
		}
	}
	return c.conn.Write(ctx, websocket.MessageText, b)
}

func serverTurnKey(thread, turn string) string {
	b, _ := json.Marshal([]string{thread, turn})
	return string(b)
}
