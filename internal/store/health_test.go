package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthContainsOnlyScopedAggregateFacts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, host := range []string{"host-one", "host-two"} {
		if _, err := s.BeginBaseline(ctx, host, "app", 100); err != nil {
			t.Fatal(err)
		}
		if err := s.ObserveNew(ctx, host, "thread-"+host, "/project", 101); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkEligible(ctx, host, "thread-"+host, "user", "/project", "/project"); err != nil {
			t.Fatal(err)
		}
		i, err := s.PrepareGroup(ctx, host, "thread-"+host, "owner", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimGroupDispatch(ctx, i.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.ConfirmGroup(ctx, i.ID, "chat-"+host); err != nil {
			t.Fatal(err)
		}
		if err := s.BindConfirmedGroup(ctx, i.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Claim(ctx, Receipt{AppID: "app", ThreadID: "thread-" + host, ChatID: "chat-" + host, MessageID: "message-" + host, Body: "private body must not appear"}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := s.Health(ctx, SchemaVersion, "host-one", "app")
	if err != nil || h.Bindings != 1 || h.Inputs["received"] != 1 {
		t.Fatal("cross-host health or incomplete count", err)
	}
	b, _ := json.Marshal(h)
	if strings.Contains(string(b), "private body") || strings.Contains(string(b), "message-") || strings.Contains(string(b), "thread-") {
		t.Fatal("diagnostic contains conversation data")
	}
	if _, err := s.Health(ctx, SchemaVersion, "host-one", "other-app"); err == nil {
		t.Fatal("wrong application used database health")
	}
}

func healthJournals(h PersistentHealth) map[string]map[string]int64 {
	return map[string]map[string]int64{"inputs": h.Inputs, "outbound": h.Outbound, "outbound_attempts": h.OutboundAttempts, "native_actions": h.NativeActions, "native_attempts": h.NativeAttempts,
		"command_replies": h.CommandReplies, "reactions": h.Reactions, "group_intents": h.GroupIntents, "task_controls": h.TaskControls, "task_claims": h.TaskClaims, "requests": h.Requests, "reminder_attempts": h.ReminderAttempts}
}

func TestHealthCountsEveryJournalWithinDeployment(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, scope := range []struct{ host, app string }{{"one", "app"}, {"two", "app"}, {"three", "other-app"}} {
		host, app, thread, chat := scope.host, scope.app, "thread-"+scope.host, "chat-"+scope.host
		if _, err := s.BeginBaseline(ctx, host, app, 100); err != nil {
			t.Fatal(err)
		}
		if err := s.ObserveNew(ctx, host, thread, "/project", 101); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkEligible(ctx, host, thread, "user", "/project", "/project"); err != nil {
			t.Fatal(err)
		}
		intent, err := s.PrepareGroup(ctx, host, thread, "owner", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimGroupDispatch(ctx, intent.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.ConfirmGroup(ctx, intent.ID, chat); err != nil {
			t.Fatal(err)
		}
		if err := s.BindConfirmedGroup(ctx, intent.ID); err != nil {
			t.Fatal(err)
		}
		receipt, _, err := s.Claim(ctx, Receipt{AppID: app, ThreadID: thread, ChatID: chat, MessageID: "message-" + host, Body: "private body"})
		if err != nil {
			t.Fatal(err)
		}
		token := "token-" + host
		for _, insert := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO submission_inputs(receipt_id,host_id,app_id,thread_id,state) VALUES(?,?,?,?,'unknown')`, []any{receipt.ID, host, app, thread}},
			{`INSERT INTO m0_reactions(receipt_id,state) VALUES(?,'unknown')`, []any{receipt.ID}},
			{`INSERT INTO mirrors(host_id,thread_id,mirror_key,turn_id,kind,card_json) VALUES(?,?,'key','turn','progress','private card')`, []any{host, thread}},
			{`INSERT INTO outbound(host_id,thread_id,mirror_key,state) VALUES(?,?,'key','unknown')`, []any{host, thread}},
			{`INSERT INTO outbound_attempts(attempt_id,host_id,thread_id,mirror_key,revision,operation,chat_id,parent_message_id,target_message_id,card_json,created_at_ms,state) VALUES(?,?,?,'key',1,'send',?,'','','private card',1,'unknown')`, []any{"outbound-" + host, host, thread, chat}},
			{`INSERT INTO submission_actions(action_id,host_id,app_id,thread_id,token,revision,kind,stage,state,created_at_ms) VALUES(?,?,?,?,?,1,'stop','interrupt','unknown',1)`, []any{"native-" + host, host, app, thread, token}},
			{`INSERT INTO submission_attempts(attempt_id,host_id,app_id,thread_id,action_id,stage,state,created_at_ms) VALUES(?,?,?,?,?,'interrupt','unknown',1)`, []any{"attempt-" + host, host, app, thread, "native-" + host}},
			{`INSERT INTO command_replies(app_id,message_id,host_id,chat_id,chat_type,user_id,thread_id,kind,attempt_id,card_json,state,created_at_ms) VALUES(?,?,?,?,'group','owner',?,'feedback',?,'private reply','unknown',1)`, []any{app, "command-" + host, host, chat, thread, "reply-" + host}},
			{`INSERT INTO task_controls(host_id,app_id,thread_id,turn_id,token,native_status,kind,state,reason,fingerprint) VALUES(?,?,?,'turn',?,'inProgress','stop','unknown','verifying','private fingerprint')`, []any{host, app, thread, token}},
			{`INSERT INTO task_control_claims(host_id,app_id,thread_id,turn_id,token,revision,kind,chat_id,message_id,external_id,state,created_at_ms) VALUES(?,?,?,'turn',?,1,'stop',?,'private callback',?,'unknown',1)`, []any{host, app, thread, token, chat, "external-" + host}},
			{`INSERT INTO app_requests(host_id,app_id,thread_id,request_key,turn_id,method,event_at,replay,state,confirmed_pending,reminder_state,created_at_ms) VALUES(?,?,?,'request','turn','method',1,0,'pending',0,'unknown',1)`, []any{host, app, thread}},
			{`INSERT INTO request_reminder_attempts(attempt_id,host_id,app_id,thread_id,request_key,chat_id,parent_message_id,card_json,created_at_ms,state) VALUES(?,?,?,?,'request',?,'private message','private card',1,'unknown')`, []any{"reminder-" + host, host, app, thread, chat}},
		} {
			if _, err := s.DB.ExecContext(ctx, insert.query, insert.args...); err != nil {
				t.Fatal(err)
			}
		}
		unbound := "unbound-" + host
		if err := s.ObserveNew(ctx, host, unbound, "/project", 102); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkEligible(ctx, host, unbound, "user", "/project", "/project"); err != nil {
			t.Fatal(err)
		}
		unknown, err := s.PrepareGroup(ctx, host, unbound, "owner", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.ExecContext(ctx, `UPDATE create_intents SET state='unknown' WHERE intent_id=?`, unknown.ID); err != nil {
			t.Fatal(err)
		}
	}
	// The same host's records with a different app must not enter app-scoped
	// journals, even if stale data exists beside the canonical deployment row.
	for _, query := range []string{
		`UPDATE submission_inputs SET app_id='other-app' WHERE host_id='two'`,
		`UPDATE command_replies SET host_id='one',app_id='other-app' WHERE host_id='three'`,
		`INSERT INTO submission_actions(action_id,host_id,app_id,thread_id,token,revision,kind,stage,state,created_at_ms) VALUES('foreign-native','one','other-app','thread-one','foreign-token',1,'stop','interrupt','unknown',1)`,
		`INSERT INTO submission_attempts(attempt_id,host_id,app_id,thread_id,action_id,stage,state,created_at_ms) VALUES('foreign-attempt','one','other-app','thread-one','foreign-native','interrupt','unknown',1)`,
		`INSERT INTO task_controls(host_id,app_id,thread_id,turn_id,token,native_status,kind,state,reason,fingerprint) VALUES('one','other-app','thread-one','turn','foreign-token','inProgress','stop','unknown','verifying','fingerprint')`,
		`INSERT INTO task_control_claims(host_id,app_id,thread_id,turn_id,token,revision,kind,chat_id,message_id,external_id,state,created_at_ms) VALUES('one','other-app','thread-one','turn','foreign-token',1,'stop','chat-one','foreign-message','foreign-external','unknown',1)`,
		`INSERT INTO app_requests(host_id,app_id,thread_id,request_key,turn_id,method,event_at,replay,state,confirmed_pending,reminder_state,created_at_ms) VALUES('one','other-app','thread-one','request','turn','method',1,0,'pending',0,'unknown',1)`,
		`INSERT INTO request_reminder_attempts(attempt_id,host_id,app_id,thread_id,request_key,chat_id,parent_message_id,card_json,created_at_ms,state) VALUES('foreign-reminder','one','other-app','thread-one','request','chat-one','parent','card',1,'unknown')`,
	} {
		if _, err := s.DB.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	foreignReceipt, _, err := s.Claim(ctx, Receipt{AppID: "other-app", ThreadID: "thread-one", ChatID: "chat-one", MessageID: "foreign-reaction", Body: "private foreign body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO m0_reactions(receipt_id,state) VALUES(?,'unknown')`, foreignReceipt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO submission_inputs(receipt_id,host_id,app_id,thread_id,state) VALUES(?,'one','other-app','thread-one','unknown')`, foreignReceipt.ID); err != nil {
		t.Fatal(err)
	}
	h, err := s.Health(ctx, SchemaVersion, "one", "app")
	if err != nil {
		t.Fatal(err)
	}
	for name, counts := range healthJournals(h) {
		key := "unknown"
		if name == "requests" {
			key = "reminder_unknown"
		}
		if counts[key] != 1 {
			t.Fatalf("%s crossed deployment or omitted unknown: %v", name, counts)
		}
	}
	if h.Inputs["received"] != 0 || h.Requests["awaiting_reconciliation"] != 1 {
		t.Fatal("formal input or unverified request not counted", h)
	}
	if h.NativeAttempts["awaiting_reconciliation"] != 1 {
		t.Fatal("unresolved native attempt not counted", h.NativeAttempts)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE submission_actions SET state='running' WHERE host_id='one'; UPDATE app_requests SET reminder_state='dispatching' WHERE host_id='one'`); err != nil {
		t.Fatal(err)
	}
	h, err = s.Health(ctx, SchemaVersion, "one", "app")
	if err != nil || h.NativeActions["running"] != 1 || h.Requests["reminder_dispatching"] != 1 {
		t.Fatal("inflight states omitted", h, err)
	}
	b, _ := json.Marshal(h)
	for _, private := range []string{"private", "thread-one", "message-one", "token-one", "owner", "other-app"} {
		if strings.Contains(string(b), private) {
			t.Fatalf("private journal detail in health: %s", b)
		}
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE submission_actions SET state='unknown' WHERE host_id='one' AND app_id='app'`); err != nil {
		t.Fatal(err)
	}
	scope := SubmissionScope{HostID: "one", AppID: "app", ThreadID: "thread-one"}
	if err := s.ResolveSubmissionAction(ctx, scope, "native-one", "rejected", "ended"); err != nil {
		t.Fatal(err)
	}
	h, err = s.Health(ctx, SchemaVersion, "one", "app")
	if err != nil || h.NativeAttempts["unknown"] != 1 || h.NativeAttempts["awaiting_reconciliation"] != 0 {
		t.Fatal("resolved native ambiguity remained active", h.NativeAttempts, err)
	}
	var input string
	if err := s.DB.QueryRowContext(ctx, `SELECT receipt_id FROM submission_inputs WHERE host_id='one' AND app_id='app'`).Scan(&input); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO submission_attempts(attempt_id,host_id,app_id,thread_id,receipt_id,stage,state,created_at_ms) VALUES('add-audit','one','app','thread-one',?,'add','unknown',1)`, input); err != nil {
		t.Fatal(err)
	}
	h, err = s.Health(ctx, SchemaVersion, "one", "app")
	if err != nil || h.NativeAttempts["unknown"] != 2 || h.NativeAttempts["awaiting_reconciliation"] != 1 {
		t.Fatal("unresolved add not distinguished", h.NativeAttempts, err)
	}
	if err := s.ObserveSubmission(ctx, scope, input, "consumed", "", 0, "observed-turn", "observed-item"); err != nil {
		t.Fatal(err)
	}
	h, err = s.Health(ctx, SchemaVersion, "one", "app")
	if err != nil || h.NativeAttempts["unknown"] != 2 || h.NativeAttempts["awaiting_reconciliation"] != 0 {
		t.Fatal("observed add remained unverified", h.NativeAttempts, err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE app_requests SET state='resolved',reminder_state='unknown' WHERE host_id='one' AND app_id='app'`); err != nil {
		t.Fatal(err)
	}
	h, err = s.Health(ctx, SchemaVersion, "one", "app")
	if err != nil || h.Requests["awaiting_reconciliation"] != 0 || h.Requests["reminder_unknown"] != 1 || h.ReminderAttempts["unknown"] != 1 {
		t.Fatal("resolved native request erased ambiguous Feishu delivery", h.Requests, h.ReminderAttempts, err)
	}
}

func TestHealthReadOnlySupportsHistoricalSchemaWithoutMigration(t *testing.T) {
	for version := 1; version <= SchemaVersion; version++ {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "legacy.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.BeginBaseline(ctx, "host", "app", 100); err != nil {
				t.Fatal(err)
			}
			later := []struct {
				version int
				tables  []string
			}{
				{9, []string{"request_reminder_attempts", "request_mentions", "app_requests"}},
				{8, []string{"task_control_claims", "task_controls", "command_replies"}},
				{7, []string{"mentions", "mention_events", "notification_policy_history"}},
				{6, []string{"directory_rules"}},
				{5, []string{"submission_attempts", "submission_actions", "submission_inputs"}},
				{4, []string{"notification_rules", "input_history_links"}},
				{3, []string{"outbound_attempts", "outbound", "mirrors", "history_watermarks"}},
				{2, []string{"bindings", "create_intents", "thread_observations", "deployment"}},
			}
			for _, schema := range later {
				if schema.version > version {
					for _, table := range schema.tables {
						if _, err := s.DB.ExecContext(ctx, "DROP TABLE "+table); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if _, err := s.DB.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0640); err != nil {
				t.Fatal(err)
			}
			readonly, actual, err := OpenReadOnly(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer readonly.Close()
			h, err := readonly.Health(ctx, actual, "host", "app")
			if err != nil || h.Schema != version {
				t.Fatal("historical health failed", h, err)
			}
			if err := readonly.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&actual); err != nil || actual != version {
				t.Fatal("health migrated database", actual, err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0640 {
				t.Fatal("health altered permissions", err)
			}
		})
	}
}
