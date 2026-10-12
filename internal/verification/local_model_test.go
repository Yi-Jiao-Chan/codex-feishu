package verification

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func localModelFixture(t *testing.T) (*LocalModel, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m, err := StartLocalModel(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m, cancel
}

func doLocalModelRequest(t *testing.T, endpoint, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func localModelValidBody() string {
	return `{"model":"fixture-model","stream":true,"input":"ignored"}`
}

type localModelFlushErrorWriter struct {
	header   http.Header
	body     bytes.Buffer
	flushErr error
}

func (w *localModelFlushErrorWriter) Header() http.Header { return w.header }

func (w *localModelFlushErrorWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *localModelFlushErrorWriter) WriteHeader(int) {}

func (w *localModelFlushErrorWriter) Flush() {}

func (w *localModelFlushErrorWriter) FlushError() error { return w.flushErr }

func readLocalModelEvent(r *bufio.Reader) (map[string]any, error) {
	var data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimSuffix(strings.TrimPrefix(line, "data: "), "\n")
		}
		if line == "\n" {
			if data == "" {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				return nil, err
			}
			return event, nil
		}
	}
}

type localModelEventResult struct {
	event map[string]any
	err   error
}

func requireLocalModelEvent(t *testing.T, r *bufio.Reader, want string) map[string]any {
	t.Helper()
	event, err := readLocalModelEvent(r)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := event["type"].(string); got != want {
		t.Fatalf("event type %q, want %q", got, want)
	}
	return event
}

func TestLocalModelSSEHoldAndLaterRequests(t *testing.T) {
	m, _ := localModelFixture(t)
	parsed, err := url.Parse(m.URL())
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" || parsed.Path != "/v1" || parsed.User != nil || parsed.RawQuery != "" {
		t.Fatalf("unsafe local model URL %q: %v", m.URL(), err)
	}
	encoded, err := json.Marshal(m)
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("local model URL escaped JSON: %s (%v)", encoded, err)
	}

	resp := doLocalModelRequest(t, m.URL()+"/responses", localModelValidBody())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream response: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(resp.Body)
	created := requireLocalModelEvent(t, reader, "response.created")
	if response, _ := created["response"].(map[string]any); response["id"] != "fixture-response-1" {
		t.Fatalf("created response identity: %#v", response)
	}
	select {
	case <-m.Started():
	case <-time.After(time.Second):
		t.Fatal("first valid request did not signal Started")
	}
	if m.Calls() != 1 {
		t.Fatalf("valid calls = %d, want 1", m.Calls())
	}

	next := make(chan localModelEventResult, 1)
	go func() {
		event, err := readLocalModelEvent(reader)
		next <- localModelEventResult{event: event, err: err}
	}()
	later := doLocalModelRequest(t, m.URL()+"/responses", localModelValidBody())
	laterReader := bufio.NewReader(later.Body)
	for _, want := range []string{"response.created", "response.output_item.added", "response.output_text.delta", "response.output_item.done", "response.completed"} {
		event := requireLocalModelEvent(t, laterReader, want)
		if want == "response.created" {
			if response, _ := event["response"].(map[string]any); response["id"] != "fixture-response-2" {
				t.Fatalf("later response identity: %#v", response)
			}
		}
		if want == "response.output_text.delta" && event["delta"] != "fixture reply 2" {
			t.Fatalf("unexpected later fixture text: %#v", event["delta"])
		}
	}
	later.Body.Close()
	select {
	case result := <-next:
		t.Fatalf("first request completed before Release: %#v (%v)", result.event, result.err)
	case <-time.After(75 * time.Millisecond):
	}
	m.Release()
	result := <-next
	if result.err != nil {
		t.Fatal(result.err)
	}
	if got, _ := result.event["type"].(string); got != "response.output_item.added" {
		t.Fatalf("event type %q, want response.output_item.added", got)
	}
	for _, want := range []string{"response.output_text.delta", "response.output_item.done", "response.completed"} {
		event := requireLocalModelEvent(t, reader, want)
		if want == "response.output_text.delta" && event["delta"] != "fixture reply 1" {
			t.Fatalf("unexpected fixture text: %#v", event["delta"])
		}
	}
	if _, err := readLocalModelEvent(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("first stream did not end: %v", err)
	}
	if m.Calls() != 2 {
		t.Fatalf("valid calls = %d, want 2", m.Calls())
	}
}

func TestLocalModelFlushFailureDoesNotSignalStarted(t *testing.T) {
	m, _ := localModelFixture(t)
	writer := &localModelFlushErrorWriter{header: make(http.Header), flushErr: errors.New("fixture flush failed")}
	req := httptest.NewRequest(http.MethodPost, m.URL()+"/responses", strings.NewReader(localModelValidBody()))
	m.ServeHTTP(writer, req)
	select {
	case <-m.Started():
		t.Fatal("Started closed after FlushError")
	default:
	}
	if m.Calls() != 1 {
		t.Fatalf("valid calls = %d, want 1", m.Calls())
	}
	body := writer.body.String()
	if !strings.Contains(body, `"type":"response.created"`) {
		t.Fatalf("created event missing from writer: %q", body)
	}
	if strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("completion event emitted after FlushError: %q", body)
	}
}

