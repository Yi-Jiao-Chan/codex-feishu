package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestWorktreeRestoreSchema34MigrationPreservesExistingJournals(t *testing.T) {
	ctx := context.Background()
	s, path, scope := taskControlStore(t, true)

	command := CommandRequest{
		HostID: "host", AppID: "app", ThreadID: "thread", ChatID: "chat", ChatType: "group",
		MessageID: "migration-settings", UserID: "operator", Operators: []string{"operator"}, Kind: "model_settings",
	}
	if _, fresh, err := s.ApplyLocalCommand(ctx, command, time.Unix(200, 0)); err != nil || !fresh {
		t.Fatalf("create settings fixture: fresh=%v err=%v", fresh, err)
	}
	requests, err := s.SessionSettingsRequests(ctx, scope)
	if err != nil || len(requests) != 1 {
		t.Fatalf("settings request fixture: %v %v", requests, err)
	}
	beforeRequest := settingsFormRequest(t, s, requests[0].ID)
	beforeForm, err := s.SessionSettingsForm(ctx, beforeRequest.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeReply, err := s.CommandReply(ctx, scope.HostID, scope.AppID, command.MessageID)
	if err != nil {
		t.Fatal(err)
	}

	r := restoreOrdinaryReceipt(t, s, scope, "migration-input")
	a, claimed, err := s.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add")
	if err != nil || !claimed {
		t.Fatalf("create unknown native journal: claimed=%v err=%v", claimed, err)
	}
	if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, "unknown", "", -1); err != nil {
		t.Fatal(err)
	}
	beforeReceipt, err := s.ByToken(ctx, r.ActionToken)
	if err != nil {
		t.Fatal(err)
	}
	beforeSubmission, err := s.Submission(ctx, scope, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeAttempts, err := s.SubmissionAttempts(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if beforeSubmission.State != "unknown" || len(beforeAttempts) != 1 || beforeAttempts[0].State != "unknown" {
		t.Fatalf("fixture did not create an unknown native journal: %+v %+v", beforeSubmission, beforeAttempts)
	}
	var grants int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM worktree_restore_grants`).Scan(&grants); err != nil || grants == 0 {
		t.Fatalf("ordinary admission did not create the disposable restore grant: %d %v", grants, err)
	}

	// Model an already deployed schema 33: remove only schema-34 state, after
	// deleting its receipt-bound grant, while leaving all older journals intact.
	for _, statement := range []string{
		`DELETE FROM worktree_restore_attempts`,
		`DELETE FROM worktree_restore_grants`,
		`DELETE FROM thread_worktree_identities`,
		`DROP INDEX IF EXISTS worktree_restore_one_pending`,
		`DROP INDEX IF EXISTS worktree_restore_grant_thread`,
		`DROP TABLE worktree_restore_attempts`,
		`DROP TABLE worktree_restore_grants`,
		`DROP TABLE thread_worktree_identities`,
		`PRAGMA user_version=33`,
	} {
		if _, err := s.DB.Exec(statement); err != nil {
			t.Fatalf("prepare schema33 fixture with %q: %v", statement, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	assertMigrated := func(got *Store) {
		t.Helper()
		var version int
		if err := got.DB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
			t.Fatalf("schema version=%d err=%v", version, err)
		}
		for _, name := range []string{"thread_worktree_identities", "worktree_restore_grants", "worktree_restore_attempts"} {
			var count int
			if err := got.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil || count != 1 {
				t.Fatalf("schema34 table %q missing: count=%d err=%v", name, count, err)
			}
			if err := got.DB.QueryRow(`SELECT COUNT(*) FROM ` + name).Scan(&count); err != nil || count != 0 {
				t.Fatalf("migration invented %s rows: count=%d err=%v", name, count, err)
			}
		}
		for _, name := range []string{"worktree_restore_grant_thread", "worktree_restore_one_pending"} {
			var count int
			if err := got.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&count); err != nil || count != 1 {
				t.Fatalf("schema34 index %q missing: count=%d err=%v", name, count, err)
			}
		}
		var integrity string
		if err := got.DB.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("integrity check=%q err=%v", integrity, err)
		}
		rows, err := got.DB.Query(`PRAGMA foreign_key_check`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if rows.Next() {
			var table, rowid, parent, fk any
			if err := rows.Scan(&table, &rowid, &parent, &fk); err != nil {
				t.Fatal(err)
			}
			t.Fatalf("foreign key violation: table=%v rowid=%v parent=%v fk=%v", table, rowid, parent, fk)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}

		gotReceipt, err := got.ByToken(ctx, beforeReceipt.ActionToken)
		if err != nil || gotReceipt != beforeReceipt {
			t.Fatalf("receipt changed across schema34: got=%+v want=%+v err=%v", gotReceipt, beforeReceipt, err)
		}
		gotSubmission, err := got.Submission(ctx, scope, r.ID)
		if err != nil || !reflect.DeepEqual(gotSubmission, beforeSubmission) {
			t.Fatalf("submission changed across schema34: got=%+v want=%+v err=%v", gotSubmission, beforeSubmission, err)
		}
		gotAttempts, err := got.SubmissionAttempts(ctx, scope)
		if err != nil || !reflect.DeepEqual(gotAttempts, beforeAttempts) {
			t.Fatalf("native journal changed across schema34: got=%+v want=%+v err=%v", gotAttempts, beforeAttempts, err)
		}
		gotRequest := settingsFormRequest(t, got, beforeRequest.ID)
		if gotRequest != beforeRequest {
			t.Fatalf("settings request changed across schema34: got=%+v want=%+v", gotRequest, beforeRequest)
		}
		gotForm, err := got.SessionSettingsForm(ctx, beforeForm.RequestID)
		if err != nil || gotForm != beforeForm {
			t.Fatalf("settings form changed across schema34: got=%+v want=%+v err=%v", gotForm, beforeForm, err)
		}
		gotReply, err := got.CommandReply(ctx, scope.HostID, scope.AppID, command.MessageID)
		if err != nil || gotReply != beforeReply {
			t.Fatalf("settings command changed across schema34: got=%+v want=%+v err=%v", gotReply, beforeReply, err)
		}
	}

	next, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	assertMigrated(next)
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertMigrated(reopened)
}
