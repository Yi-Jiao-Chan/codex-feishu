package store

import (
	"context"
	"errors"
)

// HasDueMirrors is a local wakeup hint, not dispatch authorization. Claims still
// recheck the journal, binding, parent and deadlines before any remote write.
// Unknown and dispatching work never become candidates for a blind retry.
func (s *Store) HasDueMirrors(ctx context.Context, host, thread string, nowMS, progressIntervalMS int64) (bool, error) {
	if nowMS <= 0 {
		return false, errors.New("invalid mirror selection time")
	}
	if progressIntervalMS == 0 {
		progressIntervalMS = 2000
	}
	if progressIntervalMS < 100 || progressIntervalMS > 10000 {
		return false, errors.New("invalid progress interval")
	}
	var due bool
	err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM mirrors m JOIN outbound o USING(host_id,thread_id,mirror_key)
		WHERE m.host_id=? AND m.thread_id=? AND o.state='ready'
		AND NOT EXISTS(SELECT 1 FROM outbound_throttles t
			WHERE t.host_id=m.host_id AND t.thread_id=m.thread_id AND t.mirror_key=m.mirror_key AND t.next_retry_at_ms>?)
		AND (m.kind!='progress' OR m.terminal=1 OR NOT EXISTS(
			SELECT 1 FROM outbound_attempts a WHERE a.host_id=m.host_id AND a.thread_id=m.thread_id
			AND a.mirror_key=m.mirror_key AND a.created_at_ms>?))
	)`, host, thread, nowMS, nowMS-progressIntervalMS).Scan(&due)
	return due, err
}
