package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
)

const (
	creationTestHost = "creation-host"
	creationTestApp  = "creation-app"
)

func creationTestStore(t *testing.T) *Store {
	t.Helper()
	s, _ := discoveryStore(t)
	if needed, err := s.BeginBaseline(context.Background(), creationTestHost, creationTestApp, 100); err != nil || !needed {
		t.Fatalf("begin creation baseline: needed=%v err=%v", needed, err)
	}
	return s
}

func creationTestDraft(source string, now int64) CreationDraft {
	return CreationDraft{
		HostID:          creationTestHost,
		AppID:           creationTestApp,
		CreatorID:       "creator",
		SourceMessageID: source,
		SourceChatID:    "entry-chat",
		SourceChatType:  "group",
		SourceRootID:    "entry-root",
		SourceThreadID:  "entry-thread",
		Body:            "create a project",
		Directory:       "/project",
		ProjectID:       "project",
		FollowMode:      "group",
		ExpiresAtMS:     now + 1000,
		CreatedAtMS:     now,
	}
}

func receiveCreationTestDraft(t *testing.T, s *Store, d CreationDraft) CreationDraft {
	t.Helper()
	got, fresh, err := s.ReceiveCreationDraft(context.Background(), d)
	if err != nil || !fresh {
		t.Fatalf("receive creation draft: fresh=%v err=%v", fresh, err)
	}
	if got.ID == "" || got.Token == "" {
		t.Fatalf("received draft has no durable identity: %+v", got)
	}
	return got
}

func primeCreationEntryCard(t *testing.T, s *Store, d CreationDraft, now int64) CreationDraft {
	t.Helper()
	params := `{"revision":1}`
	a, err := s.PrepareCreationAttempt(context.Background(), d.ID, "entry:main", params, now)
	if err != nil {
		t.Fatalf("prepare entry card: %v", err)
	}
	if claimed, err := s.DispatchCreationAttempt(context.Background(), a); err != nil || !claimed {
		t.Fatalf("dispatch entry card: claimed=%v err=%v", claimed, err)
	}
	if err := s.FinishCreationAttempt(context.Background(), a, "confirmed", "entry-card", `{}`); err != nil {
		t.Fatalf("finish entry card: %v", err)
	}
	got, err := s.CreationDraft(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("reload entry-card draft: %v", err)
	}
	if got.CardID != "entry-card" || got.CardRevision != 1 {
		t.Fatalf("entry card was not persisted: %+v", got)
	}
	return got
}

func claimCreationTestIntent(t *testing.T, s *Store, d CreationDraft, owner, name string, now int64) CreationIntent {
	t.Helper()
	i, claimed, err := s.ClaimCreationIntent(context.Background(), d, CreationIntent{
		OwnerID: owner,
		Name:    name,
		Members: []string{owner, "bot"},
	}, now)
	if err != nil || !claimed {
		t.Fatalf("claim creation intent: claimed=%v err=%v", claimed, err)
	}
	return i
}

func confirmCreationStep(t *testing.T, s *Store, draft, step, params, remote string, now int64) CreationAttempt {
	t.Helper()
	a, err := s.PrepareCreationAttempt(context.Background(), draft, step, params, now)
	if err != nil {
		t.Fatalf("prepare %s: %v", step, err)
	}
	if claimed, err := s.DispatchCreationAttempt(context.Background(), a); err != nil || !claimed {
		t.Fatalf("dispatch %s: claimed=%v err=%v", step, claimed, err)
	}
	if err := s.FinishCreationAttempt(context.Background(), a, "confirmed", remote, `{}`); err != nil {
		t.Fatalf("finish %s: %v", step, err)
	}
	return a
}

