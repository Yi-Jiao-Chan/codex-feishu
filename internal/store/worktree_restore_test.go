package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func worktreeRestoreStore(t *testing.T) (*Store, string, SubmissionScope) {
	t.Helper()
	s, path, scope := taskControlStore(t, true)
	// The primary owns the versioned migration; the module schema is independently
	// executable while that integration is in progress.
	if _, err := s.DB.Exec(WorktreeRestoreSchema); err != nil {
		t.Fatal(err)
	}
	return s, path, scope
}

func restoreOrdinaryReceipt(t *testing.T, s *Store, scope SubmissionScope, message string) Receipt {
	t.Helper()
	r, fresh, err := s.ReceiveSubmissionInput(context.Background(), scope, Receipt{AppID: scope.AppID, ThreadID: scope.ThreadID, ChatID: "chat", MessageID: message, Body: "synthetic ordinary input"})
	if err != nil || !fresh {
		t.Fatal("ordinary input failed", err)
	}
	return r
}

func restoreTestIdentity(scope SubmissionScope) ThreadWorktreeIdentity {
	return ThreadWorktreeIdentity{HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID, RawCWD: "/private/worktrees/abcd/repository/sub", Root: "/private/worktrees/abcd/repository", SourceRoot: "/source/repository", CommonDir: "/source/repository/.git", RelativeCWD: "sub"}
}

func restoreTestResult() string {
	return `{"version":1,"phase":"restored","generation":"v1:` + strings.Repeat("a", 64) + `","publicationPending":true}`
}

