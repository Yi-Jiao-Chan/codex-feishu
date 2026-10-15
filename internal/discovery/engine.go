// Package discovery records birth evidence before considering automatic groups.
package discovery

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

type Native interface {
	ListArchive(context.Context, bool) ([]codexclient.Thread, error)
	LoadedIDs(context.Context) ([]string, error)
	Metadata(context.Context, string) (codexclient.Thread, error)
	Turns(context.Context, string) ([]codexclient.Turn, error)
}

type Engine struct {
	Native         Native
	Store          *store.Store
	HostID, AppID  string
	AllowedRoots   []string
	DirectoryRules func(context.Context) ([]rules.DirectoryRule, error)
	Now            func() time.Time
}

// CandidateError isolates a temporarily unavailable thread from other scans.
// Error intentionally omits native payloads, paths and message bodies.
type CandidateError struct {
	ThreadID, Stage string
	Cause           error
}

func (e *CandidateError) Error() string { return "discovery candidate awaits " + e.Stage }
func (e *CandidateError) Unwrap() error { return e.Cause }

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Begin runs before draining birth events or starting the first list snapshot.
// All reconnects reuse this millisecond fence, including an interrupted baseline.
func (e *Engine) Begin(ctx context.Context) (bool, error) {
	return e.Store.BeginBaseline(ctx, e.HostID, e.AppID, e.now().UnixMilli())
}

// ObserveBirth is invoked for thread/started while snapshot requests are running.
// It does not trust thin notification fields as eligibility metadata or input.
func (e *Engine) ObserveBirth(ctx context.Context, t codexclient.Thread) error {
	if err := e.Store.ObserveCandidate(ctx, e.HostID, t.ID, t.CWD, t.CreatedAt, "birth"); err != nil {
		return err
	}
	if t.BridgeCreated() {
		return e.Store.Exclude(ctx, e.HostID, t.ID, "bridge_creation")
	}
	return nil
}

// Snapshot is intentionally separate from Begin: the composition root must keep
// consuming and persisting thread/started during every awaited native request.
// List/read failures leave the generation incomplete; no remote writes happen.
func (e *Engine) Snapshot(ctx context.Context) error {
	fence, complete, err := e.Store.BaselineFence(ctx, e.HostID)
	if err != nil {
		return err
	}
	if complete {
		return nil
	}
	active, err := e.Native.ListArchive(ctx, false)
	if err != nil {
		return err
	}
	archived, err := e.Native.ListArchive(ctx, true)
	if err != nil {
		return err
	}
	loaded, err := e.Native.LoadedIDs(ctx)
	if err != nil {
		return err
	}
	listed := map[string]codexclient.Thread{}
	for _, t := range append(active, archived...) {
		listed[t.ID] = t
	}
	ids := map[string]bool{}
	for id := range listed {
		ids[id] = true
	}
	for _, id := range loaded {
		ids[id] = true
	}
	var historical []string
	for id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		o, err := e.Store.Observation(ctx, e.HostID, id)
		if err == nil && o.Origin == "new" {
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		t, err := e.Native.Metadata(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err = e.Store.ObserveCandidate(ctx, e.HostID, id, listed[id].CWD, listed[id].CreatedAt, "ambiguous"); err != nil {
				return err
			}
			if err = e.Store.NotePending(ctx, e.HostID, id, "metadata_unavailable"); err != nil {
				return err
			}
			continue
		}
		if t.ID != id {
			return errors.New("native metadata returned another thread")
		}
		if t.ForkedFromID != nil {
			// TODO: Support fork-local input activation and inherited history when
			// fork following is requested. The user deferred this uncommon case.
			if err = e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, "ambiguous"); err != nil {
				return err
			}
			if err = e.Store.Exclude(ctx, e.HostID, id, "fork_deferred"); err != nil {
				return err
			}
			continue
		}
		if bornAfterFence(id, fence, e.now().UnixMilli()) {
			if err = e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, "v7"); err != nil {
				return err
			}
			continue
		}
		// Out-of-scope/internal history is ignored without loading its bodies.
		scope, scopeErr := rules.ResolveScope(ctx, t.CWD, e.AllowedRoots)
		if !t.InteractiveMain() || scopeErr == nil && !scope.Allowed {
			historical = append(historical, id)
			continue
		}
		if scopeErr != nil {
			if err = e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, "ambiguous"); err != nil {
				return err
			}
			if err = e.Store.NotePending(ctx, e.HostID, id, "cwd_unavailable"); err != nil {
				return err
			}
			continue
		}
		turns, err := e.Native.Turns(ctx, id)
		if codexclient.BeforeFirstInput(err) {
			err = e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, "draft")
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err = e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, "ambiguous"); err != nil {
				return err
			}
			if err = e.Store.NotePending(ctx, e.HostID, id, "history_unavailable"); err != nil {
				return err
			}
			continue
		}
		input := firstInput(turns)
		if input == "" {
			// Full history proving no actual user input is a blank before-state.
			if err = e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, "draft"); err != nil {
				return err
			}
			continue
		}
		if historyPredatesFence(turns, fence) {
			// A new birth written concurrently wins the atomic baseline INSERT.
			historical = append(historical, id)
		} else {
			// An old loaded draft may get its first input before we prove blank.
			// Missing/coarse turn timing cannot silently turn this into history.
			if err = e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, "ambiguous"); err != nil {
				return err
			}
			if err = e.Store.NotePending(ctx, e.HostID, id, "first_input_boundary_unknown"); err != nil {
				return err
			}
		}
	}
	return e.Store.CompleteBaseline(ctx, e.HostID, historical)
}

