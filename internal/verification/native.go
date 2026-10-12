package verification

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type NativeCreator interface {
	StartLocalVerification(context.Context, codexclient.LocalVerificationSpec) (codexclient.Thread, error)
	GenerationID() uint64
	ConnectionID() string
	Done() <-chan struct{}
}

type NativeResult struct {
	RequestID         string `json:"request_id"`
	BridgeCommit      string `json:"bridge_commit"`
	BinarySHA256      string `json:"binary_sha256"`
	BuildChanged      bool   `json:"build_changed"`
	State             string `json:"state"`
	CreationStatus    string `json:"creation_status"`
	ThreadID          string `json:"thread_id,omitempty"`
	CreatorGeneration int64  `json:"creator_generation"`
	CreatorConnected  bool   `json:"creator_connected"`
	ExpiresAtMS       int64  `json:"expires_at_ms"`
	Expired           bool   `json:"expired"`
}

// NativeManager is the durable creation step for the future managed verifier.
// Current returns an ALREADY initialized service connection without I/O;
// it must never invoke Connect. Model server/dir lifecycle, inputs, binding,
// cleanup and CLI authorization remain with the owning verifier workflow.
type NativeManager struct {
	Store                                        *store.Store
	HostID, AppID, OwnerID, Commit, BinarySHA256 string
	Current                                      func() NativeCreator
	Now                                          func() time.Time
}

func (m *NativeManager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}
func (m *NativeManager) current() (NativeCreator, int64, string) {
	if m.Current == nil {
		return nil, 0, ""
	}
	n := m.Current()
	if n == nil {
		return nil, 0, ""
	}
	g, id, done := n.GenerationID(), n.ConnectionID(), n.Done()
	if g == 0 || g > math.MaxInt64 || !ValidRequestID(id) || done == nil {
		return nil, 0, ""
	}
	select {
	case <-done:
		return nil, 0, ""
	default:
		return n, int64(g), id
	}
}

func (m *NativeManager) Begin(ctx context.Context, id string, spec codexclient.LocalVerificationSpec) (NativeResult, error) {
	if !ValidRequestID(id) || spec.RunID != id || m.Store == nil || m.HostID == "" || m.AppID == "" || m.OwnerID == "" || m.Commit == "" || !ValidBinaryHash(m.BinarySHA256) {
		return NativeResult{}, ErrInvalid
	}
	request, err := codexclient.LocalVerificationRequestJSON(spec)
	if err != nil {
		return NativeResult{}, ErrInvalid
	}
	now := m.now()
	c, err := m.Store.PrepareVerificationNative(ctx, store.VerificationNative{RequestID: id, HostID: m.HostID, AppID: m.AppID, OwnerID: m.OwnerID, BridgeCommit: m.Commit, BinarySHA256: m.BinarySHA256, CWD: spec.CWD, ModelURL: spec.ModelURL, RequestJSON: request, CreatedAtMS: now.UnixMilli(), ExpiresAtMS: now.Add(5 * time.Minute).UnixMilli()})
	if err != nil {
		return NativeResult{}, err
	}
	if c.CWD != spec.CWD || c.ModelURL != spec.ModelURL || c.RequestJSON != request {
		return NativeResult{}, ErrInvalid
	}
	if c.BridgeCommit != m.Commit || c.BinarySHA256 != m.BinarySHA256 || c.State != "ready" || now.UnixMilli() >= c.ExpiresAtMS || ctx.Err() != nil {
		return m.result(c), ctx.Err()
	}
	n, generation, connection := m.current()
	if n == nil {
		return m.result(c), ErrNotReady
	}
	claimed, err := m.Store.ClaimVerificationNative(ctx, c, generation, connection, m.now().UnixMilli())
	if err != nil {
		return NativeResult{}, err
	}
	if !claimed {
		return m.State(ctx, id)
	}
	c.Generation = generation
	c.ConnectionID = connection
	// If the service replaced/disconnected this generation during the claim,
	// preserve unknown without sending on a new connection under this claim.
	_, current, currentConnection := m.current()
	var created codexclient.Thread
	var createErr error
	if current != generation || currentConnection != connection {
		createErr = ErrNotReady
	} else {
		created, createErr = n.StartLocalVerification(ctx, spec)
	}
	state, thread := "unknown", ""
	if createErr == nil && nativeBirthMatches(created, spec) {
		state, thread = "confirmed", created.ID
	} else {
		var rejected *codexclient.RPCError
		if errors.As(createErr, &rejected) {
			state = "rejected"
		}
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err = m.Store.FinishVerificationNative(finishCtx, c, state, thread); err != nil {
		return NativeResult{}, err
	}
	return m.State(finishCtx, id)
}

func nativeBirthMatches(t codexclient.Thread, s codexclient.LocalVerificationSpec) bool {
	var label string
	return t.ID != "" && t.CWD == s.CWD && !t.Ephemeral && t.ParentThreadID == nil && t.ForkedFromID == nil && t.Preview == "" && len(t.Turns) == 0 && json.Unmarshal(t.ThreadSource, &label) == nil && label == "feishu.bridge.validation"
}

func (m *NativeManager) State(ctx context.Context, id string) (NativeResult, error) {
	if !ValidRequestID(id) || m.Store == nil {
		return NativeResult{}, ErrInvalid
	}
	c, err := m.Store.VerificationNative(ctx, id, m.HostID, m.AppID, m.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return NativeResult{}, ErrNotFound
	}
	if err != nil {
		return NativeResult{}, err
	}
	return m.result(c), nil
}

func (m *NativeManager) result(c store.VerificationNative) NativeResult {
	_, generation, connection := m.current()
	r := NativeResult{RequestID: c.RequestID, BridgeCommit: c.BridgeCommit, BinarySHA256: c.BinarySHA256, BuildChanged: c.BridgeCommit != m.Commit || c.BinarySHA256 != m.BinarySHA256, State: c.State, CreationStatus: "pending", ThreadID: c.ThreadID, CreatorGeneration: c.Generation, CreatorConnected: generation > 0 && generation == c.Generation && connection == c.ConnectionID, ExpiresAtMS: c.ExpiresAtMS, Expired: m.now().UnixMilli() >= c.ExpiresAtMS}
	if c.State == "dispatching" {
		r.State = "unknown"
	}
	if c.State == "rejected" {
		r.CreationStatus = "fail"
	}
	if c.State == "confirmed" && c.ThreadID != "" {
		r.CreationStatus = "pass"
	}
	return r
}