func cancelWorktreeRestoreInput(t *testing.T, s *Store, scope SubmissionScope, r Receipt) {
	t.Helper()
	ctx := context.Background()
	if err := s.StageQueueCardWithOptions(ctx, scope, r.ID, QueueCardOptions{LocalPending: true}); err != nil {
		t.Fatal(err)
	}
	m, err := s.Mirror(ctx, scope.HostID, scope.ThreadID, QueueCardKey(r.ID))
	if err != nil {
		t.Fatal(err)
	}
	a, claimed, err := s.ClaimMirror(ctx, scope.HostID, scope.ThreadID, m.Key, m.Revision, 1000)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err := s.FinishMirrorAttempt(ctx, a.ID, "confirmed", "synthetic-cancel-card-"+r.ID); err != nil {
		t.Fatal(err)
	}
	input, err := s.Submission(ctx, scope, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelPendingSubmission(ctx, scope, "operator", []string{"operator"}, "chat", "synthetic-cancel-card-"+r.ID, r.ActionToken, input.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeRestoreIdentityIsImmutableAndBoundToExactScope(t *testing.T) {
	s, _, scope := worktreeRestoreStore(t)
	ctx := context.Background()
	v := restoreTestIdentity(scope)
	for n := 0; n < 2; n++ {
		if err := s.RegisterThreadWorktreeIdentity(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ThreadWorktreeIdentity(ctx, scope)
	if err != nil || got != v {
		t.Fatal("identity changed", got, err)
	}
	for _, field := range []string{"cwd", "root", "source", "common", "relative", "host", "app", "thread"} {
		changed := v
		switch field {
		case "cwd":
			changed.RawCWD = "/other/sub"
		case "root":
			changed.Root, changed.RawCWD = "/other", "/other/sub"
		case "source":
			changed.SourceRoot = "/other/source"
		case "common":
			changed.CommonDir = "/other/common"
		case "relative":
			changed.RelativeCWD = "../sub"
		case "host":
			changed.HostID = "other-host"
		case "app":
			changed.AppID = "other-app"
		case "thread":
			changed.ThreadID = "other-thread"
		}
		if err := s.RegisterThreadWorktreeIdentity(ctx, changed); err == nil {
			t.Fatal("accepted unverified identity drift", field)
		}
	}
	if _, err := s.DB.Exec(`UPDATE bindings SET root_message_id='another-root' WHERE host_id=? AND thread_id=?`, scope.HostID, scope.ThreadID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ThreadWorktreeIdentity(ctx, scope); err == nil {
		t.Fatal("identity crossed a changed binding")
	}
}

func TestWorktreeRestoreAdmissionGrantIsAtomicAndReplayDoesNotGrantAgain(t *testing.T) {
	s, _, scope := worktreeRestoreStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(`CREATE TRIGGER reject_restore_grant BEFORE INSERT ON worktree_restore_grants BEGIN SELECT RAISE(ABORT,'synthetic grant failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReceiveSubmissionInput(ctx, scope, Receipt{AppID: scope.AppID, ThreadID: scope.ThreadID, ChatID: "chat", MessageID: "atomic-message", Body: "synthetic input"}); err == nil {
		t.Fatal("admission ignored grant persistence failure")
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM m0_receipts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("receipt escaped failed grant transaction", err)
	}
	if _, err := s.DB.Exec(`DROP TRIGGER reject_restore_grant`); err != nil {
		t.Fatal(err)
	}
	r := restoreOrdinaryReceipt(t, s, scope, "atomic-message")
	if _, fresh, err := s.ReceiveSubmissionInput(ctx, scope, r); err != nil || fresh {
		t.Fatal("replay admitted input", err)
	}
	g, err := s.NextWorktreeRestoreGrant(ctx, scope)
	if err != nil || g.ReceiptID != r.ID || g.Origin != "ordinary" {
		t.Fatal("ordinary grant unavailable", g, err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM worktree_restore_grants`).Scan(&count); err != nil || count != 1 {
		t.Fatal("replay duplicated grant", count, err)
	}
	for _, bad := range []SubmissionScope{{"other", scope.AppID, scope.ThreadID}, {scope.HostID, "other", scope.ThreadID}, {scope.HostID, scope.AppID, "other"}} {
		if _, err := s.WorktreeRestoreGrant(ctx, bad, r.ID); err == nil {
			t.Fatal("grant crossed scope", bad)
		}
	}
	if _, err := s.DB.Exec(`UPDATE bindings SET repair_required=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NextWorktreeRestoreGrant(ctx, scope); !errors.Is(err, ErrTargetRepair) {
		t.Fatal("repair-required binding authorized restore", err)
	}
}

func TestWorktreeRestoreCollectingAttachmentBlocksLaterInputUntilReady(t *testing.T) {
	s, _, scope := worktreeRestoreStore(t)
	ctx := context.Background()
	b, fresh, err := s.ReceiveAttachmentInput(ctx, AttachmentInput{Scope: scope, ChatID: "chat", SenderID: "operator", MessageID: "attachment-caption", Body: "synthetic caption", Parts: []AttachmentPart{{Kind: "file", ResourceKey: "synthetic-key", Name: "fixture.txt"}}}, 10, 5)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	later := restoreOrdinaryReceipt(t, s, scope, "later-input")
	if _, err := s.NextWorktreeRestoreGrant(ctx, scope); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("later input overtook collecting attachment", err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishAttachmentReceiptTx(ctx, tx, scope, b.ReceiptID, "received", `[{"type":"text","text":"synthetic complete input"}]`); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	g, err := s.NextWorktreeRestoreGrant(ctx, scope)
	if err != nil || g.ReceiptID != b.ReceiptID || g.ReceiptID == later.ID || g.Origin != "ordinary" {
		t.Fatal("ready attachment lost its original grant", g, err)
	}
	var grants int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM worktree_restore_grants`).Scan(&grants); err != nil || grants != 2 {
		t.Fatal("ready transition created another grant", grants, err)
	}
}

func TestWorktreeRestoreBackfillOnlyDefinitelyUnsentOrdinaryInputs(t *testing.T) {
	for _, kind := range []string{"ordinary", "unstaged", "queued", "unknown", "native-attempt", "m0-attempt", "m0-action", "history", "creation", "async", "foreign-client", "reply-root", "frozen-without-attachment", "collecting", "removed"} {
		t.Run(kind, func(t *testing.T) {
			s, _, scope, r := submissionStore(t)
			ctx := context.Background()
			if _, err := s.DB.Exec(WorktreeRestoreSchema); err != nil {
				t.Fatal(err)
			}
			var query string
			var args []any
			switch kind {
			case "unstaged":
				query, args = `DELETE FROM submission_inputs WHERE receipt_id=?`, []any{r.ID}
			case "queued":
				query, args = `UPDATE m0_receipts SET queue_id='native-queue' WHERE id=?`, []any{r.ID}
			case "unknown", "collecting", "removed":
				query, args = `UPDATE submission_inputs SET state=? WHERE receipt_id=?`, []any{kind, r.ID}
			case "native-attempt":
				if _, claimed, err := s.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add"); err != nil || !claimed {
					t.Fatal("native fixture attempt", err)
				}
				// A misleading legacy label must not erase durable dispatch proof.
				if _, err := s.DB.Exec(`UPDATE m0_receipts SET state='received' WHERE id=?`, r.ID); err != nil {
					t.Fatal(err)
				}
				query, args = `UPDATE submission_inputs SET state='received' WHERE receipt_id=?`, []any{r.ID}
			case "m0-attempt":
				_, _ = s.Begin(ctx, "m0.add."+r.ID, "queue.add")
			case "m0-action":
				query, args = `INSERT INTO m0_actions(receipt_id,kind,state) VALUES(?,'steer','processing')`, []any{r.ID}
			case "history":
				query, args = `INSERT INTO input_history_links(host_id,thread_id,turn_id,item_id,receipt_id) VALUES(?,?,'turn','item',?)`, []any{scope.HostID, scope.ThreadID, r.ID}
			case "creation":
				query, args = `UPDATE m0_receipts SET reply_message_id='creation-root' WHERE id=?`, []any{r.ID}
			case "async":
				query, args = `UPDATE m0_receipts SET client_id='cfb-another-generated-id' WHERE id=?`, []any{r.ID}
			case "foreign-client":
				query, args = `UPDATE m0_receipts SET client_id='external' WHERE id=?`, []any{r.ID}
			case "reply-root":
				query, args = `UPDATE m0_receipts SET reply_message_id='other-root' WHERE id=?`, []any{r.ID}
			case "frozen-without-attachment":
				query, args = `UPDATE m0_receipts SET input_json='[]' WHERE id=?`, []any{r.ID}
			}
			if query != "" {
				if _, err := s.DB.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			n, err := s.BackfillWorktreeRestoreGrants(ctx, scope, 100)
			want := 0
			if kind == "ordinary" || kind == "unstaged" {
				want = 1
			}
			if err != nil || n != want {
				t.Fatal("unsafe backfill", n, want, err)
			}
			if n, err := s.BackfillWorktreeRestoreGrants(ctx, scope, 101); err != nil || n != 0 {
				t.Fatal("backfill duplicated grant", n, err)
			}
		})
	}
}

func TestWorktreeRestoreJournalCheckpointsRestartAndNoUnknownBypass(t *testing.T) {
	s, path, scope := worktreeRestoreStore(t)
	ctx := context.Background()
	r := restoreOrdinaryReceipt(t, s, scope, "restore-input")
	params := `{"version":1,"root":"/private/worktree","snapshot":"refs/codex/snapshots/synthetic"}`
	a, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, r.ID, params, 100)
	if err != nil || a.State != "prepared" || a.Phase != "planned" {
		t.Fatal(err)
	}
	if _, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, r.ID, `{"version":2}`, 101); !errors.Is(err, ErrConflict) {
		t.Fatal("params were overwritten", err)
	}
	if ok, err := s.ClaimWorktreeRestoreAttempt(ctx, a, 102); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := s.ClaimWorktreeRestoreAttempt(ctx, a, 103); err != nil || ok {
		t.Fatal("dispatch CAS replayed", err)
	}
	checkpoint := `{"version":1,"phase":"stage-dispatching"}`
	if err := s.CheckpointWorktreeRestoreAttempt(ctx, a, "stage-dispatching", checkpoint, 104); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckpointWorktreeRestoreAttempt(ctx, a, "stage-ready", `{"version":1,"phase":"stage-ready"}`, 105); !errors.Is(err, ErrConflict) {
		t.Fatal("stale checkpoint overwrote later phase", err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverWorktreeRestoreDispatches(ctx, scope, 106); err != nil {
		t.Fatal(err)
	}
	a, err = s.WorktreeRestoreAttempt(ctx, scope, r.ID)
	if err != nil || a.State != "unknown" || a.Phase != "stage-dispatching" || a.ResultJSON != checkpoint || a.ParamsJSON != params {
		t.Fatal("restart lost immutable checkpoint", a, err)
	}
	later := restoreOrdinaryReceipt(t, s, scope, "later-restore-input")
	if _, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, later.ID, params, 107); !errors.Is(err, ErrWorktreeRestoreBusy) {
		t.Fatal("new receipt bypassed unknown restore", err)
	}
	if g, err := s.NextWorktreeRestoreGrant(ctx, scope); err != nil || g.ReceiptID != r.ID {
		t.Fatal("pending restore lost priority", g, err)
	}
	if err := s.ResumeWorktreeRestoreAttempt(ctx, a, "stage-ready", `{"version":1,"phase":"stage-ready","stageIdentity":"synthetic"}`, 108); err != nil {
		t.Fatal(err)
	}
	a, _ = s.WorktreeRestoreAttempt(ctx, scope, r.ID)
	if err := s.FinishWorktreeRestoreAttempt(ctx, a, "confirmed", `{}`, "", 109); err == nil {
		t.Fatal("confirmed without physical readback")
	}
	if err := s.FinishWorktreeRestoreAttempt(ctx, a, "confirmed", restoreTestResult(), "", 109); err != nil {
		t.Fatal(err)
	}
	confirmed, err := s.LatestConfirmedWorktreeRestoreAttempt(ctx, scope)
	if err != nil || confirmed.ID != a.ID || confirmed.Phase != "restored" {
		t.Fatal("restored generation unavailable", err)
	}
	if all, err := s.ConfirmedWorktreeRestoreAttempts(ctx, scope); err != nil || len(all) != 1 || all[0].ID != a.ID {
		t.Fatal("confirmed generation inventory unavailable", err)
	}
	if g, err := s.NextWorktreeRestoreGrant(ctx, scope); err != nil || g.ReceiptID != later.ID {
		t.Fatal("terminal receipt blocked later new permission", g, err)
	}
}

func TestWorktreeRestoreBlockedReusesReceiptAndNeverChangesInput(t *testing.T) {
	s, _, scope := worktreeRestoreStore(t)
	ctx := context.Background()
	r := restoreOrdinaryReceipt(t, s, scope, "blocked-input")
	a, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, r.ID, `{"version":1}`, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishWorktreeRestoreAttempt(ctx, a, "blocked", `{}`, "snapshot-unavailable", 101); err != nil {
		t.Fatal(err)
	}
	prior, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, r.ID, a.ParamsJSON, 102)
	if err != nil || prior.State != "blocked" || prior.ID != a.ID || prior.ErrorCode != "snapshot-unavailable" {
		t.Fatal("blocked receipt acquired a fresh attempt", prior, err)
	}
	if ok, err := s.ClaimWorktreeRestoreAttempt(ctx, prior, 103); err != nil || ok {
		t.Fatal("blocked attempt dispatched", err)
	}
	n, err := s.Submission(ctx, scope, r.ID)
	if err != nil || n.State != "received" || n.ClientID != r.ClientID || n.QueueID != "" {
		t.Fatal("restore failure changed original input", n.State, err)
	}
	later := restoreOrdinaryReceipt(t, s, scope, "fresh-after-blocked")
	if g, err := s.NextWorktreeRestoreGrant(ctx, scope); err != nil || g.ReceiptID != later.ID {
		t.Fatal("old blocked grant starved new message", g, err)
	}
}

func TestWorktreeRestoreBackfillRejectsPersistedCreationAndAsyncProvenance(t *testing.T) {
	t.Run("creation", func(t *testing.T) {
		s := creationTestStore(t)
		ctx := context.Background()
		d, i := creationReadyForMaterialize(t, s, "synthetic-creation", 1000)
		_, r, err := s.MaterializeCreation(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		scope := SubmissionScope{i.HostID, i.AppID, i.NativeThreadID}
		if err := s.StageSubmissions(ctx, scope); err != nil {
			t.Fatal(err)
		}
		// Even if incidental ID/root conventions become ordinary-looking, the
		// explicit creation reference must independently prohibit backfill.
		if _, err := s.DB.Exec(`UPDATE m0_receipts SET client_id='cfb-'||id,reply_message_id='' WHERE id=?`, r.ID); err != nil {
			t.Fatal(err)
		}
		if n, err := s.BackfillWorktreeRestoreGrants(ctx, scope, 1100); err != nil || n != 0 {
			t.Fatal("creation initial input acquired ordinary restore permission", n, err)
		}
	})
	t.Run("async", func(t *testing.T) {
		s, _, scope, r := submissionStore(t)
		ctx := context.Background()
		request := InteractionRequest{Key: "synthetic-question", HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID, TurnID: "synthetic-turn", ItemID: "synthetic-item", Mode: "async", Method: "async", NativeID: "synthetic-question", Payload: `{}`, ConnectionID: "synthetic-connection", Generation: 1, State: "pending", CreatedAtMS: 1000}
		if err := s.ObserveInteraction(ctx, request); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.Exec(`UPDATE interaction_requests SET submission_id=? WHERE request_key=?`, r.ID, request.Key); err != nil {
			t.Fatal(err)
		}
		if n, err := s.BackfillWorktreeRestoreGrants(ctx, scope, 1100); err != nil || n != 0 {
			t.Fatal("async input acquired ordinary restore permission", n, err)
		}
	})
}

func TestWorktreeRestoreAttachmentGrantFailureRollsBackAdmission(t *testing.T) {
	for _, resources := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "resource"}[resources], func(t *testing.T) {
			s, _, scope := worktreeRestoreStore(t)
			if _, err := s.DB.Exec(`CREATE TRIGGER fail_attachment_restore_grant BEFORE INSERT ON worktree_restore_grants BEGIN SELECT RAISE(ABORT,'synthetic grant failure'); END`); err != nil {
				t.Fatal(err)
			}
			in := AttachmentInput{Scope: scope, ChatID: "chat", SenderID: "operator", MessageID: "atomic-attachment-message", Body: "synthetic ordinary content", Parts: []AttachmentPart{{Kind: "text", Text: "synthetic ordinary content"}}}
			if resources {
				in.Parts = append(in.Parts, AttachmentPart{Kind: "file", ResourceKey: "synthetic-resource", Name: "fixture.txt"})
			}
			if _, _, err := s.ReceiveAttachmentInput(context.Background(), in, 10, 5); err == nil {
				t.Fatal("attachment ignored grant persistence failure")
			}
			for _, table := range []string{"m0_receipts", "submission_inputs", "attachment_batches", "attachment_events", "worktree_restore_grants"} {
				var count int
				if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("attachment admission escaped grant rollback", table, count, err)
				}
			}
		})
	}
}

func TestWorktreeRestoreJournalFailureAndScopeCAS(t *testing.T) {
	s, _, scope := worktreeRestoreStore(t)
	ctx := context.Background()
	r := restoreOrdinaryReceipt(t, s, scope, "journal-input")
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_restore_prepare BEFORE INSERT ON worktree_restore_attempts BEGIN SELECT RAISE(ABORT,'synthetic journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, r.ID, `{"version":1}`, 100); err == nil {
		t.Fatal("prepare ignored journal failure")
	}
	if _, err := s.DB.Exec(`DROP TRIGGER fail_restore_prepare`); err != nil {
		t.Fatal(err)
	}
	a, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, r.ID, `{"version":1}`, 101)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []SubmissionScope{{"other", scope.AppID, scope.ThreadID}, {scope.HostID, "other", scope.ThreadID}, {scope.HostID, scope.AppID, "other"}} {
		copy := a
		copy.Scope = bad
		if _, err := s.ClaimWorktreeRestoreAttempt(ctx, copy, 102); err == nil {
			t.Fatal("claim crossed scope", bad)
		}
		if _, err := s.WorktreeRestoreAttempts(ctx, bad); err == nil {
			t.Fatal("attempt lookup crossed scope", bad)
		}
	}
	if ok, err := s.ClaimWorktreeRestoreAttempt(ctx, a, 103); err != nil || !ok {
		t.Fatal(err)
	}
	checkpoint := `{"version":1,"phase":"stage-ready","stageIdentity":"synthetic-stage"}`
	if err := s.CheckpointWorktreeRestoreAttempt(ctx, a, "stage-ready", checkpoint, 104); err != nil {
		t.Fatal(err)
	}
	a, _ = s.WorktreeRestoreAttempt(ctx, scope, r.ID)
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_restore_confirm BEFORE UPDATE OF state ON worktree_restore_attempts WHEN NEW.state='confirmed' BEGIN SELECT RAISE(ABORT,'synthetic confirmation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishWorktreeRestoreAttempt(ctx, a, "confirmed", restoreTestResult(), "", 105); err == nil {
		t.Fatal("confirmation ignored journal failure")
	}
	if _, err := s.DB.Exec(`DROP TRIGGER fail_restore_confirm`); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishWorktreeRestoreAttempt(ctx, a, "unknown", `{}`, "response-unknown", 106); err != nil {
		t.Fatal(err)
	}
	a, _ = s.WorktreeRestoreAttempt(ctx, scope, r.ID)
	if a.State != "unknown" || a.ResultJSON != checkpoint {
		t.Fatal("unknown erased last physical checkpoint", a.State)
	}
	if _, claimed, err := s.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add"); err != nil || !claimed {
		t.Fatal("synthetic native write setup", err)
	}
	if err := s.ResumeWorktreeRestoreAttempt(ctx, a, "stage-ready", checkpoint, 107); !errors.Is(err, ErrSubmissionStale) {
		t.Fatal("native-attempted input resumed filesystem mutation", err)
	}
	if _, err := s.NextWorktreeRestoreGrant(ctx, scope); !errors.Is(err, ErrSubmissionBusy) {
		t.Fatal("native pending state was not explicit", err)
	}
}

