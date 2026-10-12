package verification

import (
	"context"
	"testing"

	"codex-feishu/internal/store"
)

func TestBindingStateDeliverySnapshotIsEmptyUntilNativeCardsAndReadFailureStaysPending(t *testing.T) {
	m, n, g, id := bindingFixture(t)
	ctx := context.Background()
	b, err := m.Begin(ctx, id)
	if err != nil || b.DeliveryEvidenceStatus != "pass" || b.DeliveryEvidence == nil || *b.DeliveryEvidence != (store.VerificationDeliveryEvidence{}) {
		t.Fatal("empty readable delivery snapshot was not distinguished from missing proof", b.DeliveryEvidence, err)
	}
	reads, joins := g.reads.Load(), n.joins.Load()
	b, err = m.State(ctx, id)
	if err != nil || b.DeliveryEvidenceStatus != "pass" || b.DeliveryEvidence == nil || *b.DeliveryEvidence != (store.VerificationDeliveryEvidence{}) || g.reads.Load() != reads || n.joins.Load() != joins {
		t.Fatal("delivery query invoked remote checks or invented cards", b.DeliveryEvidence, err)
	}
	if _, err := m.Sessions.native.Store.DB.Exec(`DROP TABLE outbound_attempts`); err != nil {
		t.Fatal(err)
	}
	b, err = m.State(ctx, id)
	if err != nil || b.BindingStatus != "pass" || b.InputEvidenceStatus != "pass" || b.DeliveryEvidenceStatus != "pending" || b.DeliveryEvidence != nil {
		t.Fatal("delivery read failure became successful empty evidence or broke separate input evidence", b, err)
	}
	var table int
	if err := m.Sessions.native.Store.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='outbound_attempts'`).Scan(&table); err != nil || table != 0 {
		t.Fatal("query repaired schema", table, err)
	}
}
