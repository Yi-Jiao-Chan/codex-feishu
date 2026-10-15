package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func attachmentUsageFixture(t *testing.T) (*Store, SubmissionScope, AttachmentBatch) {
	t.Helper()
	s, _, scope, _ := submissionStore(t)
	b, _, err := s.ReceiveAttachmentInput(context.Background(), AttachmentInput{Scope: scope, ChatID: "chat", SenderID: "owner", MessageID: "attachment-message", Parts: []AttachmentPart{
		{Kind: "image", ResourceKey: "private-resource-key", Name: "private-name.png"},
		{Kind: "text", Text: "private-body"},
		{Kind: "file", ResourceKey: "private-file-key", Name: "private-name.pdf"},
	}}, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	return s, scope, b
}

func TestAttachmentCacheReferencesRetainEveryLiveStateBeforeFileIDCommit(t *testing.T) {
	s, scope, b := attachmentUsageFixture(t)
	ctx := context.Background()
	for _, batchState := range []string{"open", "sealed", "failed"} {
		if _, err := s.DB.Exec(`UPDATE attachment_batches SET state=? WHERE batch_id=?`, batchState, b.ID); err != nil {
			t.Fatal(err)
		}
		for _, partState := range []string{"pending", "downloading", "ready", "failed"} {
			if _, err := s.DB.Exec(`UPDATE attachment_parts SET state=?,file_id='' WHERE batch_id=?`, partState, b.ID); err != nil {
				t.Fatal(err)
			}
			refs, err := s.AttachmentCacheReferences(ctx, scope.HostID, scope.AppID)
			if err != nil || len(refs) != 3 {
				t.Fatalf("%s/%s references=%d err=%v", batchState, partState, len(refs), err)
			}
			for _, p := range refs {
				if p.Scope != scope || p.FileID != "" {
					t.Fatalf("lost prospective reference: %#v", p)
				}
			}
		}
	}
	if _, err := s.DB.Exec(`UPDATE attachment_parts SET state='removed' WHERE batch_id=? AND kind='image'`, b.ID); err != nil {
		t.Fatal(err)
	}
	refs, err := s.AttachmentCacheReferences(ctx, scope.HostID, scope.AppID)
	if err != nil || len(refs) != 2 {
		t.Fatalf("removed references=%d err=%v", len(refs), err)
	}
	if _, err := s.DB.Exec(`UPDATE attachment_batches SET state='cleared' WHERE batch_id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	refs, err = s.AttachmentCacheReferences(ctx, scope.HostID, scope.AppID)
	if err != nil || len(refs) != 0 {
		t.Fatalf("cleared references=%d err=%v", len(refs), err)
	}
}

func TestAttachmentUsageCountsNativeUnknownAndKeepsOutputPrivate(t *testing.T) {
	s, scope, b := attachmentUsageFixture(t)
	if _, err := s.DB.Exec(`UPDATE attachment_batches SET state='sealed',receipt_id=(SELECT receipt_id FROM attachment_events WHERE batch_id=?) WHERE batch_id=?`, b.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE submission_inputs SET state='unknown' WHERE receipt_id=(SELECT receipt_id FROM attachment_batches WHERE batch_id=?)`, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE attachment_parts SET state='ready',size=12 WHERE batch_id=? AND kind<>'text'`, b.ID); err != nil {
		t.Fatal(err)
	}
	usage, err := s.AttachmentUsage(context.Background(), scope.HostID, scope.AppID)
	if err != nil || usage.BatchStates["sealed"] != 1 || usage.ResourceStates["ready"] != 2 || usage.SubmissionStates["unknown"] != 1 || usage.RecordedBytes != 24 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	encoded, _ := json.Marshal(usage)
	for _, secret := range []string{"private-resource-key", "private-file-key", "private-name", "private-body", b.ID, scope.ThreadID} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("usage leaked %q", secret)
		}
	}
	refs, err := s.AttachmentCacheReferences(context.Background(), scope.HostID, scope.AppID)
	if err != nil || len(refs) != 3 {
		t.Fatalf("unknown input references=%d err=%v", len(refs), err)
	}
}

func TestAttachmentCacheReferencesFailClosedOnWrongDeploymentOrBinding(t *testing.T) {
	for _, change := range []string{"request-app", "batch-app", "chat", "intent"} {
		t.Run(change, func(t *testing.T) {
			s, scope, b := attachmentUsageFixture(t)
			switch change {
			case "request-app":
				scope.AppID = "other-app"
			case "batch-app":
				if _, err := s.DB.Exec(`UPDATE attachment_batches SET app_id='other-app' WHERE batch_id=?`, b.ID); err != nil {
					t.Fatal(err)
				}
			case "chat":
				if _, err := s.DB.Exec(`UPDATE attachment_batches SET chat_id='other-chat' WHERE batch_id=?`, b.ID); err != nil {
					t.Fatal(err)
				}
			case "intent":
				if _, err := s.DB.Exec(`UPDATE create_intents SET state='unknown'`); err != nil {
					t.Fatal(err)
				}
			}
			if refs, err := s.AttachmentCacheReferences(context.Background(), scope.HostID, scope.AppID); !errors.Is(err, ErrConflict) || refs != nil {
				t.Fatalf("unsafe references=%#v err=%v", refs, err)
			}
		})
	}
}
