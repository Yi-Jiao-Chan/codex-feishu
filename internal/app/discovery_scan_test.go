package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/discovery"
)

type windowRuntimeNative struct {
	*runtimeNative
	fullCalls, recentCalls int
	cutoffs                []int64
	recentErr, metadataErr error
}

func (n *windowRuntimeNative) ListArchive(ctx context.Context, archived bool) ([]codexclient.Thread, error) {
	n.fullCalls++
	return n.runtimeNative.ListArchive(ctx, archived)
}

func (n *windowRuntimeNative) ListArchiveSince(ctx context.Context, archived bool, since int64) ([]codexclient.Thread, error) {
	n.recentCalls++
	n.cutoffs = append(n.cutoffs, since)
	if n.recentErr != nil {
		return nil, n.recentErr
	}
	return n.runtimeNative.ListArchive(ctx, archived)
}

func (n *windowRuntimeNative) Metadata(ctx context.Context, id string) (codexclient.Thread, error) {
	if n.metadataErr != nil {
		return codexclient.Thread{}, n.metadataErr
	}
	return n.runtimeNative.Metadata(ctx, id)
}

func TestDiscoveryWindowCheckpointsAndCompensatesAfterLongDisconnect(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	native := &windowRuntimeNative{runtimeNative: base}
	now := time.Now()
	r.Now = func() time.Time { return now }
	r.FullScanInterval = 48 * time.Hour
	engine := &discovery.Engine{Store: db, Native: native, HostID: r.HostID, AppID: r.AppID, AllowedRoots: r.AllowedRoots, Now: r.Now}
	ctx := context.Background()
	if _, err := engine.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	native.fullCalls = 0
	if err := r.scanDiscovery(ctx, engine); err != nil || native.fullCalls != 2 || native.recentCalls != 0 {
		t.Fatal("initial compensation was not full", err)
	}
	first, err := db.DiscoveryCheckpoint(ctx, r.HostID, r.AppID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	if err := r.scanDiscovery(ctx, engine); err != nil || native.fullCalls != 2 || native.recentCalls != 2 {
		t.Fatal("normal poll repeated the full history scan", err)
	}
	want := now.Add(-24 * time.Hour).Unix()
	if len(native.cutoffs) != 2 || native.cutoffs[0] != want || native.cutoffs[1] != want {
		t.Fatal("active and archived listings used different windows")
	}
	before, err := db.DiscoveryCheckpoint(ctx, r.HostID, r.AppID)
	if err != nil || before.LastFullAtMS != first.LastFullAtMS {
		t.Fatal("window poll changed the full coverage checkpoint", err)
	}
	native.recentErr = errors.New("fixture list unavailable")
	now = now.Add(15 * time.Second)
	if err := r.scanDiscovery(ctx, engine); err == nil {
		t.Fatal("failed listing was accepted")
	}
	after, err := db.DiscoveryCheckpoint(ctx, r.HostID, r.AppID)
	if err != nil || after != before {
		t.Fatal("failed listing advanced coverage", err)
	}
	native.recentErr = nil
	// Full-scan interval is 48h; a 25h disconnect still needs compensation.
	now = now.Add(25 * time.Hour)
	if err := r.scanDiscovery(ctx, engine); err != nil || native.fullCalls != 4 {
		t.Fatal("long disconnect stayed inside the rolling window", err)
	}
	fence, complete, err := db.BaselineFence(ctx, r.HostID)
	if err != nil || !complete || fence != first.LastFullAtMS {
		t.Fatal("compensation reset the immutable birth fence", err)
	}
}

func TestDiscoveryDailyCompensationAndPendingCandidateCoverage(t *testing.T) {
	db, base, _, r := runtimeFixture(t)
	native := &windowRuntimeNative{runtimeNative: base}
	now := time.Now()
	r.Now = func() time.Time { return now }
	r.FullScanInterval = time.Hour
	engine := &discovery.Engine{Store: db, Native: native, HostID: r.HostID, AppID: r.AppID, AllowedRoots: r.AllowedRoots, Now: r.Now}
	ctx := context.Background()
	if _, err := engine.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.scanDiscovery(ctx, engine); err != nil {
		t.Fatal(err)
	}
	fullCalls := native.fullCalls
	base.set("candidate", r.AllowedRoots[0], "completed")
	native.metadataErr = errors.New("fixture candidate unavailable")
	now = now.Add(time.Minute)
	if err := r.scanDiscovery(ctx, engine); err == nil {
		t.Fatal("candidate error was not surfaced")
	}
	checkpoint, err := db.DiscoveryCheckpoint(ctx, r.HostID, r.AppID)
	if err != nil || checkpoint.LastScanAtMS != now.UnixMilli() {
		t.Fatal("durably pending metadata failure invalidated complete list coverage", err)
	}
	native.metadataErr = nil
	now = now.Add(61 * time.Minute)
	if err := r.scanDiscovery(ctx, engine); err != nil || native.fullCalls != fullCalls+2 {
		t.Fatal("low-frequency compensation did not run", err)
	}
}
