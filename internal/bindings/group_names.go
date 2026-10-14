package bindings

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

// WithRobotPrefix marks task groups consistently, including titles that
// already carry the marker from an earlier sync or an App rename.
func WithRobotPrefix(title string) string {
	title = strings.TrimSpace(title)
	for strings.HasPrefix(title, "🤖") {
		title = strings.TrimSpace(strings.TrimPrefix(title, "🤖"))
	}
	if title == "" {
		title = "Codex"
	}
	return "🤖 " + title
}

var (
	ErrGroupNameUnknown  = errors.New("group name update awaits readback; redispatch disabled")
	ErrGroupNameRejected = errors.New("group name update was rejected")
	ErrGroupNameBusy     = errors.New("group name update is already in progress")
	ErrGroupNameDeferred = errors.New("group name update is waiting for its persisted rate-limit deadline")
)

type GroupNameDeferredError struct{ RetryAtMS int64 }

func (e *GroupNameDeferredError) Error() string { return ErrGroupNameDeferred.Error() }
func (e *GroupNameDeferredError) Unwrap() error { return ErrGroupNameDeferred }

type GroupNameGateway interface {
	GetGroupInfo(context.Context, string) (feishu.GroupInfo, error)
	UpdateGroupName(context.Context, string, string) error
	BotInChat(context.Context, string) (bool, error)
}

type GroupNames struct {
	Store          *store.Store
	Gateway        GroupNameGateway
	AppID, OwnerID string
	Now            func() time.Time
	BeginWrite     func(context.Context, store.Binding) (context.Context, func(), error)
	active         sync.Map
}

func (m *GroupNames) nowMS() int64 {
	if m.Now != nil {
		return m.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

func (m *GroupNames) binding(ctx context.Context, b store.Binding) error {
	current, err := m.Store.Binding(ctx, b.HostID, b.ThreadID)
	if err != nil {
		return err
	}
	if current != b {
		return store.ErrConflict
	}
	var app string
	if err := m.Store.DB.QueryRowContext(ctx, `SELECT app_id FROM deployment WHERE host_id=?`, b.HostID).Scan(&app); err != nil {
		return err
	}
	if app != m.AppID {
		return store.ErrConflict
	}
	return nil
}

func (m *GroupNames) read(ctx context.Context, b store.Binding) (feishu.GroupInfo, error) {
	present, err := m.Gateway.BotInChat(ctx, b.ChatID)
	if err != nil {
		return feishu.GroupInfo{}, err
	}
	if !present {
		return feishu.GroupInfo{}, store.ErrConflict
	}
	info, err := m.Gateway.GetGroupInfo(ctx, b.ChatID)
	if err != nil {
		return feishu.GroupInfo{}, err
	}
	if info.OwnerID != m.OwnerID || info.ChatMode != "group" {
		return feishu.GroupInfo{}, store.ErrConflict
	}
	return info, nil
}

func (m *GroupNames) begin(ctx context.Context, b store.Binding) (context.Context, func(), error) {
	opctx, release, err := m.BeginWrite(ctx, b)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, nil, err
	}
	if opctx == nil || release == nil {
		if release != nil {
			release()
		}
		return nil, nil, store.ErrConflict
	}
	if err := opctx.Err(); err != nil {
		release()
		return nil, nil, err
	}
	if err := m.binding(opctx, b); err != nil {
		release()
		return nil, nil, err
	}
	return opctx, release, nil
}

// Reconcile follows only the supplied persisted group binding. An unresolved
// frozen request is checked before the latest title and never sent again.
func (m *GroupNames) Reconcile(ctx context.Context, b store.Binding, desired string) error {
	if m.Store == nil || m.Gateway == nil || m.BeginWrite == nil || m.AppID == "" || m.OwnerID == "" || b.HostID == "" || b.ThreadID == "" || b.ChatID == "" || b.IntentID == "" || strings.TrimSpace(desired) == "" || !utf8.ValidString(desired) {
		return store.ErrConflict
	}
	// The actor already serializes a binding. Also reject direct concurrent
	// callers so readback cannot clear a request while our PUT is in flight.
	lock, _ := m.active.LoadOrStore(b, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	if !mu.TryLock() {
		return ErrGroupNameBusy
	}
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.binding(ctx, b); err != nil {
		return err
	}
	scope := store.GroupNameScope{AppID: m.AppID, OwnerID: m.OwnerID, Binding: b}
	previous, err := m.Store.GroupNameAttempt(ctx, scope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && previous.Plan.RetryAtMS > m.nowMS() {
		return &GroupNameDeferredError{RetryAtMS: previous.Plan.RetryAtMS}
	}
	if err == nil && previous.State == "rejected" && previous.Plan.RetryAtMS == 0 && previous.Plan.Desired == desired {
		return ErrGroupNameRejected
	}
	info, err := m.read(ctx, b)
	if err != nil {
		return err
	}
	if previous.State == "unknown" || previous.State == "dispatching" {
		if info.Name != previous.Plan.Desired {
			return ErrGroupNameUnknown
		}
		opctx, release, err := m.begin(ctx, b)
		if err != nil {
			return err
		}
		defer release()
		if err := m.Store.FinishGroupName(opctx, previous, "confirmed"); err != nil {
			return err
		}
		if desired != previous.Plan.Desired {
			return ErrGroupNameBusy
		}
		return nil
	}
	if info.Name == desired {
		return nil
	}
	opctx, release, err := m.begin(ctx, b)
	if err != nil {
		return err
	}
	defer release()
	plan := store.GroupNamePlan{Scope: scope, Desired: desired, Previous: info.Name}
	if previous.Plan.RetryAtMS > 0 {
		plan.RejectionCount = previous.Plan.RejectionCount
	}
	attempt, claimed, err := m.Store.ClaimGroupName(opctx, plan, previous.Key, m.nowMS())
	if err != nil {
		return err
	}
	if !claimed {
		return ErrGroupNameBusy
	}
	// Cancellation after a claim leaves dispatching for conservative readback.
	if err := opctx.Err(); err != nil {
		return err
	}
	remoteErr := m.Gateway.UpdateGroupName(opctx, b.ChatID, desired)
	if err := opctx.Err(); err != nil {
		return err
	}
	var limited *feishu.RateLimited
	if errors.As(remoteErr, &limited) {
		at := m.nowMS()
		if at < attempt.CreatedAtMS {
			at = attempt.CreatedAtMS
		}
		due, err := m.Store.DeferGroupName(opctx, attempt, at, limited.RetryAfter.Milliseconds())
		if err != nil {
			return err
		}
		return &GroupNameDeferredError{RetryAtMS: due}
	}
	state := "unknown"
	if remoteErr == nil {
		confirmed, readErr := m.Gateway.GetGroupInfo(opctx, b.ChatID)
		if readErr == nil && confirmed.Name == desired && confirmed.OwnerID == m.OwnerID && confirmed.ChatMode == "group" {
			state = "confirmed"
		}
	} else {
		var rejected *feishu.Rejected
		if errors.As(remoteErr, &rejected) {
			state = "rejected"
		}
	}
	if err := opctx.Err(); err != nil {
		return err
	}
	if err := m.Store.FinishGroupName(opctx, attempt, state); err != nil {
		return err
	}
	switch state {
	case "confirmed":
		return nil
	case "rejected":
		return ErrGroupNameRejected
	default:
		return ErrGroupNameUnknown
	}
}