func TestWorktreeRestoreFrozenBindingCannotAuthorizeAChangedTarget(t *testing.T) {
	s, _, scope := worktreeRestoreStore(t)
	ctx := context.Background()
	r := restoreOrdinaryReceipt(t, s, scope, "original-target-input")
	a, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, r.ID, `{"version":1}`, 100)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimWorktreeRestoreAttempt(ctx, a, 101); err != nil || !ok {
		t.Fatal(err)
	}
	if err := s.FinishWorktreeRestoreAttempt(ctx, a, "confirmed", restoreTestResult(), "", 102); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE bindings SET root_message_id='changed-target-root'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorktreeRestoreAttempt(ctx, scope, r.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("attempt crossed frozen target", err)
	}
	if _, err := s.LatestConfirmedWorktreeRestoreAttempt(ctx, scope); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("old physical generation authorized changed target", err)
	}
	if all, err := s.WorktreeRestoreAttempts(ctx, scope); err != nil || len(all) != 0 {
		t.Fatal("attempt inventory crossed frozen target", err)
	}
}

func TestWorktreeRestorePublicationOnlyClearsPendingOnSameProof(t *testing.T) {
	s, _, scope := worktreeRestoreStore(t)
	ctx := context.Background()
	r := restoreOrdinaryReceipt(t, s, scope, "publication-input")
	a, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, r.ID, `{"version":1}`, 100)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimWorktreeRestoreAttempt(ctx, a, 101); err != nil || !ok {
		t.Fatal(err)
	}
	result := strings.TrimSuffix(restoreTestResult(), "}") + `,"physical":{"inode":9007199254740993},"futureProof":{"marker":"synthetic"}}`
	if err := s.FinishWorktreeRestoreAttempt(ctx, a, "confirmed", result, "", 102); err != nil {
		t.Fatal(err)
	}
	a, _ = s.WorktreeRestoreAttempt(ctx, scope, r.ID)
	published := strings.Replace(result, `"publicationPending":true`, `"publicationPending":false`, 1)
	for _, bad := range []string{
		result,
		strings.Replace(published, strings.Repeat("a", 64), strings.Repeat("b", 64), 1),
		strings.Replace(published, `"phase":"restored"`, `"phase":"stage-ready"`, 1),
		strings.Replace(published, `9007199254740993`, `9007199254740992`, 1),
		strings.Replace(published, `,"futureProof":{"marker":"synthetic"}`, "", 1),
		strings.TrimSuffix(published, "}") + `,"extraProof":true}`,
		strings.TrimSuffix(published, "}") + `,"publicationPending":false}`,
	} {
		if err := s.MarkWorktreeRestorePublished(ctx, a, bad, 103); !errors.Is(err, ErrConflict) {
			t.Fatal("publication changed proof/generation or accepted duplicate fields", err)
		}
	}
	for _, state := range []string{"prepared", "dispatching", "unknown", "blocked"} {
		copy := a
		copy.State = state
		if err := s.MarkWorktreeRestorePublished(ctx, copy, published, 103); !errors.Is(err, ErrConflict) {
			t.Fatal("publication accepted an unconfirmed generation", state, err)
		}
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_publication_result BEFORE UPDATE OF result_json ON worktree_restore_attempts BEGIN SELECT RAISE(ABORT,'synthetic publication journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorktreeRestorePublished(ctx, a, published, 103); err == nil {
		t.Fatal("publication ignored local journal failure")
	}
	if current, err := s.WorktreeRestoreAttempt(ctx, scope, r.ID); err != nil || current.ResultJSON != result {
		t.Fatal("lost publication result cleared pending without durable evidence", err)
	}
	if _, err := s.DB.Exec(`DROP TRIGGER fail_publication_result`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorktreeRestorePublished(ctx, a, published, 104); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorktreeRestorePublished(ctx, a, published, 105); err != nil {
		t.Fatal("original-checkpoint replay was not idempotent", err)
	}
	current, err := s.WorktreeRestoreAttempt(ctx, scope, r.ID)
	if err != nil || current.State != "confirmed" || current.Phase != "restored" || current.ResultJSON != published || current.ParamsJSON != a.ParamsJSON || current.UpdatedAtMS != 104 {
		t.Fatal("publication changed immutable journal fields", err)
	}
	if err := s.MarkWorktreeRestorePublished(ctx, current, published, 106); err != nil {
		t.Fatal("published-checkpoint replay failed", err)
	}
	foreign := a
	foreign.Scope.AppID = "another-app"
	if err := s.MarkWorktreeRestorePublished(ctx, foreign, published, 107); err == nil {
		t.Fatal("publication crossed application scope")
	}
	if _, err := s.DB.Exec(`UPDATE bindings SET root_message_id='different-target'`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorktreeRestorePublished(ctx, a, published, 108); err == nil {
		t.Fatal("publication crossed frozen binding")
	}
}

func TestWorktreeRestoreAbandonCanceledOnlyVerifiedPreMovePhases(t *testing.T) {
	for _, phase := range []string{"planned", "stage-dispatching", "stage-ready", "owner-ready", "moved", "repair-dispatching", "restored"} {
		t.Run(phase, func(t *testing.T) {
			s, _, scope := worktreeRestoreStore(t)
			ctx := context.Background()
			r := restoreOrdinaryReceipt(t, s, scope, "cancel-partial-input")
			a, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, r.ID, `{"version":1}`, 100)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.ClaimWorktreeRestoreAttempt(ctx, a, 101); err != nil || !ok {
				t.Fatal(err)
			}
			checkpoint := fmt.Sprintf(`{"version":1,"phase":%q,"stageIdentity":"synthetic-frozen-stage"}`, phase)
			if err := s.CheckpointWorktreeRestoreAttempt(ctx, a, phase, checkpoint, 102); err != nil {
				t.Fatal(err)
			}
			if err := s.RecoverWorktreeRestoreDispatches(ctx, scope, 103); err != nil {
				t.Fatal(err)
			}
			a, _ = s.WorktreeRestoreAttempt(ctx, scope, r.ID)
			cancelWorktreeRestoreInput(t, s, scope, r)
			verifiedPhase := phase
			if phase == "stage-dispatching" {
				verifiedPhase = "stage-ready" // Reconcile can promote a durable physical stage.
			}
			allowed := phase == "planned" || phase == "stage-dispatching" || phase == "stage-ready" || phase == "owner-ready"
			if !allowed {
				verifiedPhase = "planned" // A moved root must not be downgraded to release its lock.
			}
			proof := fmt.Sprintf(`{"version":1,"phase":%q,"stageIdentity":"synthetic-frozen-stage","verified":true}`, verifiedPhase)
			err = s.AbandonCanceledWorktreeRestore(ctx, a, verifiedPhase, proof, 104)
			if !allowed {
				if !errors.Is(err, ErrConflict) {
					t.Fatal("released a moved/repair/restored operation", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			current, err := s.WorktreeRestoreAttempt(ctx, scope, r.ID)
			if err != nil || current.State != "blocked" || current.ErrorCode != "input-canceled" || current.ResultJSON != proof || current.Phase != verifiedPhase || current.ParamsJSON != a.ParamsJSON {
				t.Fatal("abandon did not preserve complete physical proof", err)
			}
			later := restoreOrdinaryReceipt(t, s, scope, "later-after-abandon")
			if _, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, later.ID, `{"version":1}`, 105); err != nil {
				t.Fatal("verified canceled operation blocked new permission", err)
			}
			input, err := s.Submission(ctx, scope, r.ID)
			if err != nil || input.State != "removed" {
				t.Fatal("abandon resurrected canceled input", err)
			}
		})
	}
}

func TestWorktreeRestoreAbandonRequiresExactLocalCancellationWithoutNativeEvidence(t *testing.T) {
	for _, damage := range []string{"no-action", "token", "client", "revision", "receipt-only", "native-attempt", "queue", "history", "scope", "stale-checkpoint"} {
		t.Run(damage, func(t *testing.T) {
			s, _, scope := worktreeRestoreStore(t)
			ctx := context.Background()
			r := restoreOrdinaryReceipt(t, s, scope, "cancel-proof-input")
			a, err := s.PrepareWorktreeRestoreAttempt(ctx, scope, r.ID, `{"version":1}`, 100)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.ClaimWorktreeRestoreAttempt(ctx, a, 101); err != nil || !ok {
				t.Fatal(err)
			}
			if err := s.RecoverWorktreeRestoreDispatches(ctx, scope, 102); err != nil {
				t.Fatal(err)
			}
			a, _ = s.WorktreeRestoreAttempt(ctx, scope, r.ID)
			cancelWorktreeRestoreInput(t, s, scope, r)
			var query string
			var args []any
			switch damage {
			case "no-action":
				query, args = `DELETE FROM submission_actions WHERE receipt_id=?`, []any{r.ID}
			case "token":
				query, args = `UPDATE submission_actions SET token='foreign' WHERE receipt_id=?`, []any{r.ID}
			case "client":
				query, args = `UPDATE submission_actions SET client_id='foreign' WHERE receipt_id=?`, []any{r.ID}
			case "revision":
				query, args = `UPDATE submission_actions SET revision=revision+1 WHERE receipt_id=?`, []any{r.ID}
			case "receipt-only":
				query, args = `UPDATE submission_inputs SET state='received' WHERE receipt_id=?`, []any{r.ID}
			case "native-attempt":
				query, args = `INSERT INTO submission_attempts(attempt_id,host_id,app_id,thread_id,receipt_id,stage,state,created_at_ms) VALUES(?,?,?,?,?,'add','confirmed',1)`, []any{ID(), scope.HostID, scope.AppID, scope.ThreadID, r.ID}
			case "queue":
				query, args = `UPDATE m0_receipts SET queue_id='native-queue' WHERE id=?`, []any{r.ID}
			case "history":
				query, args = `INSERT INTO input_history_links(host_id,thread_id,turn_id,item_id,receipt_id) VALUES(?,?,'turn','item',?)`, []any{scope.HostID, scope.ThreadID, r.ID}
			case "scope":
				a.Scope.AppID = "foreign-app"
			case "stale-checkpoint":
				a.ResultJSON = `{"version":1,"phase":"planned"}`
			}
			if query != "" {
				if _, err := s.DB.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.AbandonCanceledWorktreeRestore(ctx, a, "planned", `{"version":1,"phase":"planned","verified":true}`, 103); err == nil {
				t.Fatal("unproven cancellation released unknown operation", damage)
			}
		})
	}
}

func TestWorktreeRestorePublicationCandidatesRequireLaterOrdinaryGrantAfterExactCancel(t *testing.T) {
	s, _, scope := worktreeRestoreStore(t)
	ctx := context.Background()
	older := restoreOrdinaryReceipt(t, s, scope, "older-input")
	original := restoreOrdinaryReceipt(t, s, scope, "restore-canceled-input")
	later := restoreOrdinaryReceipt(t, s, scope, "later-ordinary-input")
	removed := restoreOrdinaryReceipt(t, s, scope, "later-removed-input")
	noGrant := restoreOrdinaryReceipt(t, s, scope, "later-no-grant-input")
	if candidates, err := s.WorktreeRestorePublicationCandidates(ctx, scope, original.ID); err != nil || len(candidates) != 0 {
		t.Fatal("uncanceled receipt selected substitute publication evidence", err)
	}
	cancelWorktreeRestoreInput(t, s, scope, original)
	cancelWorktreeRestoreInput(t, s, scope, removed)
	if _, err := s.DB.Exec(`DELETE FROM worktree_restore_grants WHERE receipt_id=?`, noGrant.ID); err != nil {
		t.Fatal(err)
	}
	candidates, err := s.WorktreeRestorePublicationCandidates(ctx, scope, original.ID)
	if err != nil || len(candidates) != 1 || candidates[0].ReceiptID != later.ID || candidates[0].ClientID != later.ClientID || candidates[0].ReceiptID == older.ID {
		t.Fatal("candidate lacked frozen ordinary origin or strict receipt order", candidates, err)
	}
	if _, err := s.DB.Exec(`UPDATE submission_order SET sequence=0 WHERE receipt_id=?`, later.ID); err != nil {
		t.Fatal(err)
	}
	if candidates, err := s.WorktreeRestorePublicationCandidates(ctx, scope, original.ID); err != nil || len(candidates) != 0 {
		t.Fatal("publication inferred order from admission time instead of sequence", err)
	}
	if candidates, err := s.WorktreeRestorePublicationCandidates(ctx, SubmissionScope{scope.HostID, "foreign-app", scope.ThreadID}, original.ID); err == nil || len(candidates) != 0 {
		t.Fatal("publication candidate crossed application scope")
	}
	if _, err := s.DB.Exec(`DELETE FROM submission_actions WHERE receipt_id=?`, original.ID); err != nil {
		t.Fatal(err)
	}
	if candidates, err := s.WorktreeRestorePublicationCandidates(ctx, scope, original.ID); err != nil || len(candidates) != 0 {
		t.Fatal("removed label without exact local action authorized publication", err)
	}
}
