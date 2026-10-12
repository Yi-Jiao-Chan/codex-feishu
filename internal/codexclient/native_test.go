package codexclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in integration against the running daemon and a disposable local model.
// This proves the Go adapter, and deliberately does not claim App UI validation.
func TestNativeDaemonQueueAndSteer(t *testing.T) {
	if os.Getenv("CODEX_FEISHU_NATIVE_TEST") != "1" {
		t.Skip("set CODEX_FEISHU_NATIVE_TEST=1 to create an isolated native test thread")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	d, err := Discover(ctx, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if d.AppServerVersion != "0.162.0" {
		t.Fatal("unverified daemon version")
	}
	var serial atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"object":"list","data":[]}`))
			return
		}
		io.Copy(io.Discard, r.Body)
		n := serial.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "close")
		event := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b); w.(http.Flusher).Flush() }
		event(map[string]any{"type": "response.created", "response": map[string]string{"id": fmt.Sprint("fixture-response-", n)}})
		if n == 1 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		id := fmt.Sprint("fixture-message-", n)
		text := fmt.Sprint("fixture reply ", n)
		item := map[string]any{"type": "message", "id": id, "role": "assistant", "phase": "final_answer", "content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}}
		event(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": id, "role": "assistant", "phase": "final_answer", "content": []any{}}})
		event(map[string]any{"type": "response.output_text.delta", "item_id": id, "output_index": 0, "content_index": 0, "delta": text})
		event(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprint("fixture-response-", n), "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	}))
	defer fixture.Close()
	a, err := Connect(ctx, d.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Connect(ctx, d.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var runBytes [16]byte
	if _, err := rand.Read(runBytes[:]); err != nil {
		t.Fatal(err)
	}
	runID := hex.EncodeToString(runBytes[:])
	repository := filepath.Join(t.TempDir(), "repository")
	worktree := filepath.Join(t.TempDir(), "worktree")
	if err := os.Mkdir(repository, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(cwd string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = cwd
		for _, env := range os.Environ() {
			key, _, _ := strings.Cut(env, "=")
			if !strings.HasPrefix(key, "GIT_") {
				cmd.Env = append(cmd.Env, env)
			}
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated git fixture failed: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	git(repository, "init", "--quiet")
	git(repository, "-c", "user.name=M0 fixture", "-c", "user.email=m0@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", "M0 isolated worktree fixture")
	git(repository, "worktree", "add", "--quiet", "--detach", worktree, "HEAD")
	spec := LocalVerificationSpec{RunID: runID, CWD: repository, ModelURL: fixture.URL + "/v1"}
	intent, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(t.TempDir(), "verification-intent.json"), intent, 0600); err != nil {
		t.Fatal(err)
	}
	created, err := a.StartLocalVerification(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	id := created.ID
	defer func() {
		once.Do(func() { close(release) })
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if e := a.Call(cleanup, "thread/archive", map[string]string{"threadId": id}, nil); e != nil {
			t.Errorf("archive isolated test: %v", e)
		}
	}()
	if _, err = a.Add(ctx, id, "initial fixture input", "cfb-test-initial"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("fixture was not invoked")
	}
	if _, err = b.Attach(ctx, id); err != nil {
		t.Fatal(err)
	}
	turns, err := b.Turns(ctx, id)
	if err != nil || len(turns) != 1 || turns[0].Status != "inProgress" {
		t.Fatalf("live turn: %v %v", turns, err)
	}
	turnID := turns[0].ID
	steer, err := b.Add(ctx, id, "fixture steering input", "cfb-test-steer")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := b.Add(ctx, id, "fixture queued input", "cfb-test-queued")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := a.Queue(ctx, id)
	if err != nil || len(queue) != 2 || queue[0].ID != steer.ID || queue[1].ID != queued.ID || serial.Load() != 1 {
		t.Fatalf("native shared queue does not preserve current turn: %v %v", queue, err)
	}
	deleted, err := b.Delete(ctx, id, steer.ID)
	if err != nil || !deleted {
		t.Fatal("exact delete claim failed")
	}
	err = b.Steer(ctx, id, "wrong-expected-turn", "must not inject", "cfb-test-rejected")
	var rejected *RPCError
	if !errors.As(err, &rejected) {
		t.Fatalf("stale turn was not rejected: %v", err)
	}
	if err = b.Steer(ctx, id, turnID, "fixture steering input", "cfb-test-steer"); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		turns, err = a.Turns(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		complete := 0
		for _, turn := range turns {
			if turn.Status == "completed" {
				complete++
			}
		}
		if complete == 2 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("native queue did not drain")
		}
	}
	correlations := map[string][]string{}
	for _, turn := range turns {
		for _, item := range turn.Items {
			if item.ClientID != nil {
				correlations[*item.ClientID] = append(correlations[*item.ClientID], turn.ID)
			}
		}
	}
	for _, cid := range []string{"cfb-test-initial", "cfb-test-steer", "cfb-test-queued"} {
		if len(correlations[cid]) != 1 {
			t.Fatalf("input persisted %d times: %s", len(correlations[cid]), cid)
		}
	}
	if correlations["cfb-test-steer"][0] != turnID || correlations["cfb-test-queued"][0] == turnID {
		t.Fatal("wrong steering / queued turn association")
	}
	if _, exists := correlations["cfb-test-rejected"]; exists {
		t.Fatal("rejected input injected")
	}
	queue, err = a.Queue(ctx, id)
	if err != nil || len(queue) != 0 {
		t.Fatal("native queue not empty")
	}
	var forked struct {
		Thread Thread `json:"thread"`
	}
	if err = a.Call(ctx, "thread/fork", map[string]any{"threadId": id, "cwd": worktree, "threadSource": "user", "excludeTurns": true, "deferGoalContinuation": true}, &forked); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if e := a.Call(cleanup, "thread/archive", map[string]string{"threadId": forked.Thread.ID}, nil); e != nil {
			t.Errorf("archive isolated fork: %v", e)
		}
	}()
	fork := forked.Thread
	if fork.ID == id || fork.ForkedFromID == nil || *fork.ForkedFromID != id || !fork.InteractiveMain() {
		t.Fatal("native human fork lineage or main-thread classification incorrect")
	}
	actualCWD, err := filepath.EvalSymlinks(fork.CWD)
	if err != nil || actualCWD != worktree {
		t.Fatal("fork cwd does not match the selected worktree")
	}
	common := git(repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if git(worktree, "rev-parse", "--path-format=absolute", "--git-common-dir") != common {
		t.Fatal("registered worktree common-dir differs")
	}
	if !strings.Contains(git(repository, "worktree", "list", "--porcelain", "-z"), "worktree "+worktree+"\x00") {
		t.Fatal("fork cwd is not in the registered worktree set")
	}
	inherited, err := b.Turns(ctx, fork.ID)
	if err != nil || len(inherited) != len(turns) || inherited[0].ID != turns[0].ID {
		t.Fatal("fork inherited history boundary not preserved")
	}
	t.Logf("fork=%s forked_from=%s checks=independent id, no subagent parent, interactive source, actual cwd, registered worktree/common-dir, inherited turn IDs; App UI NOT asserted", fork.ID, id)
	t.Logf("daemon=%s thread=%s current_turn=%s checks=idle queue/add, shared queue, exact delete, stale rejection, expected-turn steer, ordered drain, clientId persistence; App UI NOT asserted", d.AppServerVersion, id, turnID)
	if strings.TrimSpace(id) == "" {
		t.Fatal("missing isolated thread ID")
	}
}