func TestLocalModelRejectsInvalidAndOversizedRequestsWithoutConsumingFirstHold(t *testing.T) {
	m, _ := localModelFixture(t)
	client := &http.Client{Timeout: 3 * time.Second}
	checks := []struct {
		method string
		url    string
		body   string
		status int
	}{
		{http.MethodGet, m.URL() + "/responses", "", http.StatusMethodNotAllowed},
		{http.MethodPost, m.URL() + "/wrong", localModelValidBody(), http.StatusNotFound},
		{http.MethodPost, m.URL() + "/responses", `{"model":"other","stream":true}`, http.StatusBadRequest},
		{http.MethodPost, m.URL() + "/responses", `{"model":"fixture-model","stream":false}`, http.StatusBadRequest},
		{http.MethodPost, m.URL() + "/responses", strings.Repeat("x", localModelMaxBody+1), http.StatusRequestEntityTooLarge},
	}
	for _, check := range checks {
		req, err := http.NewRequest(check.method, check.url, strings.NewReader(check.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != check.status {
			t.Fatalf("%s %s status %d, want %d", check.method, check.url, resp.StatusCode, check.status)
		}
	}
	if m.Calls() != 0 {
		t.Fatalf("invalid calls consumed sequence: %d", m.Calls())
	}
	select {
	case <-m.Started():
		t.Fatal("invalid calls consumed first hold")
	default:
	}

	resp := doLocalModelRequest(t, m.URL()+"/responses", localModelValidBody())
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	requireLocalModelEvent(t, reader, "response.created")
	select {
	case <-m.Started():
	case <-time.After(time.Second):
		t.Fatal("valid request did not signal Started")
	}
	if m.Calls() != 1 {
		t.Fatalf("valid calls = %d, want 1", m.Calls())
	}
	m.Release()
	for _, want := range []string{"response.output_item.added", "response.output_text.delta", "response.output_item.done", "response.completed"} {
		requireLocalModelEvent(t, reader, want)
	}
}

func TestLocalModelContextCancelClosesHeldRequestAndListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m, err := StartLocalModel(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	resp := doLocalModelRequest(t, m.URL()+"/responses", localModelValidBody())
	reader := bufio.NewReader(resp.Body)
	requireLocalModelEvent(t, reader, "response.created")
	select {
	case <-m.Started():
	case <-time.After(time.Second):
		t.Fatal("fixture did not start")
	}
	next := make(chan localModelEventResult, 1)
	go func() {
		event, err := readLocalModelEvent(reader)
		next <- localModelEventResult{event: event, err: err}
	}()
	cancel()
	select {
	case result := <-next:
		if result.err == nil {
			t.Fatalf("held request emitted event after cancellation: %#v", result.event)
		}
		if result.event != nil && result.event["type"] == "response.completed" {
			t.Fatalf("held request fabricated completion after cancellation: %#v", result.event)
		}
	case <-time.After(time.Second):
		t.Fatal("held request did not terminate its SSE stream")
	}
	select {
	case <-m.Done():
	case <-time.After(time.Second):
		t.Fatal("Done did not close after startup context cancellation")
	}
	resp.Body.Close()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Done():
	case <-time.After(time.Second):
		t.Fatal("Done did not close after Close")
	}
	if _, err := http.Get(m.URL() + "/responses"); err == nil {
		t.Fatal("closed local model accepted a new connection")
	}
}

func TestLocalModelReleaseAndCloseAreConcurrentAndIdempotent(t *testing.T) {
	m, _ := localModelFixture(t)
	resp := doLocalModelRequest(t, m.URL()+"/responses", localModelValidBody())
	reader := bufio.NewReader(resp.Body)
	requireLocalModelEvent(t, reader, "response.created")
	select {
	case <-m.Started():
	case <-time.After(time.Second):
		t.Fatal("fixture did not start")
	}

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				m.Release()
				return
			}
			if err := m.Close(); err != nil {
				t.Errorf("concurrent Close: %v", err)
			}
		}(i)
	}
	wg.Wait()
	resp.Body.Close()
	m.Release()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if m.Calls() != 1 {
		t.Fatalf("valid calls = %d, want 1", m.Calls())
	}
}

func TestStartLocalModelRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := StartLocalModel(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("StartLocalModel error = %v, want context canceled", err)
	}
}

func TestLocalModelSSEUsesFixedWireShape(t *testing.T) {
	m, _ := localModelFixture(t)
	resp := doLocalModelRequest(t, m.URL()+"/responses", localModelValidBody())
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	requireLocalModelEvent(t, reader, "response.created")
	m.Release()
	for i, want := range []string{"response.output_item.added", "response.output_text.delta", "response.output_item.done", "response.completed"} {
		event := requireLocalModelEvent(t, reader, want)
		if i == 1 && event["delta"] != "fixture reply 1" {
			t.Fatalf("unexpected fixed text: %#v", event["delta"])
		}
	}
	if _, err := readLocalModelEvent(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("stream did not finish: %v", err)
	}
	if m.Calls() != 1 {
		t.Fatalf("valid calls = %d, want 1", m.Calls())
	}
}
