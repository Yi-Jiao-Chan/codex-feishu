package verification

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codex-feishu/internal/store"
)

func TestBindingStateReportsScopedInputEvidenceWithoutNativeReadsOrWrites(t *testing.T) {
	m, n, g, id := bindingFixture(t)
	ctx := context.Background()
	b, err := m.Begin(ctx, id)
	if err != nil || b.InputEvidenceStatus != "pass" || b.InputEvidence == nil || *b.InputEvidence != (store.VerificationInputEvidence{}) {
		t.Fatal("empty readable snapshot was not distinguished from missing proof", b, err)
	}
	db := m.Sessions.native.Store
	r, _, err := db.Claim(ctx, store.Receipt{AppID: m.Groups.AppID, ThreadID: b.ThreadID, ChatID: b.ChatID, MessageID: "fixture-owner-input", Body: "PRIVATE TEST BODY"})
	if err != nil {
		t.Fatal(err)
	}
	scope := store.SubmissionScope{HostID: m.Groups.HostID, AppID: m.Groups.AppID, ThreadID: b.ThreadID}
	if err := db.StageSubmissions(ctx, scope); err != nil {
		t.Fatal(err)
	}
	a, claimed, err := db.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add")
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	reads, joins := g.reads.Load(), n.joins.Load()
	b, err = m.State(ctx, id)
	if err != nil || b.InputEvidenceStatus != "pass" || b.InputEvidence == nil || b.InputEvidence.StagedInputs != 1 || b.InputEvidence.AddConfirmed != 0 || b.InputEvidence.UnresolvedAttempts != 1 {
		t.Fatal("dispatching input was promoted", b, err)
	}
	if err := db.FinishSubmissionAttempt(ctx, scope, a.ID, "confirmed", "fixture-native-queue", -1); err != nil {
		t.Fatal(err)
	}
	if err := db.ObserveSubmission(ctx, scope, r.ID, "consumed", "fixture-native-queue", 0, "fixture-native-turn", "fixture-native-user-item"); err != nil {
		t.Fatal(err)
	}
	var before, after int
	if err := db.DB.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	b, err = m.State(ctx, id)
	if err != nil || b.InputEvidence == nil || b.InputEvidence.AddConfirmed != 1 || b.InputEvidence.HistoryLinked != 1 || b.InputEvidence.UnresolvedAttempts != 0 || b.InputEvidence.SteerConfirmed != 0 || b.InputEvidence.SteerHistoryLinked != 0 {
		t.Fatal("scoped persisted facts missing or invented steer", b, err)
	}
	if err := db.DB.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil || before != after || g.reads.Load() != reads || n.joins.Load() != joins || n.calls.Load() != 1 || g.calls.Load() != 1 {
		t.Fatal("state performed writes, network checks or creation", err)
	}
	body, err := json.Marshal(b)
	if err != nil || strings.Contains(string(body), "PRIVATE TEST BODY") || strings.Contains(string(body), r.ActionToken) || strings.Contains(string(body), r.ClientID) || strings.Contains(string(body), r.MessageID) {
		t.Fatal("private input evidence escaped public DTO", err)
	}
}

func TestBindingStateReadFailureKeepsInputEvidencePendingWithoutRepair(t *testing.T) {
	m, _, _, id := bindingFixture(t)
	ctx := context.Background()
	if _, err := m.Begin(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Sessions.native.Store.DB.Exec(`DROP TABLE submission_attempts`); err != nil {
		t.Fatal(err)
	}
	b, err := m.State(ctx, id)
	if err != nil || b.BindingStatus != "pass" || b.InputEvidenceStatus != "pending" || b.InputEvidence != nil {
		t.Fatal("read failure became successful empty evidence", b, err)
	}
	var table int
	if err := m.Sessions.native.Store.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='submission_attempts'`).Scan(&table); err != nil || table != 0 {
		t.Fatal("state repaired schema", table, err)
	}
}
