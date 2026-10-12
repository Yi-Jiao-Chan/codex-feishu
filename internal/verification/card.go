// Package verification owns explicitly authorized, owner-only probes inside
// the existing bridge. It never opens another receiver or native connection.
package verification

import (
	"context"
	"errors"
	"regexp"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

var ErrInvalid = errors.New("invalid verification request")
var ErrNotReady = errors.New("verification receiver is not ready")
var requestID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func ValidRequestID(id string) bool { return requestID.MatchString(id) }

type Gateway interface {
	SendCard(context.Context, string, string, string, string) (string, error)
}

// Result reports component observations, never full phase-one acceptance.
// The token and card body intentionally have no public representation.
type Result struct {
	RequestID            string `json:"request_id"`
	BridgeCommit         string `json:"bridge_commit"`
	BinarySHA256         string `json:"binary_sha256"`
	BuildChanged         bool   `json:"build_changed"`
	State                string `json:"state"`
	CardStatus           string `json:"card_status"`
	CallbackStatus       string `json:"callback_status"`
	PrivateMessageStatus string `json:"private_message_status"`
	MessageID            string `json:"message_id,omitempty"`
	ExpiresAtMS          int64  `json:"expires_at_ms"`
	Expired              bool   `json:"expired"`
}

type Manager struct {
	Store                          *store.Store
	Gateway                        Gateway
	HostID, AppID, OwnerID, Commit string
	BinarySHA256                   string
	Ready                          func() bool
	Now                            func() time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) Begin(ctx context.Context, id string) (Result, error) {
	if !ValidRequestID(id) || m.Store == nil || m.Gateway == nil || m.HostID == "" || m.AppID == "" || m.OwnerID == "" || m.Commit == "" || !ValidBinaryHash(m.BinarySHA256) {
		return Result{}, ErrInvalid
	}
	if m.Ready == nil || !m.Ready() {
		return Result{}, ErrNotReady
	}
	now := m.now()
	token := store.ID()
	c, err := m.Store.PrepareVerificationCard(ctx, store.VerificationCard{RequestID: id, HostID: m.HostID, AppID: m.AppID, OwnerID: m.OwnerID, BridgeCommit: m.Commit, BinarySHA256: m.BinarySHA256, Token: token, UUID: store.ID(), Card: cards.Verification(token), CreatedAtMS: now.UnixMilli(), ExpiresAtMS: now.Add(5 * time.Minute).UnixMilli()})
	if err != nil {
		return Result{}, err
	}
	// A prior process may have lost its response. Only a committed ready
	// record from this build can leave again; dispatching never re-sends.
	if c.BridgeCommit != m.Commit || c.BinarySHA256 != m.BinarySHA256 || c.State != "ready" || ctx.Err() != nil {
		return m.result(c), ctx.Err()
	}
	claimed, err := m.Store.ClaimVerificationCard(ctx, c, m.now().UnixMilli())
	if err != nil {
		return Result{}, err
	}
	if !claimed {
		return m.State(ctx, id)
	}
	idRemote, sendErr := m.Gateway.SendCard(ctx, "open_id", m.OwnerID, c.Card, c.UUID)
	state := "confirmed"
	if sendErr != nil || idRemote == "" {
		state = "unknown"
		idRemote = ""
		var rejected *feishu.Rejected
		if errors.As(sendErr, &rejected) {
			state = "rejected"
		}
	}
	// Preserve the remote outcome even if the management client disconnected
	// during the call. A crash or failed local commit leaves dispatching,
	// which State exposes as unknown and never treats as retryable.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err = m.Store.FinishVerificationCard(finishCtx, c, state, idRemote); err != nil {
		return Result{}, err
	}
	return m.State(finishCtx, id)
}

func (m *Manager) State(ctx context.Context, id string) (Result, error) {
	if !ValidRequestID(id) || m.Store == nil {
		return Result{}, ErrInvalid
	}
	c, err := m.Store.VerificationCard(ctx, id, m.HostID, m.AppID, m.OwnerID)
	if err != nil {
		return Result{}, err
	}
	return m.result(c), nil
}

func (m *Manager) result(c store.VerificationCard) Result {
	r := Result{RequestID: c.RequestID, BridgeCommit: c.BridgeCommit, BinarySHA256: c.BinarySHA256, BuildChanged: c.BridgeCommit != m.Commit || c.BinarySHA256 != m.BinarySHA256, State: c.State, CardStatus: "pending", CallbackStatus: "pending", PrivateMessageStatus: "pending", MessageID: c.MessageID, ExpiresAtMS: c.ExpiresAtMS, Expired: m.now().UnixMilli() >= c.ExpiresAtMS}
	if c.State == "dispatching" {
		r.State = "unknown"
	}
	if c.State == "rejected" {
		r.CardStatus = "fail"
	}
	if c.State == "confirmed" && c.MessageID != "" {
		r.CardStatus = "pass"
		if c.CallbackMessageID == c.MessageID && c.CallbackChatID != "" {
			r.CallbackStatus = "pass"
		}
		if c.PrivateMessageID != "" {
			r.PrivateMessageStatus = "pass"
		}
	}
	return r
}

func (m *Manager) Callback(ctx context.Context, user, token, message, chat string) (bool, error) {
	if user != m.OwnerID || !ValidRequestID(token) {
		return false, nil
	}
	return m.Store.ObserveVerificationCallback(ctx, m.HostID, m.AppID, m.OwnerID, token, message, chat, m.now().UnixMilli())
}

func (m *Manager) PrivateMessage(ctx context.Context, user, token, message string) (bool, error) {
	if user != m.OwnerID || !ValidRequestID(token) {
		return false, nil
	}
	return m.Store.ObserveVerificationPrivateMessage(ctx, m.HostID, m.AppID, m.OwnerID, token, message, m.now().UnixMilli())
}
