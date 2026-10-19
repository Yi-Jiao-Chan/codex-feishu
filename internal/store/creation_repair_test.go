package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func creationRepairTopicFixture(t *testing.T, s *Store, source, suffix string, now int64) (CreationDraft, CreationIntent, Binding) {
	t.Helper()
	ctx := context.Background()
	draft := creationTestDraft(source, now)
	draft.FollowMode = "topic"
	draft.TargetChatID = "workgroup"
	draft = receiveCreationTestDraft(t, s, draft)
	intent := claimCreationTestIntent(t, s, draft, "owner-"+suffix, "topic-"+suffix, now+1)
	confirmCreationStep(t, s, draft.ID, "group", fmt.Sprintf(`{"chat":"workflow-chat-%s"}`, suffix), "workflow-chat-"+suffix, now+2)
	confirmCreationStep(t, s, draft.ID, "root", fmt.Sprintf(`{"root":"topic-root-%s"}`, suffix), "topic-root-"+suffix, now+3)
	confirmCreationStep(t, s, draft.ID, "native", fmt.Sprintf(`{"thread":"topic-native-%s"}`, suffix), "topic-native-"+suffix, now+4)
	intent, err := s.CreationIntent(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	binding, receipt, err := s.MaterializeCreation(ctx, draft.ID)
	if err != nil {
		t.Fatalf("materialize topic creation: %v", err)
	}
	scope := SubmissionScope{HostID: creationTestHost, AppID: creationTestApp, ThreadID: intent.NativeThreadID}
	if err := s.StageSubmissions(ctx, scope); err != nil {
		t.Fatalf("stage topic creation input: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE submission_inputs SET state='accepted' WHERE receipt_id=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteCreation(ctx, draft.ID); err != nil {
		t.Fatalf("complete topic creation: %v", err)
	}
	draft, err = s.CreationDraft(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	params := fmt.Sprintf(`{"revision":%d}`, draft.Revision)
	attempt, err := s.PrepareCreationAttempt(ctx, draft.ID, "entry:"+fmt.Sprint(draft.Revision), params, now+5)
	if err != nil {
		t.Fatalf("prepare synchronized entry card: %v", err)
	}
	if claimed, err := s.DispatchCreationAttempt(ctx, attempt); err != nil || !claimed {
		t.Fatalf("dispatch synchronized entry card: claimed=%v err=%v", claimed, err)
	}
	if err := s.FinishCreationAttempt(ctx, attempt, "confirmed", "entry-card-"+suffix, `{}`); err != nil {
		t.Fatalf("finish synchronized entry card: %v", err)
	}
	draft, err = s.CreationDraft(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if draft.State != "created" || draft.CardID == "" || draft.CardRevision != draft.Revision {
		t.Fatalf("fixture is not a synchronized created card: %+v", draft)
	}
	return draft, intent, binding
}

func hasCreationDraft(drafts []CreationDraft, id string) bool {
	for _, draft := range drafts {
		if draft.ID == id {
			return true
		}
	}
	return false
}

func TestMarkBindingRepairRequeuesSyncedTopicCreationOnce(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	first, _, firstBinding := creationRepairTopicFixture(t, s, "repair-source-one", "one", 1200)
	second, _, secondBinding := creationRepairTopicFixture(t, s, "repair-source-two", "two", 1400)
	if firstBinding.RepairRequired || secondBinding.RepairRequired {
		t.Fatal("repair fixture unexpectedly starts marked")
	}
	before, err := s.CreationDrafts(ctx, creationTestHost, creationTestApp, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if hasCreationDraft(before, first.ID) || hasCreationDraft(before, second.ID) {
		t.Fatalf("synchronized created cards were polled before repair: %+v", before)
	}
	if err := s.MarkBindingRepair(ctx, firstBinding.HostID, firstBinding.ThreadID, "topic_root_unavailable"); err != nil {
		t.Fatal(err)
	}
	repaired, err := s.CreationDraft(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := s.CreationDraft(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.State != "created" || repaired.ErrorCode != "target-root" || repaired.Revision != first.Revision+1 || repaired.CardRevision != first.CardRevision {
		t.Fatalf("repair did not requeue the created card without changing state/card identity: before=%+v after=%+v", first, repaired)
	}
	if !reflect.DeepEqual(unchanged, second) {
		t.Fatalf("repair changed another creation draft: before=%+v after=%+v", second, unchanged)
	}
	selected, err := s.CreationDrafts(ctx, creationTestHost, creationTestApp, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCreationDraft(selected, first.ID) || hasCreationDraft(selected, second.ID) {
		t.Fatalf("repair did not select only the affected entry card: %+v", selected)
	}
	revision := repaired.Revision
	if err := s.MarkBindingRepair(ctx, firstBinding.HostID, firstBinding.ThreadID, "topic_root_unavailable"); err != nil {
		t.Fatal(err)
	}
	repeated, err := s.CreationDraft(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Revision != revision || repeated.ErrorCode != "target-root" || repeated.State != "created" {
		t.Fatalf("repeating repair bumped or changed the draft: before=%+v after=%+v", repaired, repeated)
	}
	marked, err := s.Binding(ctx, firstBinding.HostID, firstBinding.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if !marked.RepairRequired || marked.RepairReason != "topic_root_unavailable" {
		t.Fatalf("repair binding metadata missing: %+v", marked)
	}
}

func TestMarkBindingRepairCreationTransactionRollsBack(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	draft, _, binding := creationRepairTopicFixture(t, s, "repair-source-rollback", "rollback", 1600)
	if _, err := s.DB.ExecContext(ctx, `CREATE TRIGGER fail_creation_repair
BEFORE UPDATE OF error_code ON creation_drafts
WHEN NEW.error_code='target-root'
BEGIN SELECT RAISE(FAIL,'injected creation repair failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBindingRepair(ctx, binding.HostID, binding.ThreadID, "topic_root_unavailable"); err == nil {
		t.Fatal("injected creation repair failure was not returned")
	}
	afterDraft, err := s.CreationDraft(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterBinding, err := s.Binding(ctx, binding.HostID, binding.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterDraft, draft) {
		t.Fatalf("draft mutation escaped failed repair transaction: before=%+v after=%+v", draft, afterDraft)
	}
	if afterBinding != binding {
		t.Fatalf("binding mutation escaped failed repair transaction: before=%+v after=%+v", binding, afterBinding)
	}
}

func TestMarkBindingRepairSchema27SkipsCreationTables(t *testing.T) {
	s, _ := discoveryStore(t)
	ctx := context.Background()
	binding := topicBinding(t, s, "legacy-topic", "legacy-root")
	if _, err := s.DB.ExecContext(ctx, `DROP TABLE creation_actions; DROP TABLE creation_attempts; DROP TABLE creation_workflows; DROP TABLE creation_drafts; PRAGMA user_version=27`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBindingRepair(ctx, binding.HostID, binding.ThreadID, "legacy_root_unavailable"); err != nil {
		t.Fatal(err)
	}
	marked, err := s.Binding(ctx, binding.HostID, binding.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if !marked.RepairRequired || marked.RepairReason != "legacy_root_unavailable" {
		t.Fatalf("schema 27 binding repair did not persist: %+v", marked)
	}
}
