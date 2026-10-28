package store

import "context"

// BindingActivationThreads returns routes with durable work that needs a
// native actor. Historical bindings with no pending work are deliberately
// absent: an incoming Feishu/native event can activate them on demand.
func (s *Store) BindingActivationThreads(ctx context.Context, host, app string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `WITH candidates(thread_id) AS (
	SELECT b.thread_id FROM bindings b
	JOIN deployment d USING(host_id)
	LEFT JOIN history_watermarks w ON w.host_id=b.host_id AND w.thread_id=b.thread_id
	WHERE b.host_id=? AND d.app_id=? AND w.thread_id IS NULL
	UNION
	SELECT n.thread_id FROM submission_inputs n
	JOIN bindings b ON b.host_id=n.host_id AND b.thread_id=n.thread_id
	WHERE n.host_id=? AND n.app_id=? AND n.state NOT IN ('removed','consumed','steered','failed')
	UNION
	SELECT a.thread_id FROM submission_actions a
	JOIN bindings b ON b.host_id=a.host_id AND b.thread_id=a.thread_id
	WHERE a.host_id=? AND a.app_id=? AND a.state IN ('ready','running','unknown')
	UNION
	SELECT o.thread_id FROM outbound o
	JOIN bindings b ON b.host_id=o.host_id AND b.thread_id=o.thread_id
	WHERE o.host_id=? AND o.state IN ('ready','dispatching','unknown')
	UNION
	SELECT c.thread_id FROM task_control_claims c
	JOIN bindings b ON b.host_id=c.host_id AND b.thread_id=c.thread_id
	WHERE c.host_id=? AND c.app_id=? AND c.state IN ('ready','pending','unknown')
	UNION
	SELECT c.thread_id FROM task_controls c
	JOIN bindings b ON b.host_id=c.host_id AND b.thread_id=c.thread_id
	WHERE c.host_id=? AND c.app_id=? AND c.state IN ('pending','unknown')
	UNION
	SELECT r.thread_id FROM session_settings_requests r
	JOIN bindings b ON b.host_id=r.host_id AND b.thread_id=r.thread_id
	WHERE r.host_id=? AND r.app_id=? AND r.state IN ('ready','dispatching','unknown')
	AND NOT EXISTS(SELECT 1 FROM session_settings_forms f WHERE f.request_id=r.id AND f.phase IN ('editing','closed'))
	UNION
	SELECT r.thread_id FROM interaction_attempts a
	JOIN interaction_requests r ON r.request_key=a.request_key
	JOIN bindings b ON b.host_id=r.host_id AND b.thread_id=r.thread_id
	WHERE r.host_id=? AND r.app_id=? AND r.state='pending'
	AND a.state IN ('ready','dispatching','submitted','unknown')
	UNION
	SELECT r.thread_id FROM interaction_requests r
	JOIN bindings b ON b.host_id=r.host_id AND b.thread_id=r.thread_id
	WHERE r.host_id=? AND r.app_id=? AND r.state='disconnected'
	UNION
	SELECT r.thread_id FROM app_requests r
	JOIN bindings b ON b.host_id=r.host_id AND b.thread_id=r.thread_id
	WHERE r.host_id=? AND r.app_id=? AND (r.state='pending' OR r.reminder_state IN ('ready','dispatching','unknown'))
	UNION
	SELECT r.thread_id FROM m0_reactions x
	JOIN m0_receipts r ON r.id=x.receipt_id
	JOIN bindings b ON b.host_id=? AND b.thread_id=r.thread_id AND b.chat_id=r.chat_id
	WHERE r.app_id=? AND x.state IN ('changing','unknown')
)
SELECT DISTINCT thread_id FROM candidates ORDER BY thread_id`,
		host, app, host, app, host, app, host, host, app,
		host, app, host, app, host, app, host, app, host, app, host, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
