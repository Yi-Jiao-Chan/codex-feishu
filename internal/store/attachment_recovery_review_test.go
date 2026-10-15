package store

import (
	"context"
	"testing"
)

func TestAttachmentFailedBatchDownloadDoesNotBlockNextBatchAfterRecovery(t *testing.T) {
	s, _, scope, _ := submissionStore(t)
	ctx := context.Background()
	b := receiveAttachment(t, s, attachmentInput(scope, "alice", "recovery-open", "",
		AttachmentPart{Kind: "image", ResourceKey: "failed-image"},
		AttachmentPart{Kind: "image", ResourceKey: "interrupted-image"},
	))
	p, claimed, err := s.ClaimAttachmentDownload(ctx, "host", "app", 100)
	if err != nil || !claimed {
		t.Fatal("first download was not claimed", claimed, err)
	}
	if err := s.FinishAttachmentDownload(ctx, p, "download-rejected", false, 100); err != nil {
		t.Fatal(err)
	}
	interrupted, claimed, err := s.ClaimAttachmentDownload(ctx, "host", "app", 100)
	if err != nil || !claimed {
		t.Fatal("second download was not claimed", claimed, err)
	}
	b = receiveAttachment(t, s, attachmentInput(scope, "alice", "recovery-instruction", "inspect",
		AttachmentPart{Kind: "text", Text: "inspect"},
	))
	refreshAttachments(t, s)
	b = readAttachmentBatch(t, s, b.ID)
	if b.State != "failed" {
		t.Fatal("sealed batch with a rejected resource did not fail", b.State)
	}
	// The second GET was in flight when the batch failed. Its download state
	// must not survive recovery as an active claim that excludes later batches.
	if err := s.RecoverAttachmentDownloads(ctx, "host", "app"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverExpiredAttachmentDownloads(ctx, "host", "app", 1_000_000); err != nil {
		t.Fatal(err)
	}
	next := receiveAttachment(t, s, attachmentInput(scope, "alice", "recovery-next", "next",
		AttachmentPart{Kind: "image", ResourceKey: "next-image"},
		AttachmentPart{Kind: "text", Text: "next"},
	))
	p, claimed, err = s.ClaimAttachmentDownload(ctx, "host", "app", 1_000_000)
	if err != nil || !claimed || p.BatchID != next.ID {
		t.Fatalf("next batch blocked by abandoned failed-batch part %s: claimed=%v batch=%s err=%v", interrupted.ID, claimed, p.BatchID, err)
	}
}