// Scan includes archived IDs for coverage but never activates them while archived.
// Normal native UUIDv7 IDs provide conditional compensation for missed/offline
// births; recency/preview/createdAt alone never promote old IDs. Repeated full
// scans avoid silently skipping delayed projections behind a recency watermark.
func (e *Engine) Scan(ctx context.Context) error {
	fence, complete, err := e.Store.BaselineFence(ctx, e.HostID)
	if err != nil {
		return err
	}
	if !complete {
		return errors.New("discovery baseline incomplete")
	}
	active, err := e.Native.ListArchive(ctx, false)
	if err != nil {
		return err
	}
	archived, err := e.Native.ListArchive(ctx, true)
	if err != nil {
		return err
	}
	loaded, err := e.Native.LoadedIDs(ctx)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	archivedIDs := map[string]bool{}
	archivedMetadata := map[string]codexclient.Thread{}
	for _, t := range active {
		ids[t.ID] = true
	}
	for _, t := range archived {
		ids[t.ID] = true
		archivedIDs[t.ID] = true
		archivedMetadata[t.ID] = t
	}
	for _, id := range loaded {
		ids[id] = true
	}
	pending, err := e.Store.PendingObservations(ctx, e.HostID)
	if err != nil {
		return err
	}
	for _, o := range pending {
		ids[o.ThreadID] = true
	}
	var unavailable []error
	for id := range ids {
		if archivedIDs[id] {
			t := archivedMetadata[id]
			proof := "ambiguous"
			if bornAfterFence(id, fence, e.now().UnixMilli()) {
				proof = "v7"
			}
			if err = e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, proof); err != nil {
				return err
			}
			if err = e.Store.HoldCandidate(ctx, e.HostID, id, "archived"); err != nil {
				return err
			}
			continue
		}
		if err = e.Check(ctx, id, fence); err != nil {
			var candidate *CandidateError
			if errors.As(err, &candidate) {
				if err = e.Store.NotePending(ctx, e.HostID, id, candidate.Stage+"_unavailable"); err != nil {
					return err
				}
				unavailable = append(unavailable, candidate)
				continue
			}
			return err
		}
	}
	return errors.Join(unavailable...)
}

func (e *Engine) Check(ctx context.Context, id string, fence int64) error {
	o, err := e.Store.Observation(ctx, e.HostID, id)
	if err == nil && (o.State == "ignored" || o.State == "excluded") {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = e.Store.Binding(ctx, e.HostID, id); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = e.Store.ObserveCandidate(ctx, e.HostID, id, o.CWD, o.CreatedAt, "ambiguous"); err != nil {
		return err
	}
	t, err := e.Native.Metadata(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &CandidateError{ThreadID: id, Stage: "metadata", Cause: err}
	}
	if t.ID != id {
		return errors.New("native metadata returned another thread")
	}
	proof := "ambiguous"
	if bornAfterFence(id, fence, e.now().UnixMilli()) {
		proof = "v7"
	}
	if err = e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, proof); err != nil {
		return err
	}
	o, err = e.Store.Observation(ctx, e.HostID, id)
	if err != nil {
		return err
	}
	if t.ForkedFromID != nil {
		// TODO: Human fork following is deferred by the user. Detailed metadata
		// is required here because thread/list can omit forkedFromId in 0.162.0.
		return e.Store.Exclude(ctx, e.HostID, id, "fork_deferred")
	}
	if !t.InteractiveMain() {
		return e.Store.Exclude(ctx, e.HostID, id, "not_interactive_main")
	}
	scope, err := rules.ResolveScope(ctx, t.CWD, e.AllowedRoots)
	if err != nil {
		return &CandidateError{ThreadID: id, Stage: "cwd", Cause: err}
	}
	if !scope.Allowed {
		return e.Store.Exclude(ctx, e.HostID, id, "outside_allowed_roots")
	}
	if e.DirectoryRules != nil {
		dirs, err := e.DirectoryRules(ctx)
		if err != nil {
			return err
		}
		if _, muted := rules.Muted(scope, dirs); muted {
			return e.Store.Exclude(ctx, e.HostID, id, "directory_muted")
		}
	}
	turns, err := e.Native.Turns(ctx, id)
	if codexclient.BeforeFirstInput(err) {
		return e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, "draft")
	}
	if err != nil {
		return &CandidateError{ThreadID: id, Stage: "history", Cause: err}
	}
	input := firstInput(turns)
	if input == "" {
		return e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, "draft")
	}
	if o.Origin != "new" {
		return nil
	}
	return e.Store.MarkEligible(ctx, e.HostID, id, input, scope.CWD, scope.ProjectRoot)
}

func firstInput(turns []codexclient.Turn) string {
	for _, turn := range turns {
		for _, item := range turn.Items {
			if item.Type == "userMessage" && item.ID != "" {
				return item.ID
			}
		}
	}
	return ""
}

func historyPredatesFence(turns []codexclient.Turn, fence int64) bool {
	for _, turn := range turns {
		for _, item := range turn.Items {
			if item.Type == "userMessage" && item.ID != "" {
				return turn.StartedAt != nil && *turn.StartedAt >= 0 && *turn.StartedAt < fence/1000
			}
		}
	}
	return false
}

func bornAfterFence(id string, fence, now int64) bool {
	millis, ok := uuidMillis(id)
	return ok && now >= fence && millis > fence && millis <= now
}

func uuidMillis(id string) (int64, bool) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return 0, false
	}
	raw := id[0:8] + id[9:13] + id[14:18] + id[19:23] + id[24:]
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) != 16 || b[6]>>4 != 7 || b[8]&0xc0 != 0x80 {
		return 0, false
	}
	var millis int64
	for _, v := range b[:6] {
		millis = millis<<8 | int64(v)
	}
	return millis, true
}
