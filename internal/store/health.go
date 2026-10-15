package store

import (
	"context"
	"database/sql"
	"errors"
)

type PersistentHealth struct {
	Schema           int              `json:"schema"`
	LegacyM0         bool             `json:"legacy_m0"`
	Bindings         int64            `json:"bindings"`
	BaselineComplete bool             `json:"baseline_complete"`
	Inputs           map[string]int64 `json:"inputs"`
	Outbound         map[string]int64 `json:"outbound"`
	OutboundAttempts map[string]int64 `json:"outbound_attempts"`
	NativeActions    map[string]int64 `json:"native_actions"`
	NativeAttempts   map[string]int64 `json:"native_attempts"`
	CommandAttempts  map[string]int64 `json:"command_reply_attempts"`
	CommandReplies   map[string]int64 `json:"command_replies"`
	Reactions        map[string]int64 `json:"reactions"`
	GroupAttempts    map[string]int64 `json:"group_attempts"`
	GroupIntents     map[string]int64 `json:"group_intents"`
	GroupNames       map[string]int64 `json:"group_names"`
	TaskControls     map[string]int64 `json:"task_controls"`
	TaskClaims       map[string]int64 `json:"task_claims"`
	Requests         map[string]int64 `json:"requests"`
	ReminderAttempts map[string]int64 `json:"reminder_attempts"`
	CreationDrafts   map[string]int64 `json:"creation_drafts"`
	CreationIntents  map[string]int64 `json:"creation_intents"`
	CreationAttempts map[string]int64 `json:"creation_attempts"`
	FollowTargets    map[string]int64 `json:"follow_targets"`
}

