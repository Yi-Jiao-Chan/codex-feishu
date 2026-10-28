package discovery

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/latency"
)

// CheckEvents shares one complete active/archive lookup across a batch of event
// IDs. Events identify candidates; only the lists prove current active status.
// This partial check does not advance the full discovery coverage checkpoint.
func (e *Engine) CheckEvents(ctx context.Context, ids []string, since int64) error {
	fence, complete, err := e.Store.BaselineFence(ctx, e.HostID)
	if err != nil {
		return err
	}
	if !complete {
		return errors.New("discovery baseline incomplete")
	}
	candidates := make(map[string]bool, len(ids))
	archivedHolds := make(map[string]bool)
	for _, id := range ids {
		if id == "" || candidates[id] {
			continue
		}
		if bound, err := e.observeBound(ctx, id, codexclient.Thread{}, false); err != nil || bound {
			if err != nil {
				return err
			}
			continue
		}
		o, err := e.Store.Observation(ctx, e.HostID, id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if o.State == "ignored" || o.State == "excluded" {
			continue
		}
		if errors.Is(err, sql.ErrNoRows) {
			if err := e.Store.ObserveCandidate(ctx, e.HostID, id, "", 0, "ambiguous"); err != nil {
				return err
			}
		}
		candidates[id] = true
		archivedHolds[id] = o.Archived()
	}
	if len(candidates) == 0 {
		return nil
	}
	hold := func(id, reason string) error {
		// A native lookup deadline must not also cancel the local operation
		// that revokes stale eligibility before notifying the runtime.
		holdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return e.Store.HoldCandidate(holdCtx, e.HostID, id, reason)
	}
	holdUnconfirmed := func(cause error) error {
		for id := range candidates {
			reason := "active_unconfirmed"
			if archivedHolds[id] {
				reason = "archived"
			}
			if err := hold(id, reason); err != nil {
				return errors.Join(cause, err)
			}
		}
		return cause
	}
	list := func(archived bool) ([]codexclient.Thread, error) {
		started := time.Now()
		var threads []codexclient.Thread
		var err error
		if recent, ok := e.Native.(recentNative); ok && since > 0 {
			threads, err = recent.ListArchiveSince(ctx, archived, since)
		} else {
			threads, err = e.Native.ListArchive(ctx, archived)
		}
		stage := "discovery.active_list"
		if archived {
			stage = "discovery.archive_list"
		}
		e.Latency.Record(stage, e.HostID, "", "", "", time.Since(started), latency.Result(err))
		return threads, err
	}
	active, err := list(false)
	if err != nil {
		return holdUnconfirmed(err)
	}
	archived, err := list(true)
	if err != nil {
		return holdUnconfirmed(err)
	}
	activeMetadata, archivedMetadata := map[string]codexclient.Thread{}, map[string]codexclient.Thread{}
	for _, t := range active {
		if candidates[t.ID] {
			activeMetadata[t.ID] = t
		}
	}
	for _, t := range archived {
		if candidates[t.ID] {
			archivedMetadata[t.ID] = t
		}
	}
	var unavailable []error
	for id := range candidates {
		// As in compensation scans, an archived row takes precedence if the
		// native generation inconsistently returns the ID in both lists.
		if t, ok := archivedMetadata[id]; ok {
			if err := hold(id, "archived"); err != nil {
				return err
			}
			proof := "ambiguous"
			if bornAfterFence(id, fence, e.now().UnixMilli()) {
				proof = "v7"
			}
			if err := e.Store.ObserveCandidate(ctx, e.HostID, id, t.CWD, t.CreatedAt, proof); err != nil {
				return err
			}
			continue
		}
		t, active := activeMetadata[id]
		var metadata *codexclient.Thread
		if active {
			metadata = &t
		}
		if err := e.check(ctx, id, fence, active, metadata, true); err != nil {
			var candidate *CandidateError
			reason := "candidate_unavailable"
			if errors.As(err, &candidate) {
				reason = candidate.Stage + "_unavailable"
			}
			if archivedHolds[id] && !active {
				reason = "archived"
			}
			if holdErr := hold(id, reason); holdErr != nil {
				return errors.Join(err, holdErr)
			}
			if candidate == nil {
				return err
			}
			unavailable = append(unavailable, candidate)
		}
	}
	return errors.Join(unavailable...)
}
