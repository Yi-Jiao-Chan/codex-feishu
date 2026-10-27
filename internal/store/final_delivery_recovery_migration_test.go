package store

import (
	"context"
	"testing"
)

func TestFinalDeliveryRecoverySchema35PreservesRejectedJournal(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	eligible(t, s, "thread")
	bindMirrorThread(t, s)
	final := Mirror{Key: "final", TurnID: "turn", Kind: "final", Card: `{"body":"frozen rejected answer"}`, Terminal: true, ParentMessageID: "input"}
	stage(t, s, 100, final)
	attempt := claimMirror(t, s, final.Key, 1000)
	if err := s.FinishMirrorAttempt(ctx, attempt.ID, "rejected", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE outbound_attempts SET card_json=? WHERE attempt_id=?`, attempt.Card, attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE mirrors SET card_json=? WHERE mirror_key=?`, final.Card, final.Key); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE outbound DROP COLUMN retry_of_attempt_id`,
		`ALTER TABLE outbound_attempts DROP COLUMN retry_of_attempt_id`,
		`ALTER TABLE outbound_attempts DROP COLUMN rejection_code`,
		`ALTER TABLE outbound_attempts DROP COLUMN rejection_http_status`,
		`ALTER TABLE outbound_attempts DROP COLUMN rejection_request_id`,
		`ALTER TABLE outbound_attempts DROP COLUMN card_hash`,
		`ALTER TABLE outbound_attempts DROP COLUMN cot_payload_hash`,
		`ALTER TABLE mirrors DROP COLUMN card_hash`,
		`ALTER TABLE mirrors DROP COLUMN projection_version`,
		`ALTER TABLE mirrors DROP COLUMN has_callback`,
		`ALTER TABLE mirrors DROP COLUMN callback_proofs`,
		`PRAGMA user_version=34`,
	} {
		if _, err := s.DB.Exec(statement); err != nil {
			t.Fatalf("prepare schema34 fixture with %q: %v", statement, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var version int
	if err := reopened.DB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	got, err := reopened.MirrorAttempt(ctx, attempt.ID)
	if err != nil || got.State != "rejected" || got.Card != attempt.Card || got.Revision != attempt.Revision ||
		got.RetryOfAttemptID != "" || got.RejectionCode != 0 || got.RejectionHTTPStatus != 0 || got.RejectionRequestID != "" {
		t.Fatalf("schema36 changed rejected evidence: %+v %v", got, err)
	}
	mirror, err := reopened.Mirror(ctx, "host", "thread", final.Key)
	if err != nil || mirror.State != "rejected" || mirror.Card != final.Card || mirror.RemoteID != "" {
		t.Fatalf("schema36 reopened rejected work: %+v %v", mirror, err)
	}
	var failures int
	if err := reopened.DB.QueryRow(`SELECT COUNT(*) FROM mirrors WHERE mirror_key LIKE 'ui:delivery-failure:%'`).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("migration invented failure cards: %d %v", failures, err)
	}
}
