package store

import (
	"context"
	"path/filepath"
	"testing"
)

func activationTestBinding(t *testing.T, s *Store, host, id, chat string) {
	t.Helper()
	ctx := context.Background()
	if err := s.ObserveNew(ctx, host, id, "/project", 101); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEligible(ctx, host, id, "input-"+id, "/project", "/project"); err != nil {
		t.Fatal(err)
	}
	intent, err := s.PrepareGroup(ctx, host, id, "owner", "task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimGroupDispatch(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmGroup(ctx, intent.ID, chat); err != nil {
		t.Fatal(err)
	}
	if err = s.BindConfirmedGroup(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
}

func TestBindingActivationThreadsSelectsOnlyUnhydratedOrActionableRoutes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "activation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.BeginBaseline(ctx, "host", "app", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteBaseline(ctx, "host", nil); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ id, chat string }{{"dormant", "chat-dormant"}, {"unhydrated", "chat-unhydrated"}, {"outbound", "chat-outbound"}} {
		activationTestBinding(t, s, "host", route.id, route.chat)
	}
	if err := s.StageMirrors(ctx, "host", "dormant", nil, Watermark{ObservedAtMS: 1_000}); err != nil {
		t.Fatal(err)
	}
	doc := Mirror{Key: "human", TurnID: "turn", Kind: "human", Card: `{}`}
	if err := s.StageMirrors(ctx, "host", "outbound", []Mirror{doc}, Watermark{TurnID: "turn", ItemID: "human", ObservedAtMS: 1_000}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.BindingActivationThreads(ctx, "host", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "outbound" || ids[1] != "unhydrated" {
		t.Fatalf("activation routes=%v", ids)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO interaction_requests(
 request_key,host_id,app_id,thread_id,turn_id,item_id,mode,method,native_id,payload_json,connection_id,generation,state,action_token,created_at_ms)
 VALUES('request','host','app','dormant','turn','item','native','question','native','{}','connection',1,'resolved','token',1);
 INSERT INTO interaction_attempts(attempt_id,request_key,user_id,connection_id,generation,result_json,summary,state,created_at_ms)
 VALUES('attempt','request','owner','connection',1,'{}','answered','submitted',1)`); err != nil {
		t.Fatal(err)
	}
	ids, err = s.BindingActivationThreads(ctx, "host", "app")
	if err != nil || len(ids) != 2 || ids[0] != "outbound" || ids[1] != "unhydrated" {
		t.Fatalf("resolved interaction retained an actor: %v %v", ids, err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE interaction_requests SET state='pending' WHERE request_key='request'`); err != nil {
		t.Fatal(err)
	}
	ids, err = s.BindingActivationThreads(ctx, "host", "app")
	if err != nil || len(ids) != 3 || ids[0] != "dormant" {
		t.Fatalf("pending submitted interaction did not activate its route: %v %v", ids, err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE interaction_requests SET state='resolved' WHERE request_key='request'`); err != nil {
		t.Fatal(err)
	}
	binding, err := s.Binding(ctx, "host", "dormant")
	if err != nil {
		t.Fatal(err)
	}
	attempt, claimed, err := s.ClaimGroupName(ctx, GroupNamePlan{Scope: GroupNameScope{AppID: "app", OwnerID: "owner", Binding: binding}, Desired: "B", Previous: "A"}, "", 2_000)
	if err != nil || !claimed {
		t.Fatal("group name fixture was not claimed", claimed, err)
	}
	if err := s.FinishGroupName(ctx, attempt, "unknown"); err != nil {
		t.Fatal(err)
	}
	ids, err = s.BindingActivationThreads(ctx, "host", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "outbound" || ids[1] != "unhydrated" {
		t.Fatalf("group name maintenance retained a native actor: %v", ids)
	}
}
