package store

import (
	"context"
	"database/sql"
	"errors"
)

type PersistentHealth struct {
	Schema        int              `json:"schema"`
	LegacyM0      bool             `json:"legacy_m0"`
	Bindings      int64            `json:"bindings"`
	Inputs        map[string]int64 `json:"inputs"`
	Outbound      map[string]int64 `json:"outbound"`
	NativeActions map[string]int64 `json:"native_actions"`
	Requests      map[string]int64 `json:"requests"`
}

// Health returns aggregate durable facts only, never bodies, card documents,
// error payloads, credentials or guesses about live socket readiness.
func (s *Store) Health(ctx context.Context, schema int, host, app string) (PersistentHealth, error) {
	h := PersistentHealth{Schema: schema, LegacyM0: schema == 1, Inputs: map[string]int64{}, Outbound: map[string]int64{}, NativeActions: map[string]int64{}, Requests: map[string]int64{}}
	count := func(query string, args ...any) (int64, error) {
		var n int64
		err := s.DB.QueryRowContext(ctx, query, args...).Scan(&n)
		return n, err
	}
	var err error
	if schema == 1 {
		h.Bindings, err = count(`SELECT COUNT(*) FROM m0_bindings`)
	} else {
		var actual string
		err = s.DB.QueryRowContext(ctx, `SELECT app_id FROM deployment WHERE host_id=?`, host).Scan(&actual)
		if errors.Is(err, sql.ErrNoRows) {
			return h, nil
		}
		if err != nil || actual != app {
			return h, errors.New("database deployment differs from configuration")
		}
		h.Bindings, err = count(`SELECT COUNT(*) FROM bindings WHERE host_id=?`, host)
	}
	if err != nil {
		return h, errors.New("binding health unavailable")
	}
	for _, state := range []string{"received", "submitting", "accepted", "queued", "consumed", "steering", "steered", "removing", "removed", "unknown", "failed"} {
		if schema == 1 {
			h.Inputs[state], err = count(`SELECT COUNT(*) FROM m0_receipts WHERE app_id=? AND state=?`, app, state)
		} else {
			h.Inputs[state], err = count(`SELECT COUNT(*) FROM m0_receipts r JOIN bindings b ON b.thread_id=r.thread_id AND b.chat_id=r.chat_id WHERE b.host_id=? AND r.app_id=? AND r.state=?`, host, app, state)
		}
		if err != nil {
			return h, errors.New("input health unavailable")
		}
	}
	if schema >= 3 {
		for _, state := range []string{"ready", "dispatching", "unknown", "rejected", "confirmed"} {
			h.Outbound[state], err = count(`SELECT COUNT(*) FROM outbound WHERE host_id=? AND state=?`, host, state)
			if err != nil {
				return h, errors.New("outbound health unavailable")
			}
		}
	}
	if schema >= 6 {
		for _, state := range []string{"ready", "pending", "unknown", "rejected", "confirmed"} {
			h.NativeActions[state], err = count(`SELECT COUNT(*) FROM submission_actions WHERE host_id=? AND app_id=? AND state=?`, host, app, state)
			if err != nil {
				return h, errors.New("native action health unavailable")
			}
		}
	}
	if schema >= 9 {
		h.Requests["pending"], err = count(`SELECT COUNT(*) FROM app_requests WHERE host_id=? AND app_id=? AND state='pending'`, host, app)
		if err != nil {
			return h, errors.New("request health unavailable")
		}
		h.Requests["awaiting_reconciliation"], err = count(`SELECT COUNT(*) FROM app_requests WHERE host_id=? AND app_id=? AND state='pending' AND confirmed_pending=0`, host, app)
		if err != nil {
			return h, errors.New("request health unavailable")
		}
		for _, state := range []string{"ready", "unknown", "rejected", "confirmed"} {
			h.Requests["reminder_"+state], err = count(`SELECT COUNT(*) FROM app_requests WHERE host_id=? AND app_id=? AND reminder_state=?`, host, app, state)
			if err != nil {
				return h, errors.New("reminder health unavailable")
			}
		}
	}
	return h, nil
}
