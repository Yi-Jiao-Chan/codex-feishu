package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func creationAttachmentFixture() []CreationAttachment {
	return []CreationAttachment{
		{Kind: "image", ResourceKey: "img-first", Name: "first.png"},
		{Kind: "file", ResourceKey: "file-second", Name: "second.txt"},
		{Kind: "file", ResourceKey: "file-third", Name: "third.txt"},
	}
}

func TestRemoveCreationDraftAttachmentPreservesOrderAndValidatedFields(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d := creationTestDraft("source-remove-fields", 100)
	d.Body = "original body"
	d.FollowMode = "topic"
	d.WorkspaceMode = "worktree"
	d.BaseBranch = "refs/heads/main"
	d.Attachments = creationAttachmentFixture()
	d = receiveCreationTestDraft(t, s, d)

	update := d
	update.Body = "edited body"
	update.FollowMode = "topic"
	update.WorkspaceMode = "worktree"
	update.BaseBranch = "refs/heads/feature"
	// The caller cannot replace the stored resources while deleting one.
	update.Attachments = []CreationAttachment{{Kind: "file", ResourceKey: "forged"}}
	got, err := s.RemoveCreationDraftAttachment(ctx, update, d.Revision, 1, 200)
	if err != nil {
		t.Fatalf("remove attachment: %v", err)
	}
	wantAttachments := []CreationAttachment{creationAttachmentFixture()[0], creationAttachmentFixture()[2]}
	if got.Revision != d.Revision+1 || !reflect.DeepEqual(got.Attachments, wantAttachments) {
		t.Fatalf("removed draft=%+v, want revision=%d attachments=%+v", got, d.Revision+1, wantAttachments)
	}
	reloaded, err := s.CreationDraft(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Body != update.Body || reloaded.FollowMode != update.FollowMode || reloaded.WorkspaceMode != update.WorkspaceMode || reloaded.BaseBranch != update.BaseBranch || !reflect.DeepEqual(reloaded.Attachments, wantAttachments) {
		t.Fatalf("remove did not persist validated form fields and stored resources: %+v", reloaded)
	}
}

func TestRemoveCreationDraftAttachmentRejectsReplayInvalidScopeAndState(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	base := creationTestDraft("source-remove-replay", 100)
	base.Attachments = creationAttachmentFixture()
	d := receiveCreationTestDraft(t, s, base)
	removed, err := s.RemoveCreationDraftAttachment(ctx, d, d.Revision, 0, 200)
	if err != nil {
		t.Fatalf("initial remove: %v", err)
	}
	for name, caller := range map[string]CreationDraft{
		"same revision replay": d,
		"negative index":       removed,
		"out of range":         removed,
	} {
		t.Run(name, func(t *testing.T) {
			index := 0
			if name == "negative index" {
				index = -1
			}
			if name == "out of range" {
				index = len(removed.Attachments)
			}
			expected := d.Revision
			if name != "same revision replay" {
				expected = removed.Revision
			}
			if _, err := s.RemoveCreationDraftAttachment(ctx, caller, expected, index, 200); !errors.Is(err, ErrSubmissionStale) {
				t.Fatalf("remove error=%v, want ErrSubmissionStale", err)
			}
		})
	}
	assertCreationAttachmentKeys(t, s, d.ID, []string{"file-second", "file-third"})

	cross := receiveCreationTestDraft(t, s, func() CreationDraft {
		v := creationTestDraft("source-remove-scope", 300)
		v.Attachments = creationAttachmentFixture()
		return v
	}())
	for name, mutate := range map[string]func(*CreationDraft){
		"creator": func(v *CreationDraft) { v.CreatorID = "other-creator" },
		"host":    func(v *CreationDraft) { v.HostID = "other-host" },
		"app":     func(v *CreationDraft) { v.AppID = "other-app" },
		"token":   func(v *CreationDraft) { v.Token = "forged-token" },
		"card":    func(v *CreationDraft) { v.CardID = "forged-card" },
	} {
		t.Run("cross "+name, func(t *testing.T) {
			caller := cross
			mutate(&caller)
			if _, err := s.RemoveCreationDraftAttachment(ctx, caller, cross.Revision, 0, 400); !errors.Is(err, ErrSubmissionStale) {
				t.Fatalf("cross-scope remove error=%v, want ErrSubmissionStale", err)
			}
		})
	}
	assertCreationAttachmentKeys(t, s, cross.ID, []string{"img-first", "file-second", "file-third"})

	expired := receiveCreationTestDraft(t, s, func() CreationDraft {
		v := creationTestDraft("source-remove-expired", 500)
		v.ExpiresAtMS = 550
		v.Attachments = creationAttachmentFixture()
		return v
	}())
	if _, err := s.RemoveCreationDraftAttachment(ctx, expired, expired.Revision, 0, 600); !errors.Is(err, ErrSubmissionStale) {
		t.Fatalf("expired remove error=%v, want ErrSubmissionStale", err)
	}
	assertCreationAttachmentKeys(t, s, expired.ID, []string{"img-first", "file-second", "file-third"})

	creating := receiveCreationTestDraft(t, s, func() CreationDraft {
		v := creationTestDraft("source-remove-creating", 700)
		v.Attachments = creationAttachmentFixture()
		return v
	}())
	claimCreationTestIntent(t, s, creating, "owner", "creating", 701)
	creating, err = s.CreationDraft(ctx, creating.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveCreationDraftAttachment(ctx, creating, creating.Revision, 0, 702); !errors.Is(err, ErrSubmissionStale) {
		t.Fatalf("creating remove error=%v, want ErrSubmissionStale", err)
	}
	assertCreationAttachmentKeys(t, s, creating.ID, []string{"img-first", "file-second", "file-third"})

	canceled := receiveCreationTestDraft(t, s, func() CreationDraft {
		v := creationTestDraft("source-remove-canceled", 800)
		v.Attachments = creationAttachmentFixture()
		return v
	}())
	if err := s.CancelCreationDraft(ctx, canceled, 801); err != nil {
		t.Fatal(err)
	}
	canceled, err = s.CreationDraft(ctx, canceled.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveCreationDraftAttachment(ctx, canceled, canceled.Revision, 0, 802); !errors.Is(err, ErrSubmissionStale) {
		t.Fatalf("canceled remove error=%v, want ErrSubmissionStale", err)
	}
	assertCreationAttachmentKeys(t, s, canceled.ID, []string{"img-first", "file-second", "file-third"})
}

func TestRemoveCreationDraftAttachmentSurvivesRestartAndMaterializesRemainingResources(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	if needed, err := s.BeginBaseline(ctx, creationTestHost, creationTestApp, 100); err != nil || !needed {
		t.Fatalf("begin creation baseline: needed=%v err=%v", needed, err)
	}
	d := creationTestDraft("source-remove-restart", 100)
	d.Attachments = creationAttachmentFixture()
	d = receiveCreationTestDraft(t, s, d)
	if _, err := s.RemoveCreationDraftAttachment(ctx, d, d.Revision, 1, 200); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted, err := s.CreationDraft(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantRemaining := []CreationAttachment{creationAttachmentFixture()[0], creationAttachmentFixture()[2]}
	if !reflect.DeepEqual(restarted.Attachments, wantRemaining) {
		t.Fatalf("restart lost attachment order: got=%+v want=%+v", restarted.Attachments, wantRemaining)
	}
	claimCreationTestIntent(t, s, restarted, "owner", "restart", 201)
	confirmCreationStep(t, s, restarted.ID, "group", `{}`, "task-chat", 202)
	confirmCreationStep(t, s, restarted.ID, "root", `{}`, "task-root", 203)
	confirmCreationStep(t, s, restarted.ID, "native", `{}`, "native-thread", 204)
	_, receipt, err := s.MaterializeCreation(ctx, restarted.ID)
	if err != nil || receipt.State != "collecting" {
		t.Fatalf("materialization did not retain remaining attachments: receipt=%+v err=%v", receipt, err)
	}
	assertCreationMaterializedKeys(t, s, receipt.ID, []string{"img-first", "file-third"})
}

func TestRemoveAllCreationAttachmentsUsesOrdinaryInitialInputAfterRestart(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	if needed, err := s.BeginBaseline(ctx, creationTestHost, creationTestApp, 100); err != nil || !needed {
		t.Fatalf("begin creation baseline: needed=%v err=%v", needed, err)
	}
	d := creationTestDraft("source-remove-all", 300)
	d.Attachments = []CreationAttachment{{Kind: "image", ResourceKey: "img-only", Name: "only.png"}}
	d = receiveCreationTestDraft(t, s, d)
	removed, err := s.RemoveCreationDraftAttachment(ctx, d, d.Revision, 0, 400)
	if err != nil || len(removed.Attachments) != 0 {
		t.Fatalf("remove last attachment: draft=%+v err=%v", removed, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted, err := s.CreationDraft(ctx, d.ID)
	if err != nil || len(restarted.Attachments) != 0 {
		t.Fatalf("deleted attachment returned after restart: %+v err=%v", restarted, err)
	}
	claimCreationTestIntent(t, s, restarted, "owner", "no-attachments", 401)
	confirmCreationStep(t, s, restarted.ID, "group", `{}`, "task-chat", 402)
	confirmCreationStep(t, s, restarted.ID, "root", `{}`, "task-root", 403)
	confirmCreationStep(t, s, restarted.ID, "native", `{}`, "native-thread", 404)
	_, receipt, err := s.MaterializeCreation(ctx, restarted.ID)
	if err != nil || receipt.State != "received" {
		t.Fatalf("empty attachment draft did not use ordinary input: receipt=%+v err=%v", receipt, err)
	}
	var batches, parts int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM attachment_batches WHERE receipt_id=?", receipt.ID).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM attachment_parts WHERE batch_id IN (SELECT batch_id FROM attachment_batches WHERE receipt_id=?)", receipt.ID).Scan(&parts); err != nil {
		t.Fatal(err)
	}
	if batches != 0 || parts != 0 {
		t.Fatalf("empty attachment materialization created resource rows: batches=%d parts=%d", batches, parts)
	}
}

func TestRemoveCreationDraftAttachmentUpdateFailureIsAtomic(t *testing.T) {
	s := creationTestStore(t)
	ctx := context.Background()
	d := creationTestDraft("source-remove-failure", 100)
	d.Body = "before body"
	d.Attachments = creationAttachmentFixture()
	d = receiveCreationTestDraft(t, s, d)
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_creation_attachment_update BEFORE UPDATE ON creation_drafts BEGIN SELECT RAISE(ABORT,'synthetic attachment update failure'); END`); err != nil {
		t.Fatal(err)
	}
	defer s.DB.Exec(`DROP TRIGGER fail_creation_attachment_update`)
	caller := d
	caller.Body = "forged after failure"
	caller.FollowMode = "topic"
	caller.Attachments = []CreationAttachment{{Kind: "file", ResourceKey: "forged"}}
	if _, err := s.RemoveCreationDraftAttachment(ctx, caller, d.Revision, 1, 200); err == nil {
		t.Fatal("injected update failure was ignored")
	}
	saved, err := s.CreationDraft(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Body != d.Body || saved.Revision != d.Revision || !reflect.DeepEqual(saved.Attachments, d.Attachments) {
		t.Fatalf("failed remove partially committed form or attachments: before=%+v after=%+v", d, saved)
	}
}

func assertCreationAttachmentKeys(t *testing.T, s *Store, draft string, want []string) {
	t.Helper()
	d, err := s.CreationDraft(context.Background(), draft)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(d.Attachments))
	for _, attachment := range d.Attachments {
		got = append(got, attachment.ResourceKey)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("draft attachment keys=%v want=%v", got, want)
	}
}

func assertCreationMaterializedKeys(t *testing.T, s *Store, receipt string, want []string) {
	t.Helper()
	rows, err := s.DB.QueryContext(context.Background(), `SELECT resource_key FROM attachment_parts WHERE batch_id IN (SELECT batch_id FROM attachment_batches WHERE receipt_id=?) AND kind<>'text' ORDER BY ordinal`, receipt)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		got = append(got, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("materialized resource keys=%v want=%v", got, want)
	}
}
