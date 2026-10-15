package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func seedCreationHealthDeployment(t *testing.T, s *Store, host, app string) {
	t.Helper()
	if _, err := s.BeginBaseline(context.Background(), host, app, 100); err != nil {
		t.Fatal(err)
	}
}

func seedCreationHealthState(t *testing.T, s *Store, host, app, tag, draftState, intentState, attemptState string) {
	t.Helper()
	ctx := context.Background()
	draft := "health-draft-" + tag + "-" + draftState + "-" + intentState + "-" + attemptState
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO creation_drafts(
		draft_id,host_id,app_id,creator_id,source_message_id,source_chat_id,source_chat_type,
		body,directory,project_id,token,expires_at_ms,created_at_ms,state)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		draft, host, app, "health-creator", "health-source-"+tag+"-"+draftState+"-"+intentState+"-"+attemptState, "health-entry-chat", "group",
		"private body "+tag, "/private/root/"+tag, "private-project", "private-token-"+tag+"-"+draftState+"-"+intentState+"-"+attemptState, 10000, 100, draftState); err != nil {
		t.Fatal(err)
	}
	if intentState != "" {
		intent := "health-intent-" + tag + "-" + intentState
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO creation_workflows(
		intent_id,draft_id,host_id,app_id,creator_id,body,directory,project_id,follow_mode,
		target_chat_id,owner_id,members_json,name,state,created_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			intent, draft, host, app, "health-creator", "private body "+tag, "/private/root/"+tag, "private-project", "group",
			"private-target", "private-owner", `["private-member"]`, "private-name", intentState, 100); err != nil {
			t.Fatal(err)
		}
	}
	if attemptState != "" {
		attempt := "health-attempt-" + tag + "-" + attemptState
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO creation_attempts(
		attempt_id,draft_id,step,params_json,state,created_at_ms)
			VALUES(?,?,?,?,?,?)`, attempt, draft, "group-"+attemptState, `{"payload":"private-payload"}`, attemptState, 100); err != nil {
			t.Fatal(err)
		}
	}
}

func seedCreationHealthBinding(t *testing.T, s *Store, host, thread, intent, chat, mode string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO thread_observations(host_id,thread_id,origin,proof,state,cwd,created_at)
		VALUES(?,?, 'new','birth','excluded','/private/observed',100)`, host, thread); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO create_intents(intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,created_at,description)
		VALUES(?,?,?,'private-owner','private-name','["private-member"]','confirmed',?,100,'private-description')`, intent, host, thread, chat); err != nil {
		t.Fatal(err)
	}
	root, platform := "", ""
	if mode == "topic" {
		root, platform = "private-root", "private-platform"
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO bindings(host_id,thread_id,intent_id,chat_id,follow_mode,root_message_id,platform_thread_id)
		VALUES(?,?,?,?,?,?,?)`, host, thread, intent, chat, mode, root, platform); err != nil {
		t.Fatal(err)
	}
}

func TestHealthReportsScopedCreationAndFollowAggregatesWithoutPrivateData(t *testing.T) {
	s := creationTestStore(t)
	seedCreationHealthDeployment(t, s, "health-foreign-host", "health-foreign-app")
	for _, state := range []string{"draft", "creating", "created", "canceled", "expired"} {
		seedCreationHealthState(t, s, creationTestHost, creationTestApp, "target", state, "", "")
	}
	for _, state := range []string{"ready", "working", "unknown", "rejected", "confirmed"} {
		// The state is independently counted in the workflow and attempt maps;
		// each row uses its own draft so all states remain valid and scoped.
		seedCreationHealthState(t, s, creationTestHost, creationTestApp, "intent-"+state, "created", state, "")
	}
	for _, state := range []string{"prepared", "dispatching", "unknown", "rejected", "confirmed"} {
		seedCreationHealthState(t, s, creationTestHost, creationTestApp, "attempt-"+state, "created", "", state)
	}
	seedCreationHealthState(t, s, "health-foreign-host", "health-foreign-app", "foreign", "created", "unknown", "unknown")
	seedCreationHealthBinding(t, s, creationTestHost, "health-group-thread", "health-group-intent", "health-group-chat", "group")
	seedCreationHealthBinding(t, s, creationTestHost, "health-topic-thread", "health-topic-intent", "health-topic-chat", "topic")
	if err := s.MarkBindingRepair(context.Background(), creationTestHost, "health-topic-thread", "private-repair-reason"); err != nil {
		t.Fatal(err)
	}
	seedCreationHealthBinding(t, s, "health-foreign-host", "health-foreign-thread", "health-foreign-intent", "health-foreign-chat", "group")

	h, err := s.Health(context.Background(), SchemaVersion, creationTestHost, creationTestApp)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"draft", "creating", "created", "canceled", "expired"} {
		want := int64(1)
		if state == "created" {
			want = 11 // five intent fixtures and five attempt fixtures also use a created draft.
		}
		if h.CreationDrafts[state] != want {
			t.Fatalf("creation draft state %q crossed scope or was omitted: got=%d want=%d all=%v", state, h.CreationDrafts[state], want, h.CreationDrafts)
		}
	}
	for _, state := range []string{"ready", "working", "unknown", "rejected", "confirmed"} {
		if h.CreationIntents[state] != 1 {
			t.Fatalf("creation intent state %q crossed scope or was omitted: %v", state, h.CreationIntents)
		}
	}
	for _, state := range []string{"prepared", "dispatching", "unknown", "rejected", "confirmed"} {
		if h.CreationAttempts[state] != 1 {
			t.Fatalf("creation attempt state %q crossed scope or was omitted: %v", state, h.CreationAttempts)
		}
	}
	if h.FollowTargets["group"] != 1 || h.FollowTargets["topic"] != 1 || h.FollowTargets["repair_required"] != 1 {
		t.Fatalf("follow target aggregates crossed scope or were omitted: %v", h.FollowTargets)
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private body", "/private/root", "private-project", "private-token", "private-payload", "private-member", "health-draft-", "health-intent-", "health-attempt-", "health-foreign"} {
		if strings.Contains(string(b), private) {
			t.Fatalf("private creation detail leaked into health JSON: %q in %s", private, b)
		}
	}
}

func TestHealthSkipsCreationTablesBelowSchema28(t *testing.T) {
	s := creationTestStore(t)
	seedCreationHealthState(t, s, creationTestHost, creationTestApp, "below-schema", "created", "unknown", "unknown")
	h, err := s.Health(context.Background(), 27, creationTestHost, creationTestApp)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.CreationDrafts) != 0 || len(h.CreationIntents) != 0 || len(h.CreationAttempts) != 0 || len(h.FollowTargets) != 0 {
		t.Fatalf("schema 27 queried creation/follow tables: drafts=%v intents=%v attempts=%v follow=%v", h.CreationDrafts, h.CreationIntents, h.CreationAttempts, h.FollowTargets)
	}
}
