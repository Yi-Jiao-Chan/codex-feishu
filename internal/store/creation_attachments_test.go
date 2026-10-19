package store

import (
	"context"
	"reflect"
	"testing"
)

func TestCreationAttachmentDescriptorsAreImmutableAndMaterializeAtomically(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d := creationTestDraft("source-materials", 100)
	d.Attachments = []CreationAttachment{{Kind: "image", ResourceKey: "img_original"}, {Kind: "file", ResourceKey: "file_original", Name: "original.csv"}}
	d = receiveCreationTestDraft(t, s, d)
	want := append([]CreationAttachment{}, d.Attachments...)
	replay := d
	replay.Attachments = []CreationAttachment{{Kind: "file", ResourceKey: "file_replay"}}
	again, fresh, err := s.ReceiveCreationDraft(ctx, replay)
	if err != nil || fresh || !reflect.DeepEqual(again.Attachments, want) {
		t.Fatal("source replay changed immutable resources", err)
	}
	replay.Body = "edited task"
	d, err = s.UpdateCreationDraft(ctx, replay, d.Revision, 101)
	if err != nil || d.Body != "edited task" || !reflect.DeepEqual(d.Attachments, want) {
		t.Fatal("body edit replaced resource descriptors", err)
	}
	if pending, err := s.HasCollectingAttachments(ctx, d.HostID, d.AppID); err != nil || pending {
		t.Fatal("unconfirmed draft activated the downloader", err)
	}
	claimCreationTestIntent(t, s, d, "owner", "materials", 102)
	if pending, err := s.HasCollectingAttachments(ctx, d.HostID, d.AppID); err != nil || !pending {
		t.Fatal("confirmed creation cannot resume downloader before materialization", err)
	}
	confirmCreationStep(t, s, d.ID, "group", `{}`, "task-chat", 103)
	confirmCreationStep(t, s, d.ID, "root", `{}`, "task-root", 104)
	confirmCreationStep(t, s, d.ID, "native", `{}`, "native-thread", 105)
	if _, err := s.DB.Exec(`CREATE TRIGGER reject_creation_part BEFORE INSERT ON attachment_parts BEGIN SELECT RAISE(FAIL,'injected attachment failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MaterializeCreation(ctx, d.ID); err == nil {
		t.Fatal("failed resource insert was ignored")
	}
	for _, table := range []string{"bindings", "m0_receipts", "submission_inputs", "attachment_batches", "attachment_events", "attachment_parts"} {
		var count int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("partial materialization escaped the transaction", table, err)
		}
	}
	if _, err := s.DB.Exec(`DROP TRIGGER reject_creation_part`); err != nil {
		t.Fatal(err)
	}
	_, receipt, err := s.MaterializeCreation(ctx, d.ID)
	if err != nil || receipt.State != "collecting" {
		t.Fatal("initial input was not sealed for collection", err)
	}
	if _, _, err := s.MaterializeCreation(ctx, d.ID); err != nil {
		t.Fatal("replayed materialization failed", err)
	}
	for table, want := range map[string]int{"m0_receipts": 1, "submission_inputs": 1, "submission_order": 1, "attachment_batches": 1, "attachment_events": 1, "attachment_parts": 3} {
		var count int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != want {
			t.Fatal("materialization duplicated resources or receipts", table, count, err)
		}
	}
}

func TestCreationAttachmentMigrationPreservesSchema29Workflow(t *testing.T) {
	s, path := discoveryStore(t)
	ctx := context.Background()
	if _, err := s.BeginBaseline(ctx, creationTestHost, creationTestApp, 100); err != nil {
		t.Fatal(err)
	}
	d, _ := creationReadyForMaterialize(t, s, "schema29-source", 100)
	binding, receipt, err := s.MaterializeCreation(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	d, err = s.CreationDraft(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	i, err := s.CreationIntent(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`ALTER TABLE creation_drafts DROP COLUMN attachments_json; PRAGMA user_version=29`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	got, err := upgraded.CreationDraft(ctx, d.ID)
	if err != nil || !reflect.DeepEqual(got, d) || len(got.Attachments) != 0 {
		t.Fatal("migration changed the old draft", err)
	}
	gotIntent, err := upgraded.CreationIntent(ctx, d.ID)
	if err != nil || !reflect.DeepEqual(gotIntent, i) {
		t.Fatal("migration changed the frozen workflow", err)
	}
	gotBinding, gotReceipt, err := upgraded.MaterializeCreation(ctx, d.ID)
	if err != nil || gotBinding != binding || !reflect.DeepEqual(gotReceipt, receipt) {
		t.Fatal("migration replaced the existing binding/receipt", err)
	}
}
