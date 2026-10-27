package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestRetrySchema37PreservesUnknownNativeJournals(t *testing.T) {
	s, path, scope, _ := submissionStore(t)
	ctx := context.Background()
	action, err := s.ClaimThreadSubmissionActionExpected(ctx, scope, "stop", "old-stop", "original")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PrepareSubmissionAction(ctx, scope, action.ID, "interrupt", "original", "", ""); err != nil {
		t.Fatal(err)
	}
	attempt, claimed, err := s.ClaimSubmissionAttempt(ctx, scope, "", action.ID, "interrupt")
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err = s.FinishSubmissionAttempt(ctx, scope, attempt.ID, "unknown", "", -1); err != nil {
		t.Fatal(err)
	}
	action, _ = s.SubmissionAction(ctx, scope, action.ID)
	attempts, _ := s.SubmissionAttempts(ctx, scope)
	// Recreate the real v36 kind constraint while preserving the fixture's
	// journals, then exercise the normal Open migration rather than a helper.
	oldSchema := strings.Replace(SubmissionSchema, "'continue','retry'", "'continue'", 1)
	if _, err = s.DB.Exec(`ALTER TABLE submission_actions RENAME TO retained_actions`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`INSERT INTO submission_actions SELECT * FROM retained_actions ORDER BY rowid; DROP TABLE retained_actions; PRAGMA user_version=36`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err = s.DB.QueryRow(`SELECT sql FROM sqlite_master WHERE name='submission_actions'`).Scan(&schema); err != nil || strings.Contains(schema, "'retry'") {
		t.Fatal("fixture did not restore the v36 constraint", err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.SubmissionAction(ctx, scope, action.ID)
	afterAttempts, readErr := reopened.SubmissionAttempts(ctx, scope)
	if err != nil || readErr != nil || !reflect.DeepEqual(got, action) || !reflect.DeepEqual(afterAttempts, attempts) {
		t.Fatal("schema37 altered unknown native operation evidence", err, readErr)
	}
	if _, err = reopened.ClaimThreadSubmissionActionExpected(ctx, scope, "retry", "new-retry", "failed"); err != ErrSubmissionBusy {
		t.Fatal("migration bypassed the prior unknown operation")
	}
	if err = reopened.FinishSubmissionAttempt(ctx, scope, attempt.ID, "rejected", "", -1); err != nil {
		t.Fatal(err)
	}
	retry, err := reopened.ClaimThreadSubmissionActionExpected(ctx, scope, "retry", "new-retry", "failed")
	if err != nil || retry.ExpectedTurnID != "failed" || retry.Kind != "retry" {
		t.Fatal("upgraded journal could not admit a bound retry", err)
	}
	var index int
	if err = reopened.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='submission_one_active_item_action' AND tbl_name='submission_actions'`).Scan(&index); err != nil || index != 1 {
		t.Fatal("migration lost native action admission index", err)
	}
	rows, err := reopened.DB.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() || rows.Err() != nil {
		t.Fatal("migration broke an existing foreign key")
	}
}