// Health returns aggregate durable facts only, never bodies, card documents,
// error payloads, credentials or guesses about live socket readiness.
func (s *Store) Health(ctx context.Context, schema int, host, app string) (PersistentHealth, error) {
	h := PersistentHealth{Schema: schema, LegacyM0: schema == 1,
		Inputs: map[string]int64{}, Outbound: map[string]int64{}, OutboundAttempts: map[string]int64{}, NativeActions: map[string]int64{}, NativeAttempts: map[string]int64{},
		CommandAttempts: map[string]int64{}, CommandReplies: map[string]int64{}, Reactions: map[string]int64{}, GroupAttempts: map[string]int64{}, GroupIntents: map[string]int64{}, GroupNames: map[string]int64{}, TaskControls: map[string]int64{}, TaskClaims: map[string]int64{}, Requests: map[string]int64{}, ReminderAttempts: map[string]int64{}, CreationDrafts: map[string]int64{}, CreationIntents: map[string]int64{}, CreationAttempts: map[string]int64{}, FollowTargets: map[string]int64{}}
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
		var completed int64
		err = s.DB.QueryRowContext(ctx, `SELECT app_id,baseline_completed_at FROM deployment WHERE host_id=?`, host).Scan(&actual, &completed)
		if errors.Is(err, sql.ErrNoRows) {
			return h, nil
		}
		if err != nil || actual != app {
			return h, errors.New("database deployment differs from configuration")
		}
		h.BaselineComplete = completed > 0
		h.Bindings, err = count(`SELECT COUNT(*) FROM bindings WHERE host_id=?`, host)
	}
	if err != nil {
		return h, errors.New("binding health unavailable")
	}
	if schema >= 2 {
		h.GroupNames, err = s.GroupNameHealth(ctx, host, app)
		if err != nil {
			return h, errors.New("group name health unavailable")
		}
	}
	for _, state := range []string{"draft", "collecting", "received", "submitting", "accepted", "queued", "consumed", "steering", "steered", "removing", "removed", "unknown", "failed"} {
		if schema == 1 {
			h.Inputs[state], err = count(`SELECT COUNT(*) FROM m0_receipts WHERE app_id=? AND state=?`, app, state)
		} else {
			if schema >= 5 {
				h.Inputs[state], err = count(`SELECT COUNT(*) FROM m0_receipts r JOIN bindings b ON b.thread_id=r.thread_id AND b.chat_id=r.chat_id LEFT JOIN submission_inputs n ON n.receipt_id=r.id AND n.host_id=b.host_id AND n.app_id=r.app_id WHERE b.host_id=? AND r.app_id=? AND COALESCE(n.state,r.state)=?`, host, app, state)
			} else {
				h.Inputs[state], err = count(`SELECT COUNT(*) FROM m0_receipts r JOIN bindings b ON b.thread_id=r.thread_id AND b.chat_id=r.chat_id WHERE b.host_id=? AND r.app_id=? AND r.state=?`, host, app, state)
			}
		}
		if err != nil {
			return h, errors.New("input health unavailable")
		}
	}
	countStates := func(dst map[string]int64, states []string, query string, args ...any) error {
		for _, state := range states {
			stateArgs := append(append([]any(nil), args...), state)
			n, err := count(query, stateArgs...)
			if err != nil {
				return errors.New("operation health unavailable")
			}
			dst[state] = n
		}
		return nil
	}
	remoteStates := []string{"ready", "dispatching", "unknown", "rejected", "confirmed"}
	attemptStates := []string{"dispatching", "unknown", "rejected", "confirmed"}
	reactionStates := []string{"changing", "unknown", "rejected", "confirmed"}
	if schema == 1 {
		err = countStates(h.Reactions, reactionStates, `SELECT COUNT(*) FROM m0_reactions x JOIN m0_receipts r ON r.id=x.receipt_id WHERE r.app_id=? AND x.state=?`, app)
	} else {
		err = countStates(h.Reactions, reactionStates, `SELECT COUNT(*) FROM m0_reactions x JOIN m0_receipts r ON r.id=x.receipt_id JOIN bindings b ON b.thread_id=r.thread_id AND b.chat_id=r.chat_id WHERE b.host_id=? AND r.app_id=? AND x.state=?`, host, app)
	}
	if err != nil {
		return h, err
	}
	if schema >= 12 {
		h.Reactions["deferred"], err = count(`SELECT COUNT(*) FROM reaction_throttles t JOIN m0_receipts r ON r.id=t.receipt_id JOIN bindings b ON b.thread_id=r.thread_id AND b.chat_id=r.chat_id WHERE b.host_id=? AND r.app_id=?`, host, app)
		if err != nil {
			return h, errors.New("reaction retry health unavailable")
		}
	}
	if schema >= 2 {
		if err := countStates(h.GroupIntents, remoteStates, `SELECT COUNT(*) FROM create_intents WHERE host_id=? AND state=?`, host); err != nil {
			return h, err
		}
	}
	if schema >= 13 {
		if err := countStates(h.GroupAttempts, attemptStates, `SELECT COUNT(*) FROM group_attempts a JOIN create_intents i USING(intent_id) WHERE i.host_id=? AND a.state=?`, host); err != nil {
			return h, err
		}
		h.GroupIntents["deferred"], err = count(`SELECT COUNT(*) FROM group_throttles t JOIN create_intents i USING(intent_id) WHERE i.host_id=?`, host)
		if err != nil {
			return h, errors.New("group retry health unavailable")
		}
	}
	if schema >= 3 {
		for _, state := range []string{"ready", "dispatching", "unknown", "rejected", "confirmed"} {
			h.Outbound[state], err = count(`SELECT COUNT(*) FROM outbound WHERE host_id=? AND state=?`, host, state)
			if err != nil {
				return h, errors.New("outbound health unavailable")
			}
		}
		if err := countStates(h.OutboundAttempts, attemptStates, `SELECT COUNT(*) FROM outbound_attempts WHERE host_id=? AND state=?`, host); err != nil {
			return h, err
		}
	}
	if schema >= 5 {
		for _, state := range []string{"ready", "running", "unknown", "rejected", "confirmed"} {
			h.NativeActions[state], err = count(`SELECT COUNT(*) FROM submission_actions WHERE host_id=? AND app_id=? AND state=?`, host, app, state)
			if err != nil {
				return h, errors.New("native action health unavailable")
			}
		}
		if err := countStates(h.NativeAttempts, attemptStates, `SELECT COUNT(*) FROM submission_attempts WHERE host_id=? AND app_id=? AND state=?`, host, app); err != nil {
			return h, err
		}
		// ResolveSubmissionAction deliberately preserves ambiguous RPC attempts
		// after observable native evidence has settled the action's result. Keep
		// raw unknown counts for audit, but distinguish currently unresolved work.
		h.NativeAttempts["awaiting_reconciliation"], err = count(`SELECT COUNT(*) FROM submission_attempts a
		 LEFT JOIN submission_actions o ON o.host_id=a.host_id AND o.app_id=a.app_id AND o.thread_id=a.thread_id AND o.action_id=a.action_id
		 LEFT JOIN submission_inputs n ON n.host_id=a.host_id AND n.app_id=a.app_id AND n.thread_id=a.thread_id AND n.receipt_id=a.receipt_id
		 WHERE a.host_id=? AND a.app_id=? AND a.state='unknown' AND
		 ((a.action_id<>'' AND (o.action_id IS NULL OR o.state NOT IN ('confirmed','rejected'))) OR
		 (a.action_id='' AND (n.receipt_id IS NULL OR n.state IN ('received','submitting','unknown','steering','removing'))))`, host, app)
		if err != nil {
			return h, errors.New("native attempt reconciliation health unavailable")
		}
	}
	if schema >= 8 {
		if err := countStates(h.CommandReplies, remoteStates, `SELECT COUNT(*) FROM command_replies WHERE host_id=? AND app_id=? AND state=?`, host, app); err != nil {
			return h, err
		}
		if err := countStates(h.TaskControls, []string{"available", "unavailable", "pending", "unknown"}, `SELECT COUNT(*) FROM task_controls WHERE host_id=? AND app_id=? AND state=?`, host, app); err != nil {
			return h, err
		}
		if err := countStates(h.TaskClaims, []string{"ready", "pending", "unknown", "rejected", "confirmed"}, `SELECT COUNT(*) FROM task_control_claims WHERE host_id=? AND app_id=? AND state=?`, host, app); err != nil {
			return h, err
		}
	}
	if schema >= 14 {
		if err := countStates(h.CommandAttempts, attemptStates, `SELECT COUNT(*) FROM command_reply_attempts WHERE host_id=? AND app_id=? AND state=?`, host, app); err != nil {
			return h, err
		}
		h.CommandReplies["deferred"], err = count(`SELECT COUNT(*) FROM command_reply_throttles t JOIN command_replies r USING(app_id,message_id) WHERE r.host_id=? AND r.app_id=?`, host, app)
		if err != nil {
			return h, errors.New("command retry health unavailable")
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
		for _, state := range []string{"ready", "dispatching", "unknown", "rejected", "confirmed"} {
			h.Requests["reminder_"+state], err = count(`SELECT COUNT(*) FROM app_requests WHERE host_id=? AND app_id=? AND reminder_state=?`, host, app, state)
			if err != nil {
				return h, errors.New("reminder health unavailable")
			}
		}
		if err := countStates(h.ReminderAttempts, attemptStates, `SELECT COUNT(*) FROM request_reminder_attempts WHERE host_id=? AND app_id=? AND state=?`, host, app); err != nil {
			return h, err
		}
	}
	if schema >= 15 {
		h.Requests["reminder_deferred"], err = count(`SELECT COUNT(*) FROM request_reminder_throttles t JOIN app_requests r USING(host_id,app_id,thread_id,request_key) WHERE r.host_id=? AND r.app_id=? AND r.state='pending' AND r.reminder_state='ready'`, host, app)
		if err != nil {
			return h, errors.New("reminder retry health unavailable")
		}
	}
	if schema >= 16 {
		h.Outbound["readback_available"], err = count(`SELECT COUNT(*) FROM outbound_attempts a JOIN outbound_request_markers k USING(attempt_id) WHERE a.host_id=? AND a.state='unknown'`, host)
		if err != nil {
			return h, errors.New("message readback health unavailable")
		}
	}
	if schema >= 17 {
		h.CommandReplies["readback_available"], err = count(`SELECT COUNT(*) FROM command_reply_attempts a JOIN card_request_markers k ON k.kind='command' AND k.attempt_id=a.attempt_id JOIN command_replies r USING(app_id,message_id) WHERE a.host_id=? AND a.app_id=? AND a.state='unknown' AND r.state='unknown' AND r.attempt_id=a.attempt_id`, host, app)
		if err != nil {
			return h, errors.New("command readback health unavailable")
		}
		h.Requests["reminder_readback_available"], err = count(`SELECT COUNT(*) FROM request_reminder_attempts a JOIN card_request_markers k ON k.kind='reminder' AND k.attempt_id=a.attempt_id JOIN app_requests r USING(host_id,app_id,thread_id,request_key) WHERE a.host_id=? AND a.app_id=? AND a.state='unknown' AND r.reminder_state='unknown'`, host, app)
		if err != nil {
			return h, errors.New("reminder readback health unavailable")
		}
	}
	if schema >= 28 {
		if err := countStates(h.CreationDrafts, []string{"draft", "creating", "created", "canceled", "expired"}, `SELECT COUNT(*) FROM creation_drafts WHERE host_id=? AND app_id=? AND state=?`, host, app); err != nil {
			return h, errors.New("creation draft health unavailable")
		}
		if err := countStates(h.CreationIntents, []string{"ready", "working", "unknown", "rejected", "confirmed"}, `SELECT COUNT(*) FROM creation_workflows WHERE host_id=? AND app_id=? AND state=?`, host, app); err != nil {
			return h, errors.New("creation intent health unavailable")
		}
		if err := countStates(h.CreationAttempts, []string{"prepared", "dispatching", "unknown", "rejected", "confirmed"}, `SELECT COUNT(*) FROM creation_attempts a JOIN creation_drafts d ON d.draft_id=a.draft_id WHERE d.host_id=? AND d.app_id=? AND a.state=?`, host, app); err != nil {
			return h, errors.New("creation attempt health unavailable")
		}
		for key, query := range map[string]string{
			"group":           `SELECT COUNT(*) FROM bindings b JOIN deployment d ON d.host_id=b.host_id WHERE b.host_id=? AND d.app_id=? AND b.follow_mode='group'`,
			"topic":           `SELECT COUNT(*) FROM bindings b JOIN deployment d ON d.host_id=b.host_id WHERE b.host_id=? AND d.app_id=? AND b.follow_mode='topic'`,
			"repair_required": `SELECT COUNT(*) FROM bindings b JOIN deployment d ON d.host_id=b.host_id WHERE b.host_id=? AND d.app_id=? AND b.repair_required=1`,
		} {
			h.FollowTargets[key], err = count(query, host, app)
			if err != nil {
				return h, errors.New("follow target health unavailable")
			}
		}
	}
	return h, nil
}
