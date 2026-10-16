package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	discoveryCheckpointKind = "codex.discovery_checkpoint"
	pinnedSnapshotKind      = "codex.pinned_snapshot"
)

// DiscoveryCheckpoint records coverage, independently of the immutable birth fence.
type DiscoveryCheckpoint struct {
	LastScanAtMS int64 `json:"last_scan_at_ms"`
	LastFullAtMS int64 `json:"last_full_at_ms"`
}

// PinnedSnapshot is private checkpoint data, never diagnostic output. A section
// identity survives renaming; a complete set also survives reconnects.
type PinnedSnapshot struct {
	SectionID    string   `json:"section_id"`
	ThreadIDs    []string `json:"thread_ids"`
	ObservedAtMS int64    `json:"observed_at_ms"`
}

func pollingKey(kind, host, app string) string {
	scope, _ := json.Marshal([]string{host, app})
	return fmt.Sprintf("%s:%x", kind, sha256.Sum256(scope))
}

func (s *Store) readPollingCheckpoint(ctx context.Context, kind, host, app string, target any) error {
	if host == "" || app == "" {
		return ErrConflict
	}
	var raw, state string
	err := s.DB.QueryRowContext(ctx, `SELECT o.remote_id,o.state FROM m0_operations o
		WHERE o.key=? AND o.kind=? AND EXISTS(SELECT 1 FROM deployment WHERE host_id=? AND app_id=?)`,
		pollingKey(kind, host, app), kind, host, app).Scan(&raw, &state)
	if err != nil {
		return err
	}
	if state != "confirmed" || json.Unmarshal([]byte(raw), target) != nil {
		return ErrConflict
	}
	return nil
}

func (s *Store) savePollingCheckpoint(ctx context.Context, kind, host, app string, at int64, value any) error {
	if host == "" || app == "" || at <= 0 {
		return ErrConflict
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO m0_operations(key,kind,state,remote_id,created_at)
		SELECT ?,?,'confirmed',?,? FROM deployment WHERE host_id=? AND app_id=?
		ON CONFLICT(key) DO UPDATE SET remote_id=excluded.remote_id,created_at=excluded.created_at
		WHERE m0_operations.kind=excluded.kind AND m0_operations.state='confirmed'`,
		pollingKey(kind, host, app), kind, string(raw), at, host, app)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count != 1 {
		return ErrConflict
	}
	return nil
}

func validDiscoveryCheckpoint(c DiscoveryCheckpoint) bool {
	return c.LastScanAtMS > 0 && c.LastFullAtMS > 0 && c.LastFullAtMS <= c.LastScanAtMS
}

func (s *Store) DiscoveryCheckpoint(ctx context.Context, host, app string) (DiscoveryCheckpoint, error) {
	var c DiscoveryCheckpoint
	if err := s.readPollingCheckpoint(ctx, discoveryCheckpointKind, host, app, &c); err != nil {
		return c, err
	}
	if !validDiscoveryCheckpoint(c) {
		return DiscoveryCheckpoint{}, ErrConflict
	}
	return c, nil
}

func (s *Store) SaveDiscoveryCheckpoint(ctx context.Context, host, app string, c DiscoveryCheckpoint) error {
	if !validDiscoveryCheckpoint(c) {
		return ErrConflict
	}
	return s.savePollingCheckpoint(ctx, discoveryCheckpointKind, host, app, c.LastScanAtMS, c)
}

func validPinnedSnapshot(p PinnedSnapshot) bool {
	if p.ObservedAtMS <= 0 || p.SectionID == "" && len(p.ThreadIDs) != 0 {
		return false
	}
	seen := make(map[string]bool, len(p.ThreadIDs))
	for _, id := range p.ThreadIDs {
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func (s *Store) PinnedSnapshot(ctx context.Context, host, app string) (PinnedSnapshot, error) {
	var p PinnedSnapshot
	if err := s.readPollingCheckpoint(ctx, pinnedSnapshotKind, host, app, &p); err != nil {
		return p, err
	}
	if !validPinnedSnapshot(p) {
		return PinnedSnapshot{}, ErrConflict
	}
	return p, nil
}

func (s *Store) SavePinnedSnapshot(ctx context.Context, host, app string, p PinnedSnapshot) error {
	if !validPinnedSnapshot(p) {
		return ErrConflict
	}
	p.ThreadIDs = append([]string(nil), p.ThreadIDs...)
	sort.Strings(p.ThreadIDs)
	return s.savePollingCheckpoint(ctx, pinnedSnapshotKind, host, app, p.ObservedAtMS, p)
}