func creationReadyForMaterialize(t *testing.T, s *Store, source string, now int64) (CreationDraft, CreationIntent) {
	t.Helper()
	d := receiveCreationTestDraft(t, s, creationTestDraft(source, now))
	i := claimCreationTestIntent(t, s, d, "owner", "project", now+1)
	confirmCreationStep(t, s, d.ID, "group", `{"chat":"task-chat"}`, "task-chat", now+2)
	confirmCreationStep(t, s, d.ID, "root", `{"parent":"task-chat"}`, "task-root", now+3)
	confirmCreationStep(t, s, d.ID, "native", `{"thread":"task-root"}`, "native-thread", now+4)
	i, err := s.CreationIntent(context.Background(), d.ID)
	if err != nil {
		t.Fatalf("reload materialize intent: %v", err)
	}
	return d, i
}

func TestCreationSchemaFreshDatabaseMigrates(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	var version int
	if err := s.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("fresh schema version=%d, want %d", version, SchemaVersion)
	}
	var drafts, workflows, attempts int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM creation_drafts").Scan(&drafts); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM creation_workflows").Scan(&workflows); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM creation_attempts").Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if drafts != 0 || workflows != 0 || attempts != 0 {
		t.Fatalf("fresh creation tables not empty: drafts=%d workflows=%d attempts=%d", drafts, workflows, attempts)
	}
	var receipts int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(reply_message_id) FROM m0_receipts").Scan(&receipts); err != nil {
		t.Fatalf("m0 receipt reply anchor column missing: %v", err)
	}
}

func TestReceiveCreationDraftDeduplicatesAfterCancelAndExpiry(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()

	first := receiveCreationTestDraft(t, s, creationTestDraft("source-canceled", 100))
	if first.FollowMode != "group" {
		t.Fatalf("default follow mode=%q", first.FollowMode)
	}
	if err := s.CancelCreationDraft(ctx, first, 200); err != nil {
		t.Fatalf("cancel draft: %v", err)
	}
	replay := creationTestDraft("source-canceled", 100)
	replay.Body = "changed replay body"
	canceled, fresh, err := s.ReceiveCreationDraft(ctx, replay)
	if err != nil || fresh {
		t.Fatalf("canceled source was recreated: fresh=%v err=%v", fresh, err)
	}
	if canceled.ID != first.ID || canceled.State != "canceled" || canceled.Body != first.Body {
		t.Fatalf("canceled replay changed durable draft: first=%+v replay=%+v", first, canceled)
	}

	expiring := creationTestDraft("source-expired", 300)
	expiring.ExpiresAtMS = 350
	expired := receiveCreationTestDraft(t, s, expiring)
	if _, err := s.CreationDrafts(ctx, creationTestHost, creationTestApp, 400); err != nil {
		t.Fatalf("expire drafts: %v", err)
	}
	replay = creationTestDraft("source-expired", 300)
	replay.Body = "another replay body"
	got, fresh, err := s.ReceiveCreationDraft(ctx, replay)
	if err != nil || fresh {
		t.Fatalf("expired source was recreated: fresh=%v err=%v", fresh, err)
	}
	if got.ID != expired.ID || got.State != "expired" || got.Body != expired.Body {
		t.Fatalf("expired replay changed durable draft: first=%+v replay=%+v", expired, got)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM creation_drafts WHERE host_id=? AND app_id=?", creationTestHost, creationTestApp).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("source replay created extra drafts: count=%d", count)
	}
}

