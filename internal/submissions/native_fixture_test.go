package submissions

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

const nativeControllerOptIn = "CODEX_FEISHU_NATIVE_CONTROLLER_TEST"
const nativeCrashExit = 86

// Every request is held independently. This is a local fixed-text model, not
// a proxy or fake native queue: the actual shared daemon executes every RPC.
type nativeResponseGate struct {
	release chan struct{}
	once    sync.Once
}

type nativeQueueModel struct {
	server *httptest.Server
	mu     sync.Mutex
	gates  []*nativeResponseGate
	seen   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func newNativeQueueModel() *nativeQueueModel {
	m := &nativeQueueModel{seen: make(chan struct{}, 1), done: make(chan struct{})}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	return m
}

func (m *nativeQueueModel) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
		http.NotFound(w, r)
		return
	}
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request) != nil || request.Model != "fixture-model" || !request.Stream {
		http.Error(w, "unsupported fixture request", http.StatusBadRequest)
		return
	}
	gate := &nativeResponseGate{release: make(chan struct{})}
	m.mu.Lock()
	m.gates = append(m.gates, gate)
	sequence := len(m.gates)
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "close")
	emit := func(event any) error {
		body, err := json.Marshal(event)
		if err == nil {
			_, err = fmt.Fprintf(w, "data: %s\n\n", body)
		}
		if err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	responseID := fmt.Sprintf("owned-response-%d", sequence)
	if emit(map[string]any{"type": "response.created", "response": map[string]any{"id": responseID}}) != nil {
		return
	}
	select {
	case m.seen <- struct{}{}:
	default:
	}
	select {
	case <-gate.release:
	case <-r.Context().Done():
		return
	case <-m.done:
		return
	}
	select {
	case <-r.Context().Done():
		return
	case <-m.done:
		return
	default:
	}
	itemID := fmt.Sprintf("owned-message-%d", sequence)
	text := "owned native controller fixture reply"
	item := map[string]any{"type": "message", "id": itemID, "role": "assistant", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	for _, event := range []any{
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": itemID, "role": "assistant", "phase": "final_answer", "content": []any{}}},
		map[string]any{"type": "response.output_text.delta", "item_id": itemID, "output_index": 0, "content_index": 0, "delta": text},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
		map[string]any{"type": "response.completed", "response": map[string]any{"id": responseID, "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
	} {
		if emit(event) != nil {
			return
		}
	}
}

func (m *nativeQueueModel) wait(ctx context.Context, sequence int) error {
	for {
		m.mu.Lock()
		seen := len(m.gates) >= sequence
		m.mu.Unlock()
		if seen {
			return nil
		}
		select {
		case <-m.seen:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *nativeQueueModel) release(sequence int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sequence <= len(m.gates) {
		gate := m.gates[sequence-1]
		gate.once.Do(func() { close(gate.release) })
	}
}

func (m *nativeQueueModel) close() {
	m.once.Do(func() { close(m.done); m.server.Close() })
}

// The marker is exclusive and persists before thread/start. Native thread IDs
// are accepted only from the checked start response and an exclusive second
// marker. Child processes cannot select a user thread or arbitrary workspace.
type nativeQueueOwnership struct {
	Spec     codexclient.LocalVerificationSpec `json:"spec"`
	ThreadID string                            `json:"thread_id"`
	Version  string                            `json:"daemon_version"`
}

func writeNativePrivateJSON(path string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

type nativeQueueBoundary struct {
	client                                    *codexclient.Client
	owned                                     string
	adds, deletes, steers, starts, interrupts int
	beforeDelete                              func(context.Context, string) error
	beforeStart                               func(context.Context) error
	afterDelete                               func(context.Context, string) error
	discardAdd, discardDelete, discardSteer   bool
	steerTargets                              []string
}

func (n *nativeQueueBoundary) check(thread string) error {
	if thread != n.owned || thread == "" {
		return errors.New("native fixture target mismatch")
	}
	return nil
}

func (n *nativeQueueBoundary) Add(ctx context.Context, thread, body, client string) (codexclient.Queued, error) {
	if err := n.check(thread); err != nil {
		return codexclient.Queued{}, err
	}
	n.adds++
	q, err := n.client.Add(ctx, thread, body, client)
	if err == nil && n.discardAdd {
		n.discardAdd = false
		return codexclient.Queued{}, &codexclient.UnknownError{Method: "thread/queue/add", Cause: errors.New("accepted reply discarded at Controller boundary")}
	}
	return q, err
}

func (n *nativeQueueBoundary) Delete(ctx context.Context, thread, queue string) (bool, error) {
	if err := n.check(thread); err != nil {
		return false, err
	}
	if n.beforeDelete != nil {
		if err := n.beforeDelete(ctx, queue); err != nil {
			return false, err
		}
	}
	n.deletes++
	deleted, err := n.client.Delete(ctx, thread, queue)
	if err == nil && deleted && n.afterDelete != nil {
		if err := n.afterDelete(ctx, queue); err != nil {
			return false, err
		}
	}
	if err == nil && n.discardDelete {
		n.discardDelete = false
		return false, &codexclient.UnknownError{Method: "thread/queue/delete", Cause: errors.New("accepted reply discarded at Controller boundary")}
	}
	return deleted, err
}

func (n *nativeQueueBoundary) Steer(ctx context.Context, thread, turn, body, client string) error {
	if err := n.check(thread); err != nil {
		return err
	}
	n.steers++
	n.steerTargets = append(n.steerTargets, turn)
	err := n.client.Steer(ctx, thread, turn, body, client)
	if err == nil && n.discardSteer {
		n.discardSteer = false
		return &codexclient.UnknownError{Method: "turn/steer", Cause: errors.New("accepted reply discarded at Controller boundary")}
	}
	return err
}

func (n *nativeQueueBoundary) Interrupt(ctx context.Context, thread, turn string) error {
	if err := n.check(thread); err != nil {
		return err
	}
	n.interrupts++
	return n.client.Interrupt(ctx, thread, turn)
}
func (n *nativeQueueBoundary) StartQueue(ctx context.Context, thread string) error {
	if err := n.check(thread); err != nil {
		return err
	}
	if n.beforeStart != nil {
		if err := n.beforeStart(ctx); err != nil {
			return err
		}
	}
	n.starts++
	return n.client.StartQueue(ctx, thread)
}
func (n *nativeQueueBoundary) Queue(ctx context.Context, thread string) ([]codexclient.Queued, error) {
	if err := n.check(thread); err != nil {
		return nil, err
	}
	return n.client.Queue(ctx, thread)
}
func (n *nativeQueueBoundary) Turns(ctx context.Context, thread string) ([]codexclient.Turn, error) {
	if err := n.check(thread); err != nil {
		return nil, err
	}
	return n.client.Turns(ctx, thread)
}

type nativeQueueHarness struct {
	ctx                                context.Context
	client, other                      *codexclient.Client
	model                              *nativeQueueModel
	spec                               codexclient.LocalVerificationSpec
	root, path, socket, thread, active string
	db                                 *store.Store
	boundary                           *nativeQueueBoundary
	controller                         *Controller
}

func requireNativeQueueOptIn(t *testing.T) {
	t.Helper()
	if os.Getenv(nativeControllerOptIn) != "1" {
		t.Skip("opt-in creates only owned local-model native fixtures; set CODEX_FEISHU_NATIVE_CONTROLLER_TEST=1")
	}
}

func newNativeQueueHarness(t *testing.T, busy bool) *nativeQueueHarness {
	t.Helper()
	requireNativeQueueOptIn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	t.Cleanup(cancel)
	daemon, err := codexclient.Discover(ctx, "auto")
	if err != nil || daemon.AppServerVersion != "0.162.0" {
		t.Fatal("existing verified native daemon 0.162.0 required")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("cannot resolve private scratch root")
	}
	cwd := filepath.Join(root, "cwd")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal("cannot create private fixture cwd")
	}
	// A fixed candidate path is an additional safety fence, never an input to
	// native RPCs. The suite uses fresh OS scratch directories only.
	candidate := "/data00/home/chenxiaoyu.lemon/.local/share/codex-feishu/p1-candidate-20261012-b26f1988/project"
	if rel, err := filepath.Rel(candidate, cwd); err != nil || rel == "." || !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		t.Fatal("scratch directory must be outside the current candidate project")
	}
	model := newNativeQueueModel()
	h := &nativeQueueHarness{ctx: ctx, root: root, path: filepath.Join(root, "controller.db"), socket: daemon.SocketPath, model: model}
	t.Cleanup(func() { h.cleanup(t) })
	h.spec = codexclient.LocalVerificationSpec{RunID: store.ID(), CWD: cwd, ModelURL: model.server.URL + "/v1"}
	if err := writeNativePrivateJSON(filepath.Join(root, "intent.json"), nativeQueueOwnership{Spec: h.spec, Version: daemon.AppServerVersion}); err != nil {
		t.Fatal("cannot persist exclusive fixture intent")
	}
	h.client, err = codexclient.Connect(ctx, daemon.SocketPath)
	if err != nil {
		t.Fatal("cannot initialize fixture native client")
	}
	created, err := h.client.StartLocalVerification(ctx, h.spec)
	if err != nil {
		t.Fatal("typed fixture thread creation failed; no creation retry")
	}
	h.thread = created.ID
	if err := writeNativePrivateJSON(filepath.Join(root, "owned-thread.json"), nativeQueueOwnership{Spec: h.spec, ThreadID: h.thread, Version: daemon.AppServerVersion}); err != nil {
		t.Fatal("cannot persist exclusive owned-thread marker")
	}
	if busy {
		// A new cold thread has no rollout to resume on 0.162. The creator
		// materializes this authorized fixture before the second client joins.
		if _, err := h.client.Add(ctx, h.thread, "owned initial fixture input", "owned-initial-"+h.spec.RunID); err != nil {
			t.Fatal("owned initial queue/add failed")
		}
		if err := model.wait(ctx, 1); err != nil {
			t.Fatal("first local model request missing")
		}
		h.active = h.waitActive(t)
		h.other, err = codexclient.Connect(ctx, daemon.SocketPath)
		if err != nil {
			t.Fatal("cannot initialize second fixture client")
		}
		if _, err := h.other.AttachLocalVerification(ctx, h.thread, h.spec); err != nil {
			t.Log("typed start response passed exact fixture model/provider, private cwd, never approval, readonly/no-network, non-ephemeral and validation-label checks; creator materialization has completed; no Controller input follows a failed join")
			var rpc *codexclient.RPCError
			if errors.As(err, &rpc) {
				message := strings.NewReplacer(h.thread, "[owned-thread]", h.spec.RunID, "[owned-run]", h.root, "[private-scratch]", h.model.server.URL, "[owned-loopback]").Replace(rpc.Message)
				t.Logf("typed join rejected before policy readback: RPC code=%d message=%q; request keys=threadId,excludeTurns; no overrides; reply policy/cwd/endpoint fields unavailable", rpc.Code, message)
			} else {
				t.Log("typed join policy proof failed; no diagnostic resume retry or native input follows")
			}
			t.Fatal("second client could not verify owned fixture policy")
		}
	}
	h.db, err = store.Open(h.path)
	if err != nil {
		t.Fatal("cannot create isolated controller journal")
	}
	if err := nativeQueueSeedBinding(ctx, h.db, h.thread, cwd); err != nil {
		t.Fatal("cannot seed isolated local-only route")
	}
	h.boundary = &nativeQueueBoundary{client: h.client, owned: h.thread}
	h.controller = &Controller{Native: h.boundary, Store: h.db, HostID: "native-fixture", AppID: "native-fixture", ThreadID: h.thread}
	return h
}

func nativeQueueSeedBinding(ctx context.Context, db *store.Store, thread, cwd string) error {
	if _, err := db.BeginBaseline(ctx, "native-fixture", "native-fixture", 1); err != nil {
		return err
	}
	if err := db.ObserveNew(ctx, "native-fixture", thread, cwd, 2); err != nil {
		return err
	}
	if err := db.MarkEligible(ctx, "native-fixture", thread, "owned fixture", cwd, cwd); err != nil {
		return err
	}
	g, err := db.PrepareGroup(ctx, "native-fixture", thread, "fixture-owner", "local-only fixture route", nil)
	if err != nil {
		return err
	}
	if _, err := db.ClaimGroupDispatch(ctx, g.ID); err != nil {
		return err
	}
	if err := db.ConfirmGroup(ctx, g.ID, "local-only-chat"); err != nil {
		return err
	}
	return db.BindConfirmedGroup(ctx, g.ID)
}

func (h *nativeQueueHarness) cleanup(t *testing.T) {
	t.Helper()
	// Close held Responses streams even if a native primitive failed. Only the
	// exact ID returned by this test's checked birth can be archived.
	h.model.close()
	if h.thread != "" && h.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		client := h.client
		select {
		case <-client.Done():
			daemon, err := codexclient.Discover(ctx, "auto")
			if err == nil && daemon.AppServerVersion == "0.162.0" && daemon.SocketPath == h.socket {
				if connected, err := codexclient.Connect(ctx, h.socket); err == nil {
					client = connected
					defer client.Close()
				}
			}
		default:
		}
		meta, err := client.Metadata(ctx, h.thread)
		var label string
		if err == nil && meta.ID == h.thread && meta.CWD == h.spec.CWD && json.Unmarshal(meta.ThreadSource, &label) == nil && label == "feishu.bridge.validation" {
			queue, err := client.Queue(ctx, h.thread)
			if err == nil {
				for _, q := range queue {
					_, _ = client.Delete(ctx, h.thread, q.ID)
				}
			}
			turns, err := client.Turns(ctx, h.thread)
			if err == nil {
				for _, turn := range turns {
					if turn.Status == "inProgress" {
						_ = client.Interrupt(ctx, h.thread, turn.ID)
					}
				}
			}
			if err := nativeQueueEventually(ctx, func() (bool, error) {
				turns, err := client.Turns(ctx, h.thread)
				if codexclient.BeforeFirstInput(err) {
					return true, nil
				}
				for _, turn := range turns {
					if turn.Status == "inProgress" {
						return false, err
					}
				}
				return true, err
			}); err != nil {
				t.Error("owned fixture did not become idle for cleanup")
			}
			if err := client.Call(ctx, "thread/archive", map[string]any{"threadId": h.thread}, nil); err != nil {
				t.Error("owned fixture archive failed")
			}
		} else {
			t.Error("owned fixture cleanup identity could not be verified; refusing archive")
		}
	}
	if h.other != nil {
		h.other.Close()
	}
	if h.client != nil {
		h.client.Close()
	}
	if h.db != nil {
		_ = h.db.Close()
	}
}

func nativeQueueEventually(ctx context.Context, predicate func() (bool, error)) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		done, err := predicate()
		if err != nil || done {
			return err
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (h *nativeQueueHarness) waitActive(t *testing.T) string {
	t.Helper()
	var active string
	err := nativeQueueEventually(h.ctx, func() (bool, error) {
		turns, err := h.client.Turns(h.ctx, h.thread)
		for _, turn := range turns {
			if turn.Status == "inProgress" {
				active = turn.ID
			}
		}
		return active != "", err
	})
	if err != nil {
		t.Fatal("owned active turn missing")
	}
	return active
}

func (h *nativeQueueHarness) claim(t *testing.T) store.Receipt {
	t.Helper()
	r, fresh, err := h.db.Claim(h.ctx, store.Receipt{AppID: "native-fixture", ThreadID: h.thread, ChatID: "local-only-chat", MessageID: store.ID(), Body: "owned controller fixture input"})
	if err != nil || !fresh {
		t.Fatal("isolated receipt claim failed")
	}
	return r
}

func (h *nativeQueueHarness) view(t *testing.T, receipt string) store.Submission {
	t.Helper()
	rows, err := h.db.Submissions(h.ctx, h.controller.scope())
	if err != nil {
		t.Fatal("isolated submissions read failed")
	}
	for _, row := range rows {
		if row.ID == receipt {
			return row
		}
	}
	t.Fatal("isolated submission missing")
	return store.Submission{}
}

func (h *nativeQueueHarness) tick(t *testing.T) {
	t.Helper()
	if err := h.controller.Tick(h.ctx); err != nil {
		t.Fatal("real native Controller tick failed")
	}
}

func (h *nativeQueueHarness) steerAction(t *testing.T, receipt string) store.SubmissionAction {
	t.Helper()
	n := h.view(t, receipt)
	op, err := h.controller.ClaimAction(h.ctx, receipt, "steer", n.QueueID, n.Revision)
	if err != nil {
		t.Fatal("isolated steering action claim failed")
	}
	return op
}

func (h *nativeQueueHarness) reopen(t *testing.T) {
	t.Helper()
	if err := h.db.Close(); err != nil {
		t.Fatal("isolated journal close failed")
	}
	db, err := store.Open(h.path)
	if err != nil {
		t.Fatal("isolated journal reopen failed")
	}
	h.db = db
	h.controller = &Controller{Native: h.boundary, Store: db, HostID: "native-fixture", AppID: "native-fixture", ThreadID: h.thread}
}

func (h *nativeQueueHarness) history(t *testing.T, client string) []string {
	t.Helper()
	turns, err := h.client.Turns(h.ctx, h.thread)
	if err != nil {
		t.Fatal("owned native history unavailable")
	}
	var matches []string
	for _, turn := range turns {
		for _, item := range turn.Items {
			if item.Type == "userMessage" && item.ClientID != nil && *item.ClientID == client {
				matches = append(matches, turn.ID)
			}
		}
	}
	return matches
}

func TestNativeQueueFixtureHoldsEachResponseAndClosesWithoutCompletion(t *testing.T) {
	m := newNativeQueueModel()
	defer m.close()
	client := &http.Client{Timeout: 3 * time.Second}
	for sequence := 1; sequence <= 2; sequence++ {
		resp, err := client.Post(m.server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"fixture-model","stream":true}`))
		if err != nil {
			t.Fatal("fixture request failed")
		}
		reader := bufio.NewReader(resp.Body)
		line, err := reader.ReadString('\n')
		if err != nil || !strings.Contains(line, "response.created") {
			t.Fatal("fixture did not flush creation")
		}
		if sequence == 1 {
			m.release(sequence)
		} else {
			m.close()
		}
		remaining, _ := io.ReadAll(reader)
		_ = resp.Body.Close()
		if strings.Contains(string(remaining), "response.completed") != (sequence == 1) {
			t.Fatal("fixture completion did not follow its gate/lifecycle")
		}
	}
}
