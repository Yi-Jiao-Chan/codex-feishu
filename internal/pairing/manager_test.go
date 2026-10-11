package pairing

import (
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const testApp = "cli_pairing_test"
const testOwner = "ou_pairing_owner"

func ptr(s string) *string { return &s }

func message(text string) *im.P2MessageReceiveV1 {
	content, _ := json.Marshal(map[string]string{"text": text})
	return &im.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{AppID: testApp}},
		Event: &im.P2MessageReceiveV1Data{
			Sender:  &im.EventSender{SenderType: ptr("user"), SenderId: &im.UserId{OpenId: ptr(testOwner)}},
			Message: &im.EventMessage{MessageId: ptr("om_pair"), ChatId: ptr("oc_pair"), ChatType: ptr("p2p"), MessageType: ptr("text"), Content: ptr(string(content))},
		},
	}
}

func id(n int) string { return fmt.Sprintf("%032x", n) }

func fakeManager(t *testing.T, owner string) (*Manager, *atomic.Int64) {
	t.Helper()
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC).UnixNano())
	m, err := NewWithOptions(testApp, owner, Options{Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m, &clock
}

func begin(t *testing.T, m *Manager, n int, ttl time.Duration) Challenge {
	t.Helper()
	c, err := m.Begin(id(n), ttl)
	if err != nil || c.State != StateWaiting {
		t.Fatalf("begin state=%s err=%v", c.State, err)
	}
	return c
}

func state(t *testing.T, m *Manager, n int, want string) Result {
	t.Helper()
	r, err := m.State(id(n))
	if err != nil || r.State != want {
		t.Fatalf("state=%s want=%s err=%v", r.State, want, err)
	}
	return r
}

func assertCleared(t *testing.T, r *request, code []byte) {
	t.Helper()
	if r.code != nil || r.digest != [32]byte{} {
		t.Fatal("terminal request retains its code or digest")
	}
	for _, b := range code {
		if b != 0 {
			t.Fatal("code backing storage was not cleared")
		}
	}
}

func TestValidationAndRandomness(t *testing.T) {
	for _, owner := range []string{"user_id", "ou_short", "ou_bad<markup>"} {
		if _, err := New(testApp, owner); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid owner error=%v", err)
		}
	}
	if _, err := New("", ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("empty app accepted")
	}
	m, _ := fakeManager(t, "")
	for _, requestID := range []string{"", "input contains secret", strings.Repeat("A", 32), strings.Repeat("g", 32), strings.Repeat("0", 31)} {
		if _, err := m.Begin(requestID, time.Second); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid request ID accepted")
		}
		if _, err := m.State(requestID); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid state request accepted")
		}
		if _, err := m.Cancel(requestID); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid cancel request accepted")
		}
	}
	for _, ttl := range []time.Duration{0, time.Second - 1, 300*time.Second + 1} {
		if _, err := m.Begin(id(1), ttl); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid TTL accepted")
		}
	}
	seen := map[string]bool{}
	for range 32 {
		requestID, err := GenerateRequestID()
		if err != nil || !validRequestID(requestID) || seen[requestID] {
			t.Fatal("request ID randomness/format failed")
		}
		seen[requestID] = true
	}
	c := begin(t, m, 1, 300*time.Second)
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(c.Code)
	if err != nil || len(raw) != 16 || strings.Contains(c.Code, "=") {
		t.Fatal("challenge is not a 128-bit unpadded base32 code")
	}
}

func TestBeginIdempotenceBusyAndTerminal(t *testing.T) {
	m, clock := fakeManager(t, "")
	c := begin(t, m, 1, 10*time.Second)
	clock.Add(int64(5 * time.Second))
	again, err := m.Begin(id(1), 300*time.Second)
	if err != nil || again != c {
		t.Fatal("idempotent begin changed code or deadline")
	}
	if _, err := m.Begin(id(2), time.Second); !errors.Is(err, ErrBusy) {
		t.Fatal("second active request accepted")
	}
	r := m.active
	code := r.code
	if _, err := m.Cancel(id(1)); err != nil {
		t.Fatal(err)
	}
	assertCleared(t, r, code)
	terminal, err := m.Begin(id(1), time.Second)
	if err != nil || terminal.State != StateCanceled || terminal.Code != "" || terminal.ExpiresAtMS != c.ExpiresAtMS {
		t.Fatal("terminal begin restarted request or exposed code")
	}
	begin(t, m, 2, time.Second)
}

