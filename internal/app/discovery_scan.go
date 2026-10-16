package app

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"codex-feishu/internal/discovery"
)

const recentActivityWindow = 24 * time.Hour

// The rolling window bounds normal listings. Persisted coverage keeps the
// initial/long-disconnect/daily compensation path without resetting the fence.
func (r *Runtime) scanDiscovery(ctx context.Context, engine *discovery.Engine) error {
	now := r.now()
	checkpoint, err := r.Store.DiscoveryCheckpoint(ctx, r.HostID, r.AppID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	fullInterval := r.FullScanInterval
	if fullInterval <= 0 {
		fullInterval = recentActivityWindow
	}
	full := err != nil || checkpoint.LastScanAtMS > now.UnixMilli() ||
		now.UnixMilli()-checkpoint.LastScanAtMS >= recentActivityWindow.Milliseconds() ||
		now.UnixMilli()-checkpoint.LastFullAtMS >= fullInterval.Milliseconds()
	var since int64
	if !full {
		since = now.Add(-recentActivityWindow).Unix()
	}
	scanErr := engine.ScanSince(ctx, since)
	var candidate *discovery.CandidateError
	if scanErr != nil && !errors.As(scanErr, &candidate) {
		return scanErr
	}
	// Candidate failures are durably pending and do not invalidate complete
	// listing coverage. A failed list never reaches this checkpoint update.
	checkpoint.LastScanAtMS = now.UnixMilli()
	if full {
		checkpoint.LastFullAtMS = now.UnixMilli()
	}
	if err := r.Store.SaveDiscoveryCheckpoint(ctx, r.HostID, r.AppID, checkpoint); err != nil {
		return errors.Join(scanErr, err)
	}
	return scanErr
}
