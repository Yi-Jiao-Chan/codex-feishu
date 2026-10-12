package verification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

const (
	localModelName    = "fixture-model"
	localModelMaxBody = 1 << 20
)

var errLocalModelBodyTooLarge = errors.New("local model request body too large")

// LocalModel is a private, deterministic Responses API fixture. It only
// listens on loopback and never forwards request data to an external service.
type LocalModel struct {
	server    *http.Server
	url       string
	started   chan struct{}
	release   chan struct{}
	stop      chan struct{}
	serveDone chan struct{}
	closeDone chan struct{}

	startedOnce sync.Once
	releaseOnce sync.Once
	stopOnce    sync.Once
	closeOnce   sync.Once
	handlers    sync.WaitGroup
	handlerMu   sync.Mutex
	closed      bool

	calls atomic.Int64

	mu       sync.Mutex
	closeErr error
}

// StartLocalModel starts a loopback-only local Responses fixture. The caller
// cannot select its address or port. Cancelling ctx closes the fixture.
func StartLocalModel(ctx context.Context) (*LocalModel, error) {
	if ctx == nil {
		return nil, errors.New("local model context is nil")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.Port <= 0 {
		listener.Close()
		return nil, errors.New("local model listener has no TCP port")
	}
	m := &LocalModel{
		url:       fmt.Sprintf("http://127.0.0.1:%d/v1", address.Port),
		started:   make(chan struct{}),
		release:   make(chan struct{}),
		stop:      make(chan struct{}),
		serveDone: make(chan struct{}),
		closeDone: make(chan struct{}),
	}
	m.server = &http.Server{Handler: m, ErrorLog: log.New(io.Discard, "", 0)}
	go func() {
		serveErr := m.server.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			m.mu.Lock()
			if m.closeErr == nil {
				m.closeErr = serveErr
			}
			m.mu.Unlock()
			m.stopNow()
			m.Release()
			go func() { _ = m.Close() }()
		}
		close(m.serveDone)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = m.Close()
		case <-m.closeDone:
		}
	}()
	select {
	case <-ctx.Done():
		_ = m.Close()
		return nil, ctx.Err()
	default:
		return m, nil
	}
}

// URL returns the private loopback Responses endpoint.
func (m *LocalModel) URL() string { return m.url }

// Calls returns the number of valid Responses requests accepted by the
// fixture. Invalid requests do not consume a sequence number.
func (m *LocalModel) Calls() int64 { return m.calls.Load() }

// Started closes after the first valid request's response.created event has
// been flushed to its client.
func (m *LocalModel) Started() <-chan struct{} { return m.started }

// Done closes when the fixture is cancelled or closed.
func (m *LocalModel) Done() <-chan struct{} { return m.stop }

// Release allows the first valid request to continue. It is idempotent.
func (m *LocalModel) Release() { m.releaseOnce.Do(func() { close(m.release) }) }

// Close stops accepting requests, closes active connections, and waits for
// handlers and the serving goroutine to exit. It is safe to call repeatedly
// and concurrently with Release.
func (m *LocalModel) Close() error {
	m.closeOnce.Do(func() {
		m.stopNow()
		m.Release()
		m.handlerMu.Lock()
		m.closed = true
		m.handlerMu.Unlock()
		err := m.server.Close()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.mu.Lock()
			if m.closeErr == nil {
				m.closeErr = err
			}
			m.mu.Unlock()
		}
		<-m.serveDone
		m.handlers.Wait()
		close(m.closeDone)
	})
	<-m.closeDone
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeErr
}

// stopNow is kept separate from closeOnce because a serving failure must
// release handlers even when Close has not been called yet.
func (m *LocalModel) stopNow() { m.stopOnce.Do(func() { close(m.stop) }) }

func (m *LocalModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.handlerMu.Lock()
	if m.closed {
		m.handlerMu.Unlock()
		return
	}
	m.handlers.Add(1)
	m.handlerMu.Unlock()
	defer m.handlers.Done()
	if r.URL.Path != "/v1/responses" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := decodeLocalModelRequest(r); err != nil {
		if errors.Is(err, errLocalModelBodyTooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid request", http.StatusBadRequest)
		}
		return
	}
	select {
	case <-m.stop:
		http.Error(w, "local model is closed", http.StatusServiceUnavailable)
		return
	default:
	}
	sequence := m.calls.Add(1)
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming is unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	if err := writeLocalModelEvent(w, createdEvent(sequence)); err != nil {
		return
	}
	if sequence == 1 {
		m.startedOnce.Do(func() { close(m.started) })
		select {
		case <-m.release:
			if requestStopped(r, m) {
				return
			}
		case <-r.Context().Done():
			return
		case <-m.stop:
			return
		}
	}
	for _, event := range completionEvents(sequence) {
		if requestStopped(r, m) {
			return
		}
		if err := writeLocalModelEvent(w, event); err != nil {
			return
		}
	}
}

func requestStopped(r *http.Request, m *LocalModel) bool {
	select {
	case <-r.Context().Done():
		return true
	case <-m.stop:
		return true
	default:
		return false
	}
}

type localModelRequest struct {
	Model  string `json:"model"`
	Stream *bool  `json:"stream"`
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}

func decodeLocalModelRequest(r *http.Request) (localModelRequest, error) {
	if r.ContentLength > localModelMaxBody {
		return localModelRequest{}, errLocalModelBodyTooLarge
	}
	counted := &countingReader{reader: r.Body}
	decoder := json.NewDecoder(io.LimitReader(counted, localModelMaxBody+1))
	var request localModelRequest
	if err := decoder.Decode(&request); err != nil {
		if counted.count > localModelMaxBody {
			return localModelRequest{}, errLocalModelBodyTooLarge
		}
		return localModelRequest{}, err
	}
	var trailing struct{}
	err := decoder.Decode(&trailing)
	if counted.count > localModelMaxBody {
		return localModelRequest{}, errLocalModelBodyTooLarge
	}
	if err != io.EOF {
		if err == nil {
			return localModelRequest{}, errors.New("multiple JSON values")
		}
		return localModelRequest{}, err
	}
	if request.Model != localModelName || request.Stream == nil || !*request.Stream {
		return localModelRequest{}, errors.New("unsupported local model request")
	}
	return request, nil
}

func writeLocalModelEvent(w http.ResponseWriter, event any) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

func createdEvent(sequence int64) map[string]any {
	return map[string]any{"type": "response.created", "response": map[string]any{"id": fmt.Sprintf("fixture-response-%d", sequence)}}
}

func completionEvents(sequence int64) []map[string]any {
	messageID := fmt.Sprintf("fixture-message-%d", sequence)
	text := fmt.Sprintf("fixture reply %d", sequence)
	item := map[string]any{
		"type":    "message",
		"id":      messageID,
		"role":    "assistant",
		"phase":   "final_answer",
		"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	return []map[string]any{
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": messageID, "role": "assistant", "phase": "final_answer", "content": []any{}}},
		{"type": "response.output_text.delta", "item_id": messageID, "output_index": 0, "content_index": 0, "delta": text},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("fixture-response-%d", sequence), "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
	}
}