func TestRejectsUnauthenticatedOrInexactCommands(t *testing.T) {
	mutations := []struct {
		name string
		edit func(*im.P2MessageReceiveV1)
	}{
		{"group", func(e *im.P2MessageReceiveV1) { e.Event.Message.ChatType = ptr("group") }},
		{"wrong-app", func(e *im.P2MessageReceiveV1) { e.EventV2Base.Header.AppID = "cli_another" }},
		{"no-header", func(e *im.P2MessageReceiveV1) { e.EventV2Base.Header = nil }},
		{"no-base", func(e *im.P2MessageReceiveV1) { e.EventV2Base = nil }},
		{"bot", func(e *im.P2MessageReceiveV1) { e.Event.Sender.SenderType = ptr("bot") }},
		{"no-sender", func(e *im.P2MessageReceiveV1) { e.Event.Sender = nil }},
		{"no-identity", func(e *im.P2MessageReceiveV1) { e.Event.Sender.SenderId = nil }},
		{"wrong-identity-kind", func(e *im.P2MessageReceiveV1) { e.Event.Sender.SenderId = &im.UserId{UserId: ptr(testOwner)} }},
		{"empty-open-id", func(e *im.P2MessageReceiveV1) { e.Event.Sender.SenderId.OpenId = nil }},
		{"invalid-open-id", func(e *im.P2MessageReceiveV1) { e.Event.Sender.SenderId.OpenId = ptr("ou_bad<markup>") }},
		{"no-message-id", func(e *im.P2MessageReceiveV1) { e.Event.Message.MessageId = nil }},
		{"no-chat-id", func(e *im.P2MessageReceiveV1) { e.Event.Message.ChatId = nil }},
		{"nontext-readable-command", func(e *im.P2MessageReceiveV1) { e.Event.Message.MessageType = ptr("post") }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := fakeManager(t, "")
			c := begin(t, m, 1, time.Second)
			e := message("/pair " + c.Code)
			tc.edit(e)
			if !m.Handle(e) {
				t.Fatal("recognized pairing command escaped interception")
			}
			state(t, m, 1, StateWaiting)
		})
	}
	m, _ := fakeManager(t, "")
	c := begin(t, m, 1, time.Second)
	for _, text := range []string{"/pair", "/pair wrong-token", "/pair " + c.Code + " extra", "/pair  " + c.Code, " /pair " + c.Code, "/pair " + c.Code + "\n", "/pair\t" + c.Code} {
		if !m.Handle(message(text)) {
			t.Fatal("invalid pairing command escaped interception")
		}
		state(t, m, 1, StateWaiting)
	}
	for _, e := range []*im.P2MessageReceiveV1{nil, {}, {Event: &im.P2MessageReceiveV1Data{}}, message("ordinary input"), message("/pairing unrelated")} {
		if m.Handle(e) {
			t.Fatal("non-pair message consumed")
		}
	}
	e := message("ordinary")
	e.Event.Message.Content = ptr("malformed JSON")
	if m.Handle(e) {
		t.Fatal("unreadable content consumed")
	}
}

func TestSingleUseExpectedOwnerAndRestart(t *testing.T) {
	m, _ := fakeManager(t, testOwner)
	if !m.Handle(message("/pair pending-code")) {
		t.Fatal("pre-begin command escaped interception")
	}
	c := begin(t, m, 1, time.Second)
	e := message("/pair " + c.Code)
	e.Event.Sender.SenderId.OpenId = ptr("ou_other_valid_user")
	m.Handle(e)
	state(t, m, 1, StateWaiting)
	r, raw := m.active, m.active.code
	if !m.Handle(message("/pair "+c.Code)) || state(t, m, 1, StateConfirmed).OwnerOpenID != testOwner {
		t.Fatal("expected owner failed to pair")
	}
	assertCleared(t, r, raw)
	m.Handle(e)
	if state(t, m, 1, StateConfirmed).OwnerOpenID != testOwner {
		t.Fatal("terminal replay replaced owner")
	}
	canceled, err := m.Cancel(id(1))
	if err != nil || canceled.State != StateConfirmed {
		t.Fatal("cancel changed confirmed request")
	}
	begin(t, m, 2, time.Second)
	m.Handle(message("/pair " + c.Code))
	state(t, m, 2, StateWaiting)
	m.Close()
	other, _ := fakeManager(t, "")
	begin(t, other, 1, time.Second)
	other.Handle(message("/pair " + c.Code))
	state(t, other, 1, StateWaiting)
}

func TestExpiryIsStrictAndClearsCode(t *testing.T) {
	for _, delta := range []time.Duration{time.Second - time.Nanosecond, time.Second, time.Second + time.Nanosecond} {
		m, clock := fakeManager(t, "")
		c := begin(t, m, 1, time.Second)
		r, code := m.active, m.active.code
		clock.Add(int64(delta))
		m.Handle(message("/pair " + c.Code))
		want := StateExpired
		if delta < time.Second {
			want = StateConfirmed
		}
		state(t, m, 1, want)
		assertCleared(t, r, code)
	}
	m, clock := fakeManager(t, "")
	begin(t, m, 1, time.Second)
	clock.Add(int64(61 * time.Second))
	if _, err := m.State(id(1)); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired result survived terminal retention boundary")
	}
}

