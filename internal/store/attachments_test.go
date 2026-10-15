package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func attachmentInput(scope SubmissionScope, user, message, body string, parts ...AttachmentPart) AttachmentInput {
	return AttachmentInput{Scope: scope, SenderID: user, ChatID: "chat", MessageID: message, Body: body, Parts: parts}
}
func receiveAttachment(t *testing.T, s *Store, in AttachmentInput) AttachmentBatch {
	t.Helper()
	b, fresh, err := s.ReceiveAttachmentInput(context.Background(), in, 10, 5)
	if err != nil || !fresh {
		t.Fatal("input not saved", fresh, err)
	}
	return b
}
func deliverAttachmentCard(t *testing.T, s *Store, b AttachmentBatch) {
	t.Helper()
	if _, err := s.DB.Exec(`UPDATE mirrors SET remote_id='attachment-card' WHERE host_id=? AND thread_id=? AND mirror_key=?`, b.Scope.HostID, b.Scope.ThreadID, AttachmentCardKey(b.ID)); err != nil {
		t.Fatal(err)
	}
}
func readAttachmentBatch(t *testing.T, s *Store, id string) AttachmentBatch {
	t.Helper()
	b, err := scanAttachmentBatch(s.DB.QueryRow(`SELECT `+attachmentBatchColumns+` FROM attachment_batches WHERE batch_id=?`, id))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func refreshAttachments(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.RefreshAttachmentBatches(context.Background(), "host", "app", 50<<20, 20<<20, func(p AttachmentPart) (string, error) { return "/private/" + p.ID, nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentDraftBatchIsAtomicOrderedAndIdempotent(t *testing.T) {
	s, _, scope, _ := submissionStore(t)
	ctx := context.Background()
	image := attachmentInput(scope, "alice", "image", "", AttachmentPart{Kind: "image", ResourceKey: "img_fixture"})
	b := receiveAttachment(t, s, image)
	file := attachmentInput(scope, "alice", "file", "", AttachmentPart{Kind: "file", ResourceKey: "file_fixture", Name: "中文报告.csv"})
	if next := receiveAttachment(t, s, file); next.ID != b.ID {
		t.Fatal("resources split across drafts")
	}
	if _, fresh, err := s.ReceiveAttachmentInput(ctx, image, 10, 5); err != nil || fresh {
		t.Fatal("event replay appended resources", err)
	}
	b = receiveAttachment(t, s, attachmentInput(scope, "alice", "instruction", "compare", AttachmentPart{Kind: "text", Text: "compare"}))
	if b.State != "sealed" {
		t.Fatal(b.State)
	}
	n, err := s.Submission(ctx, scope, b.ReceiptID)
	if err != nil || n.State != "collecting" || n.InputJSON != "" {
		t.Fatal("input escaped before resource readiness", n.State, err)
	}
	for i := 0; i < 2; i++ {
		p, claimed, err := s.ClaimAttachmentDownload(ctx, "host", "app", 100)
		if err != nil || !claimed {
			t.Fatal("no resource claim", err)
		}
		p.Size, p.Digest, p.FileID = 12, "fixture-digest", "fixture-file"
		if err := s.FinishAttachmentDownload(ctx, p, "", false, 100); err != nil {
			t.Fatal(err)
		}
		refreshAttachments(t, s)
		got, _ := s.Submission(ctx, scope, b.ReceiptID)
		if i == 0 && (got.State != "collecting" || got.InputJSON != "") {
			t.Fatal("partial batch became dispatchable")
		}
	}
	n, _ = s.Submission(ctx, scope, b.ReceiptID)
	var input []map[string]any
	if n.State != "received" || json.Unmarshal([]byte(n.InputJSON), &input) != nil || len(input) != 3 {
		t.Fatal("multipart input not frozen", n.State, n.InputJSON)
	}
	if input[0]["type"] != "localImage" || !strings.Contains(input[1]["text"].(string), "中文报告.csv") || input[2]["text"] != "compare" {
		t.Fatal("source order or file reference lost")
	}
	frozen := n.InputJSON
	refreshAttachments(t, s)
	n, _ = s.Submission(ctx, scope, b.ReceiptID)
	if n.InputJSON != frozen {
		t.Fatal("frozen input changed")
	}
	if next := receiveAttachment(t, s, attachmentInput(scope, "alice", "next-image", "", AttachmentPart{Kind: "image", ResourceKey: "img_next"})); next.ID == b.ID {
		t.Fatal("late resource entered sealed batch")
	}
}

func TestAttachmentPromptKeepsUserTextAndOnlyRequiredReferences(t *testing.T) {
	for _, kind := range []string{"image", "file"} {
		t.Run(kind, func(t *testing.T) {
			s, _, scope, _ := submissionStore(t)
			ctx := context.Background()
			body := "  inspect this attachment\nkeep my wording  "
			name := "report \"one\"\n中文.csv"
			b := receiveAttachment(t, s, attachmentInput(scope, "alice", "prompt", body,
				AttachmentPart{Kind: "text", Text: body}, AttachmentPart{Kind: kind, ResourceKey: "fixture_key", Name: name}))
			p, claimed, err := s.ClaimAttachmentDownload(ctx, "host", "app", 100)
			if err != nil || !claimed {
				t.Fatal(err)
			}
			p.Size, p.Digest, p.FileID = 5, "digest", "file"
			if err := s.FinishAttachmentDownload(ctx, p, "", false, 100); err != nil {
				t.Fatal(err)
			}
			refreshAttachments(t, s)
			n, err := s.Submission(ctx, scope, b.ReceiptID)
			var input []map[string]any
			if err != nil || json.Unmarshal([]byte(n.InputJSON), &input) != nil || len(input) != 2 || input[0]["type"] != "text" || input[0]["text"] != body {
				t.Fatal("attachment added prompt text or changed the user's message", err)
			}
			path := "/private/" + p.ID
			if kind == "image" {
				if input[1]["type"] != "localImage" || input[1]["path"] != path {
					t.Fatal("image input changed")
				}
			} else {
				quotedName, _ := json.Marshal(name)
				quotedPath, _ := json.Marshal(path)
				if input[1]["type"] != "text" || input[1]["text"] != string(quotedName)+": "+string(quotedPath) {
					t.Fatal("file input contains more than its escaped name and path")
				}
			}
		})
	}
}

func TestAttachmentSenderIsolationAndConcurrentSeal(t *testing.T) {
	s, _, scope, _ := submissionStore(t)
	ctx := context.Background()
	b := receiveAttachment(t, s, attachmentInput(scope, "alice", "alice-image", "", AttachmentPart{Kind: "image", ResourceKey: "img_alice"}))
	other := receiveAttachment(t, s, attachmentInput(scope, "bob", "bob-image", "", AttachmentPart{Kind: "image", ResourceKey: "img_bob"}))
	if b.ID == other.ID {
		t.Fatal("sender drafts mixed")
	}
	deliverAttachmentCard(t, s, b)
	for _, user := range []string{"bob", "stranger"} {
		if _, err := s.AttachmentAction(ctx, scope, user, "chat", "attachment-card", b.Token, b.Revision, "attachment_send", "", ""); !errors.Is(err, ErrSubmissionStale) {
			t.Fatal("foreign draft action accepted", err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.AttachmentAction(ctx, scope, "alice", "chat", "attachment-card", b.Token, b.Revision, "attachment_send", "", "use these")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	success, stale := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrSubmissionStale) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatal("batch sent multiple times", success, stale)
	}
	in := attachmentInput(scope, "alice", "racing-instruction", "go", AttachmentPart{Kind: "text", Text: "go"})
	in.ExpectedBatch = b.ID
	if _, _, err := s.ReceiveAttachmentInput(ctx, in, 10, 5); !errors.Is(err, ErrSubmissionStale) {
		t.Fatal("racing @ created another input", err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM m0_receipts WHERE message_id='racing-instruction'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("stale action committed a receipt", err)
	}
	if draft, err := s.OpenAttachmentBatch(ctx, scope, "bob"); err != nil || draft.ID != other.ID {
		t.Fatal("another sender's draft was consumed", err)
	}
}

func TestAttachmentFailureUnblocksAndRetryMovesToEndWithoutNewIdentity(t *testing.T) {
	s, _, scope, _ := submissionStore(t)
	ctx := context.Background()
	b := receiveAttachment(t, s, attachmentInput(scope, "alice", "image-and-text", "inspect", AttachmentPart{Kind: "text", Text: "inspect"}, AttachmentPart{Kind: "image", ResourceKey: "img_bad"}))
	p, _, err := s.ClaimAttachmentDownload(ctx, "host", "app", 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAttachmentDownload(ctx, p, "download-rejected", false, 100); err != nil {
		t.Fatal(err)
	}
	refreshAttachments(t, s)
	b = readAttachmentBatch(t, s, b.ID)
	n, _ := s.Submission(ctx, scope, b.ReceiptID)
	client := n.ClientID
	if b.State != "failed" || n.State != "failed" || n.InputJSON != "" {
		t.Fatal("failed batch is dispatchable", b.State, n.State)
	}
	receiveAttachment(t, s, attachmentInput(scope, "bob", "later-text", "later", AttachmentPart{Kind: "text", Text: "later"}))
	deliverAttachmentCard(t, s, b)
	if _, err := s.AttachmentAction(ctx, scope, "alice", "chat", "attachment-card", b.Token, b.Revision, "attachment_retry", "", ""); err != nil {
		t.Fatal(err)
	}
	inputs, err := s.Submissions(ctx, scope)
	if err != nil || inputs[len(inputs)-1].ID != b.ReceiptID {
		t.Fatal("retry retained its original queue order", err)
	}
	n, _ = s.Submission(ctx, scope, b.ReceiptID)
	if n.ClientID != client || n.State != "collecting" {
		t.Fatal("retry changed association", n.State)
	}
}

func TestAttachmentReadyProofMustCommitBeforeReceivedState(t *testing.T) {
	s, _, scope, _ := submissionStore(t)
	ctx := context.Background()
	b := receiveAttachment(t, s, attachmentInput(scope, "alice", "proof-image", "inspect", AttachmentPart{Kind: "image", ResourceKey: "img_proof"}))
	p, _, _ := s.ClaimAttachmentDownload(ctx, "host", "app", 100)
	p.Size, p.Digest, p.FileID = 5, "digest", "file"
	if err := s.FinishAttachmentDownload(ctx, p, "", false, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER ignore_input BEFORE UPDATE OF input_json ON m0_receipts BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshAttachmentBatches(ctx, "host", "app", 50<<20, 20<<20, func(AttachmentPart) (string, error) { return "/private/image", nil }); !errors.Is(err, ErrConflict) {
		t.Fatal("ignored frozen input accepted", err)
	}
	n, _ := s.Submission(ctx, scope, b.ReceiptID)
	if n.State != "collecting" || n.InputJSON != "" {
		t.Fatal("missing proof became legacy text input")
	}
}

func TestAttachmentCountLimitAndForeignScopeRollback(t *testing.T) {
	s, _, scope, _ := submissionStore(t)
	ctx := context.Background()
	in := attachmentInput(scope, "alice", "too-many", "", AttachmentPart{Kind: "image", ResourceKey: "img_one"}, AttachmentPart{Kind: "image", ResourceKey: "img_two"})
	if _, _, err := s.ReceiveAttachmentInput(ctx, in, 1, 1); !errors.Is(err, ErrAttachmentLimit) {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM attachment_batches`).Scan(&count); err != nil || count != 0 {
		t.Fatal("over-limit draft partially committed", err)
	}
	in.Scope.AppID = "other-app"
	if _, _, err := s.ReceiveAttachmentInput(ctx, in, 10, 5); !errors.Is(err, ErrConflict) {
		t.Fatal("foreign deployment accepted", err)
	}
}

func TestAttachmentIngressOverlappingCardSealCannotStartSecondRound(t *testing.T) {
	s, _, scope, _ := submissionStore(t)
	ctx := context.Background()
	b := receiveAttachment(t, s, attachmentInput(scope, "alice", "first-image", "", AttachmentPart{Kind: "image", ResourceKey: "img_first"}))
	deliverAttachmentCard(t, s, b)
	started := time.Now().UnixNano()
	if _, err := s.AttachmentAction(ctx, scope, "alice", "chat", "attachment-card", b.Token, b.Revision, "attachment_send", "", ""); err != nil {
		t.Fatal(err)
	}
	in := attachmentInput(scope, "alice", "overlap-text", "inspect", AttachmentPart{Kind: "text", Text: "inspect"})
	in.IngressStartedNS = started
	if _, _, err := s.ReceiveAttachmentInput(ctx, in, 10, 5); !errors.Is(err, ErrSubmissionStale) {
		t.Fatal("overlapping instruction started a second round", err)
	}
	if _, err := s.CommandReply(ctx, "host", "app", "overlap-text"); err != nil {
		t.Fatal("loser has no durable feedback", err)
	}
	// A resource-only arrival after seal belongs to the next draft, even when
	// its handler began before the card action completed.
	next := attachmentInput(scope, "alice", "late-image", "", AttachmentPart{Kind: "image", ResourceKey: "img_late"})
	next.IngressStartedNS = started
	if draft := receiveAttachment(t, s, next); draft.ID == b.ID {
		t.Fatal("late resource reused sealed batch")
	}
}

func TestAttachmentsMigrationFrom25PreservesUnknownReceiptAndIdentity(t *testing.T) {
	s, path, scope, r := submissionStore(t)
	ctx := context.Background()
	a, claimed, err := s.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add")
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err := s.FinishSubmissionAttempt(ctx, scope, a.ID, "unknown", "", -1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DROP TABLE attachment_parts; DROP TABLE attachment_events; DROP TABLE attachment_batches; DROP TABLE submission_order; ALTER TABLE m0_receipts DROP COLUMN input_json; PRAGMA user_version=25;`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	n, err := reopened.Submission(ctx, scope, r.ID)
	if err != nil || n.State != "unknown" || n.ClientID != r.ClientID || n.MessageID != r.MessageID || n.Body != r.Body || n.InputJSON != "" {
		t.Fatal("migration rewrote old input", err)
	}
	if _, claimed, err := reopened.ClaimSubmissionAttempt(ctx, scope, r.ID, "", "add"); err != nil || claimed {
		t.Fatal("migration made unknown add replayable", err)
	}
}
