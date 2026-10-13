// Package pairing implements short-lived, single-use private-chat pairing.
// It never persists messages, codes, or identities and makes no remote calls.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"codex-feishu/internal/config"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

var (
	ErrBusy     = errors.New("pairing is already waiting")
	ErrNotFound = errors.New("pairing request not found")
	ErrInvalid  = errors.New("invalid pairing request")
	ErrClosed   = errors.New("pairing manager is closed")
)

const (
	StateWaiting   = "waiting"
	StateConfirmed = "confirmed"
	StateExpired   = "expired"
	StateCanceled  = "canceled"
	maxTerminal    = 8
	retention      = 60 * time.Second
)

// Result is safe for status responses; it never includes the pairing code.
type Result struct {
	RequestID   string `json:"request_id"`
	State       string `json:"state"`
	OwnerOpenID string `json:"owner_open_id,omitempty"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

// Challenge exposes the code only to the local caller of Begin. Callers must
// not persist or log it. Terminal challenges always have an empty Code.
type Challenge struct {
	Result
	Code string `json:"code,omitempty"`
}

// Options allows deterministic expiry checks. Now must be safe for concurrent
// calls. A custom clock is checked on every operation; the real-time expiry
// timer additionally clears the active code when its deadline is reached.
type Options struct {
	Now       func() time.Time
	BotOpenID string // Resolved by the authenticated bot API before listening.
}

type request struct {
	result     Result
	expires    time.Time
	terminalAt time.Time
	code       []byte
	digest     [sha256.Size]byte
}

type Manager struct {
	mu            sync.Mutex
	appID         string
	expectedOwner string
	botOpenID     string
	now           func() time.Time
	active        *request
	terminal      []*request
	timer         *time.Timer
	closed        bool
}

func New(appID, expectedOwner string) (*Manager, error) {
	return NewWithOptions(appID, expectedOwner, Options{})
}

func NewWithOptions(appID, expectedOwner string, options Options) (*Manager, error) {
	if !config.ValidAppID(appID) || expectedOwner != "" && !config.ValidOpenID(expectedOwner) || options.BotOpenID != "" && !config.ValidOpenID(options.BotOpenID) {
		return nil, ErrInvalid
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Manager{appID: appID, expectedOwner: expectedOwner, botOpenID: options.BotOpenID, now: options.Now}, nil
}

// GenerateRequestID returns a cryptographically random 128-bit correlation ID.
func GenerateRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("pairing randomness unavailable")
	}
	return hex.EncodeToString(raw[:]), nil
}

func validRequestID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Begin is idempotent for a retained request ID and never extends its deadline.
func (m *Manager) Begin(requestID string, ttl time.Duration) (Challenge, error) {
	if !validRequestID(requestID) || ttl < time.Second || ttl > 300*time.Second {
		return Challenge{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Challenge{}, ErrClosed
	}
	now := m.now()
	m.refresh(now)
	if r := m.find(requestID); r != nil {
		return Challenge{Result: r.result, Code: string(r.code)}, nil
	}
	if m.active != nil {
		return Challenge{}, ErrBusy
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Challenge{}, errors.New("pairing randomness unavailable")
	}
	code := make([]byte, base32.StdEncoding.WithPadding(base32.NoPadding).EncodedLen(len(random)))
	base32.StdEncoding.WithPadding(base32.NoPadding).Encode(code, random[:])
	clear(random[:])
	r := &request{
		result:  Result{RequestID: requestID, State: StateWaiting, ExpiresAtMS: now.Add(ttl).UnixMilli()},
		expires: now.Add(ttl), code: code, digest: sha256.Sum256(code),
	}
	m.active = r
	m.timer = time.AfterFunc(ttl, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.active == r {
			m.refresh(m.now())
		}
	})
	return Challenge{Result: r.result, Code: string(r.code)}, nil
}

func (m *Manager) State(requestID string) (Result, error) {
	if !validRequestID(requestID) {
		return Result{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh(m.now())
	if r := m.find(requestID); r != nil {
		return r.result, nil
	}
	return Result{}, ErrNotFound
}

// Cancel preserves an already terminal result and clears a waiting code.
func (m *Manager) Cancel(requestID string) (Result, error) {
	if !validRequestID(requestID) {
		return Result{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.refresh(now)
	if r := m.find(requestID); r != nil {
		if r == m.active {
			m.finish(StateCanceled, "", now)
		}
		return r.result, nil
	}
	return Result{}, ErrNotFound
}

// Handle consumes every readable /pair command, including rejected commands.
// The caller must dispatch consumed commands before normal input processing.
func (m *Manager) Handle(e *im.P2MessageReceiveV1) bool {
	if e == nil || e.Event == nil || e.Event.Message == nil {
		return false
	}
	msg := e.Event.Message
	var content struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(value(msg.Content)), &content) != nil {
		return false
	}
	// Clipboard copies can include surrounding whitespace. Normalize only the
	// outside of the command; the token and internal separator remain exact.
	text := strings.TrimSpace(content.Text)
	// Private-chat users can also @ the bot. Remove only leading placeholders
	// whose structured mention identity matches the authenticated bot.
	for m.botOpenID != "" {
		removed := false
		for _, mention := range msg.Mentions {
			if mention == nil || mention.Id == nil || value(mention.Id.OpenId) != m.botOpenID {
				continue
			}
			key := value(mention.Key)
			if key != "" && strings.HasPrefix(text, key) {
				text = strings.TrimSpace(strings.TrimPrefix(text, key))
				removed = true
				break
			}
		}
		if !removed {
			break
		}
	}
	fields := strings.Fields(text)
	if len(fields) == 0 || fields[0] != "/pair" {
		return false
	}
	// Authentication and shape checks occur after recognition so a rejected
	// pairing command cannot be persisted as an ordinary command or message.
	if e.EventV2Base == nil || e.EventV2Base.Header == nil || e.EventV2Base.Header.AppID != m.appID ||
		value(msg.MessageType) != "text" || value(msg.ChatType) != "p2p" ||
		value(msg.MessageId) == "" || value(msg.ChatId) == "" ||
		e.Event.Sender == nil || value(e.Event.Sender.SenderType) != "user" || e.Event.Sender.SenderId == nil {
		return true
	}
	owner := value(e.Event.Sender.SenderId.OpenId)
	if !config.ValidOpenID(owner) || m.expectedOwner != "" && owner != m.expectedOwner ||
		len(fields) != 2 || text != "/pair "+fields[1] {
		return true
	}
	digest := sha256.Sum256([]byte(fields[1]))
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.refresh(now)
	if !m.closed && m.active != nil && subtle.ConstantTimeCompare(digest[:], m.active.digest[:]) == 1 {
		m.finish(StateConfirmed, owner, now)
	}
	return true
}

// Close cancels the active request, clears its code, and prevents new requests.
// Retained terminal results remain readable until their usual retention limit.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		now := m.now()
		m.refresh(now)
		if m.active != nil {
			m.finish(StateCanceled, "", now)
		}
		m.closed = true
	}
}

// The following helpers require mu to be held.
func (m *Manager) find(id string) *request {
	if m.active != nil && m.active.result.RequestID == id {
		return m.active
	}
	for _, r := range m.terminal {
		if r.result.RequestID == id {
			return r
		}
	}
	return nil
}

func (m *Manager) refresh(now time.Time) {
	if m.active != nil && !now.Before(m.active.expires) {
		m.finish(StateExpired, "", m.active.expires)
	}
	kept := m.terminal[:0]
	for _, r := range m.terminal {
		if now.Before(r.terminalAt.Add(retention)) {
			kept = append(kept, r)
		}
	}
	clear(m.terminal[len(kept):])
	m.terminal = kept
}

func (m *Manager) finish(state, owner string, now time.Time) {
	r := m.active
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	clear(r.code)
	r.code = nil
	clear(r.digest[:])
	r.result.State, r.result.OwnerOpenID, r.terminalAt = state, owner, now
	m.active = nil
	if len(m.terminal) == maxTerminal {
		copy(m.terminal, m.terminal[1:])
		m.terminal[len(m.terminal)-1] = r
	} else {
		m.terminal = append(m.terminal, r)
	}
}

func value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