func TestClaimCreationIntentOnlyOneConcurrentRevisionWins(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d := receiveCreationTestDraft(t, s, creationTestDraft("source-claim", 500))
	d = primeCreationEntryCard(t, s, d, 501)

	type result struct {
		intent  CreationIntent
		claimed bool
		err     error
	}
	results := make(chan result, 12)
	var wg sync.WaitGroup
	for n := 0; n < cap(results); n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			i, claimed, err := s.ClaimCreationIntent(ctx, d, CreationIntent{
				OwnerID: "owner",
				Name:    "project",
				Members: []string{"owner", "bot"},
			}, 600+int64(n))
			results <- result{intent: i, claimed: claimed, err: err}
		}(n)
	}
	wg.Wait()
	close(results)

	winners := 0
	var winner CreationIntent
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent claim failed: %v", got.err)
		}
		if got.claimed {
			winners++
			winner = got.intent
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent claims created %d intents", winners)
	}
	if winner.ID == "" || winner.DraftID != d.ID || winner.CreatorID != d.CreatorID || winner.Body != d.Body || winner.Directory != d.Directory {
		t.Fatalf("winning intent lost frozen fields: %+v", winner)
	}
	got, err := s.CreationIntent(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != winner.ID || got.State != "ready" {
		t.Fatalf("persisted winner mismatch: winner=%+v saved=%+v", winner, got)
	}
	draft, err := s.CreationDraft(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if draft.State != "creating" || draft.CardID != "entry-card" || draft.Token == "" || draft.CreatorID == "" || draft.SourceRootID != "entry-root" {
		t.Fatalf("claimed draft lost card/token/creator/root: %+v", draft)
	}
}

func TestExpiredCreationDraftCannotClaim(t *testing.T) {
	s := creationTestStore(t)
	d := creationTestDraft("source-expired-claim", 700)
	d.ExpiresAtMS = 750
	d = receiveCreationTestDraft(t, s, d)
	_, _, err := s.ClaimCreationIntent(context.Background(), d, CreationIntent{OwnerID: "owner", Name: "project"}, 750)
	if !errors.Is(err, ErrSubmissionStale) {
		t.Fatalf("expired claim error=%v, want ErrSubmissionStale", err)
	}
	if _, err := s.CreationIntent(context.Background(), d.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired draft created intent: %v", err)
	}
}

func TestUpdateCreationDraftRejectsOldRevision(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d := receiveCreationTestDraft(t, s, creationTestDraft("source-update", 800))
	old := d
	update := d
	update.Body = "new body"
	updated, err := s.UpdateCreationDraft(ctx, update, d.Revision, 850)
	if err != nil {
		t.Fatalf("update current revision: %v", err)
	}
	if updated.Revision != d.Revision+1 || updated.Body != "new body" {
		t.Fatalf("updated draft=%+v", updated)
	}
	old.Body = "stale body"
	if _, err := s.UpdateCreationDraft(ctx, old, d.Revision, 850); !errors.Is(err, ErrSubmissionStale) {
		t.Fatalf("old revision update error=%v, want ErrSubmissionStale", err)
	}
	saved, err := s.CreationDraft(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Body != "new body" || saved.Revision != updated.Revision {
		t.Fatalf("stale update changed persisted draft: %+v", saved)
	}
}

func TestCreationAttemptDispatchClaimedOnlyOnce(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d := receiveCreationTestDraft(t, s, creationTestDraft("source-attempt", 900))
	params := `{"chat":"task-chat"}`
	a, err := s.PrepareCreationAttempt(ctx, d.ID, "group", params, 901)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.PrepareCreationAttempt(ctx, d.ID, "group", params, 902)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != a.ID || replayed.State != "prepared" {
		t.Fatalf("same prepared request changed: first=%+v replay=%+v", a, replayed)
	}
	if claimed, err := s.DispatchCreationAttempt(ctx, a); err != nil || !claimed {
		t.Fatalf("first dispatch claim: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := s.DispatchCreationAttempt(ctx, a); err != nil || claimed {
		t.Fatalf("second dispatch claim: claimed=%v err=%v", claimed, err)
	}
	saved, err := s.CreationAttempt(ctx, d.ID, "group")
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != a.ID || saved.State != "dispatching" || saved.Params != params {
		t.Fatalf("dispatch state not frozen: %+v", saved)
	}
}

func TestUnknownCreationCannotRedispatchAfterRecoveryOrRetry(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d := receiveCreationTestDraft(t, s, creationTestDraft("source-unknown", 1000))
	claimCreationTestIntent(t, s, d, "owner", "project", 1001)
	a, err := s.PrepareCreationAttempt(ctx, d.ID, "group", `{"chat":"task-chat"}`, 1002)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.DispatchCreationAttempt(ctx, a); err != nil || !claimed {
		t.Fatalf("dispatch unknown candidate: claimed=%v err=%v", claimed, err)
	}
	if err := s.RecoverCreations(ctx, creationTestHost, creationTestApp); err != nil {
		t.Fatalf("recover creations: %v", err)
	}
	unknown, err := s.CreationAttempt(ctx, d.ID, "group")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.State != "unknown" {
		t.Fatalf("recovered attempt state=%q", unknown.State)
	}
	intent, err := s.CreationIntent(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != "unknown" {
		t.Fatalf("recovered workflow state=%q", intent.State)
	}
	if claimed, err := s.DispatchCreationAttempt(ctx, unknown); err != nil || claimed {
		t.Fatalf("unknown attempt redispatched after recovery: claimed=%v err=%v", claimed, err)
	}
	if err := s.RetryCreation(ctx, d); !errors.Is(err, ErrSubmissionStale) {
		t.Fatalf("unknown retry error=%v, want ErrSubmissionStale", err)
	}
	if claimed, err := s.DispatchCreationAttempt(ctx, unknown); err != nil || claimed {
		t.Fatalf("unknown attempt redispatched after retry: claimed=%v err=%v", claimed, err)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM creation_attempts WHERE draft_id=? AND step='group'", d.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("unknown retry created attempts: count=%d", count)
	}
}

func TestCreationRecoveryLanesDoNotStealEachOthersDispatch(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d := receiveCreationTestDraft(t, s, creationTestDraft("lane-ownership", 1000))
	d = primeCreationEntryCard(t, s, d, 1001)
	action, fresh, err := s.ClaimCreationAction(ctx, creationTestHost, creationTestApp, d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_panel", `{}`, 1002)
	if err != nil || !fresh {
		t.Fatal("action claim failed", err)
	}
	if _, ok, err := s.NextCreationAction(ctx, creationTestHost, creationTestApp); err != nil || !ok {
		t.Fatal("action ownership failed", err)
	}
	entry, err := s.PrepareCreationAttempt(ctx, d.ID, "entry:busy:"+action.ID, `{"revision":1}`, 1003)
	if err != nil {
		t.Fatal(err)
	}
	group, err := s.PrepareCreationAttempt(ctx, d.ID, "group", `{}`, 1004)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []CreationAttempt{entry, group} {
		if ok, err := s.DispatchCreationAttempt(ctx, attempt); err != nil || !ok {
			t.Fatal("dispatch claim failed", err)
		}
	}
	if err := s.RecoverCreationWorkflows(ctx, creationTestHost, creationTestApp); err != nil {
		t.Fatal(err)
	}
	busy, err := s.CreationAttempt(ctx, d.ID, entry.Step)
	if err != nil || busy.State != "dispatching" {
		t.Fatal("workflow recovery stole active UI dispatch", busy.State, err)
	}
	var state string
	if err := s.DB.QueryRowContext(ctx, `SELECT state FROM creation_actions WHERE action_id=?`, action.ID).Scan(&state); err != nil || state != "working" {
		t.Fatal("workflow recovery stole active UI action", state, err)
	}
	if err := s.RecoverCreationUI(ctx, creationTestHost, creationTestApp); err != nil {
		t.Fatal(err)
	}
	busy, err = s.CreationAttempt(ctx, d.ID, entry.Step)
	if err != nil || busy.State != "unknown" {
		t.Fatal("UI recovery failed to retain uncertain busy write", busy.State, err)
	}
	remote, err := s.CreationAttempt(ctx, d.ID, group.Step)
	if err != nil || remote.State != "unknown" {
		t.Fatal("remote uncertainty was reset by UI recovery", remote.State, err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT state FROM creation_actions WHERE action_id=?`, action.ID).Scan(&state); err != nil || state != "ready" {
		t.Fatal("local UI action was not recoverable", state, err)
	}
}

func TestCreationEntryDispatchWaitsForOlderBusyProof(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d := receiveCreationTestDraft(t, s, creationTestDraft("ordered-entry", 1000))
	busy, err := s.PrepareCreationAttempt(ctx, d.ID, "entry:busy:action", `{"revision":1}`, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DispatchCreationAttempt(ctx, busy); err != nil || !ok {
		t.Fatal("busy not dispatched", err)
	}
	if err := s.FinishCreationAttempt(ctx, busy, "unknown", "", `{}`); err != nil {
		t.Fatal(err)
	}
	result, err := s.PrepareCreationAttempt(ctx, d.ID, "entry:2", `{"revision":2}`, 1002)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DispatchCreationAttempt(ctx, result); err != nil || ok {
		t.Fatal("result could overwrite unproven busy", err)
	}
	if err := s.FinishCreationAttempt(ctx, busy, "confirmed", "entry-card", `{}`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DispatchCreationAttempt(ctx, result); err != nil || !ok {
		t.Fatal("confirmed busy did not release result", err)
	}
}

func TestRejectedCreationRetryPreservesOldAttempt(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d := receiveCreationTestDraft(t, s, creationTestDraft("source-rejected", 1100))
	claimCreationTestIntent(t, s, d, "owner", "project", 1101)
	params := `{"chat":"task-chat","name":"frozen"}`
	old, err := s.PrepareCreationAttempt(ctx, d.ID, "group", params, 1102)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.DispatchCreationAttempt(ctx, old); err != nil || !claimed {
		t.Fatalf("dispatch rejected attempt: claimed=%v err=%v", claimed, err)
	}
	result := `{"reason":"denied"}`
	if err := s.FinishCreationAttempt(ctx, old, "rejected", "", result); err != nil {
		t.Fatalf("finish rejected attempt: %v", err)
	}
	if err := s.RetryCreation(ctx, d); err != nil {
		t.Fatalf("explicit rejected retry: %v", err)
	}
	if err := s.RetryCreation(ctx, d); !errors.Is(err, ErrSubmissionStale) {
		t.Fatalf("implicit second retry error=%v, want ErrSubmissionStale", err)
	}

	rows, err := s.DB.QueryContext(ctx, `SELECT attempt_id,draft_id,step,params_json,state,remote_id,result_json,created_at_ms FROM creation_attempts WHERE draft_id=? AND step='group' ORDER BY rowid`, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var attempts []CreationAttempt
	for rows.Next() {
		attempt, err := scanCreationAttempt(rows)
		if err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("rejected retry attempt count=%d, want 2", len(attempts))
	}
	if attempts[0].ID != old.ID || attempts[0].State != "rejected" || attempts[0].Params != params || attempts[0].Result != result || attempts[0].RemoteID != "" {
		t.Fatalf("old rejected attempt was changed: %+v", attempts[0])
	}
	if attempts[1].ID == old.ID || attempts[1].State != "prepared" || attempts[1].Params != params || attempts[1].RemoteID != "" || attempts[1].Result != "{}" {
		t.Fatalf("retry did not create a fresh frozen attempt: %+v", attempts[1])
	}
	intent, err := s.CreationIntent(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != "ready" || intent.ErrorCode != "" {
		t.Fatalf("workflow after explicit retry=%+v", intent)
	}
	if claimed, err := s.DispatchCreationAttempt(ctx, attempts[1]); err != nil || !claimed {
		t.Fatalf("fresh rejected retry was not dispatchable: claimed=%v err=%v", claimed, err)
	}
}

func TestMaterializeCreationDeduplicatesAcrossChatsAndAnchorsReplyToNewRoot(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d, intent := creationReadyForMaterialize(t, s, "source-cross-chat", 1200)
	if intent.ChatID != "task-chat" || intent.RootMessageID != "task-root" || intent.NativeThreadID != "native-thread" {
		t.Fatalf("creation confirmations incomplete: %+v", intent)
	}
	binding, receipt, err := s.MaterializeCreation(ctx, d.ID)
	if err != nil {
		t.Fatalf("materialize creation: %v", err)
	}
	if binding.ChatID != "task-chat" || binding.ThreadID != "native-thread" || binding.RootMessageID != "task-root" {
		t.Fatalf("materialized binding=%+v", binding)
	}
	if receipt.MessageID != "source-cross-chat" || receipt.ChatID != "task-chat" || receipt.ThreadID != "native-thread" || receipt.ReplyMessageID != "task-root" {
		t.Fatalf("materialized receipt=%+v", receipt)
	}
	if receipt.ClientID == "" || receipt.ActionToken == "" || receipt.ReplyMessageID == d.SourceRootID {
		t.Fatalf("receipt lacks new-root/client identity: %+v source=%q", receipt, d.SourceRootID)
	}

	bindingAgain, receiptAgain, err := s.MaterializeCreation(ctx, d.ID)
	if err != nil {
		t.Fatalf("repeat materialize creation: %v", err)
	}
	if bindingAgain.ChatID != binding.ChatID || bindingAgain.ThreadID != binding.ThreadID || bindingAgain.RootMessageID != binding.RootMessageID || bindingAgain.IntentID != binding.IntentID {
		t.Fatalf("repeat materialization changed binding: first=%+v second=%+v", binding, bindingAgain)
	}
	if receiptAgain.ID != receipt.ID || receiptAgain.ClientID != receipt.ClientID || receiptAgain.ActionToken != receipt.ActionToken || receiptAgain.MessageID != receipt.MessageID || receiptAgain.ReplyMessageID != receipt.ReplyMessageID {
		t.Fatalf("repeat materialization changed receipt: first=%+v second=%+v", receipt, receiptAgain)
	}
	var receipts, bindings, intents, observations int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM m0_receipts WHERE app_id=? AND message_id=?", creationTestApp, d.SourceMessageID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM bindings WHERE host_id=? AND thread_id=?", creationTestHost, "native-thread").Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM create_intents WHERE host_id=? AND thread_id=?", creationTestHost, "native-thread").Scan(&intents); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM thread_observations WHERE host_id=? AND thread_id=?", creationTestHost, "native-thread").Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || bindings != 1 || intents != 1 || observations != 1 {
		t.Fatalf("repeat materialization duplicated rows: receipts=%d bindings=%d intents=%d observations=%d", receipts, bindings, intents, observations)
	}
	savedIntent, err := s.CreationIntent(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if savedIntent.ReceiptID != receipt.ID {
		t.Fatalf("workflow receipt binding=%q, receipt=%q", savedIntent.ReceiptID, receipt.ID)
	}
}

func TestCompleteCreationRequiresFirstSubmissionOutcome(t *testing.T) {
	for _, state := range []string{"accepted", "queued", "consumed"} {
		t.Run(state, func(t *testing.T) {
			s := creationTestStore(t)
			ctx := context.Background()
			d, _ := creationReadyForMaterialize(t, s, "source-complete-"+state, 1300)
			if _, receipt, err := s.MaterializeCreation(ctx, d.ID); err != nil {
				t.Fatal(err)
			} else if err := s.CompleteCreation(ctx, d.ID); !errors.Is(err, ErrConflict) {
				t.Fatalf("complete before staging error=%v, want ErrConflict", err)
			} else {
				scope := SubmissionScope{HostID: creationTestHost, AppID: creationTestApp, ThreadID: "native-thread"}
				if err := s.StageSubmissions(ctx, scope); err != nil {
					t.Fatalf("stage first submission: %v", err)
				}
				if err := s.CompleteCreation(ctx, d.ID); !errors.Is(err, ErrConflict) {
					t.Fatalf("complete before outcome error=%v, want ErrConflict", err)
				}
				if _, err := s.DB.ExecContext(ctx, "UPDATE submission_inputs SET state=? WHERE receipt_id=?", state, receipt.ID); err != nil {
					t.Fatal(err)
				}
				if err := s.CompleteCreation(ctx, d.ID); err != nil {
					t.Fatalf("complete after %s: %v", state, err)
				}
				done, err := s.CreationDraft(ctx, d.ID)
				if err != nil {
					t.Fatal(err)
				}
				if done.State != "created" {
					t.Fatalf("draft after %s=%+v", state, done)
				}
				intent, err := s.CreationIntent(ctx, d.ID)
				if err != nil {
					t.Fatal(err)
				}
				if intent.State != "confirmed" {
					t.Fatalf("workflow after %s=%+v", state, intent)
				}
			}
		})
	}
}