func TestExpirationTimerClearsWithoutPolling(t *testing.T) {
	m, err := New(testApp, "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	begin(t, m, 1, time.Second)
	r, code := m.active, m.active.code
	deadline := time.Now().Add(3 * time.Second)
	for {
		m.mu.Lock()
		finished := m.active == nil
		if finished {
			assertCleared(t, r, code)
		}
		m.mu.Unlock()
		if finished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expiration timer did not clear idle challenge")
		}
		time.Sleep(10 * time.Millisecond)
	}
	state(t, m, 1, StateExpired)
}

func TestBoundedTerminalRetention(t *testing.T) {
	m, clock := fakeManager(t, "")
	for n := 1; n <= 12; n++ {
		begin(t, m, n, time.Second)
		if _, err := m.Cancel(id(n)); err != nil {
			t.Fatal(err)
		}
		clock.Add(int64(time.Millisecond))
	}
	if len(m.terminal) != 8 {
		t.Fatal("terminal results exceeded their bound")
	}
	if _, err := m.State(id(4)); !errors.Is(err, ErrNotFound) {
		t.Fatal("oldest terminal result was not evicted")
	}
	state(t, m, 5, StateCanceled)
	begin(t, m, 13, 300*time.Second)
	clock.Add(int64(60 * time.Second))
	state(t, m, 13, StateWaiting)
	if len(m.terminal) != 0 || m.active == nil {
		t.Fatal("retention cleanup affected active challenge or retained old results")
	}
}

func TestConcurrentWinnerAndCloseCancel(t *testing.T) {
	m, _ := fakeManager(t, "")
	c := begin(t, m, 1, time.Second)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for n := range 32 {
		e := message("/pair " + c.Code)
		e.Event.Sender.SenderId.OpenId = ptr(fmt.Sprintf("ou_competing_user_%d", n))
		wg.Add(1)
		go func() { defer wg.Done(); <-start; m.Handle(e) }()
	}
	close(start)
	wg.Wait()
	winner := state(t, m, 1, StateConfirmed).OwnerOpenID
	if !strings.HasPrefix(winner, "ou_competing_user_") {
		t.Fatal("no authenticated winner")
	}
	m.Handle(message("/pair " + c.Code))
	if state(t, m, 1, StateConfirmed).OwnerOpenID != winner {
		t.Fatal("winner changed after replay")
	}
	for range 20 {
		m, _ := fakeManager(t, "")
		c := begin(t, m, 1, time.Second)
		r, code := m.active, m.active.code
		wg.Add(3)
		go func() { defer wg.Done(); m.Handle(message("/pair " + c.Code)) }()
		go func() { defer wg.Done(); m.Cancel(id(1)) }()
		go func() { defer wg.Done(); m.Close() }()
		wg.Wait()
		result, err := m.State(id(1))
		if err != nil || result.State != StateConfirmed && result.State != StateCanceled || result.State == StateCanceled && result.OwnerOpenID != "" {
			t.Fatalf("inconsistent concurrent terminal state=%s error=%v", result.State, err)
		}
		assertCleared(t, r, code)
		if _, err := m.Begin(id(2), time.Second); !errors.Is(err, ErrClosed) {
			t.Fatal("closed manager accepted new request")
		}
		if !m.Handle(message("/pair " + c.Code)) {
			t.Fatal("closed manager leaked pair command")
		}
	}
}

func TestResultsAndErrorsDoNotDiscloseInputOrCode(t *testing.T) {
	m, _ := fakeManager(t, "")
	c := begin(t, m, 1, time.Second)
	for _, result := range []Result{state(t, m, 1, StateWaiting), func() Result { r, _ := m.Cancel(id(1)); return r }()} {
		data, err := json.Marshal(result)
		if err != nil || strings.Contains(string(data), c.Code) || strings.Contains(string(data), "code") || strings.Contains(string(data), "text") {
			t.Fatal("result JSON disclosed code or input")
		}
	}
	if _, err := m.Begin("private-input "+c.Code, time.Second); err == nil || strings.Contains(err.Error(), c.Code) || strings.Contains(err.Error(), "private-input") {
		t.Fatal("error disclosed invalid input")
	}
	if _, err := m.State(id(99)); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown result lookup did not return fixed error")
	}
	if _, err := m.Cancel(id(99)); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown cancel did not return fixed error")
	}
}
