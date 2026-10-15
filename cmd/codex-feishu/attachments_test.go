package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
)

func attachmentManagementFixture(t *testing.T, initialize bool) (string, config.Config, *store.Store, store.AttachmentPart) {
	t.Helper()
	path, cfg, _, _ := doctorFixture(t)
	cfg.Feishu.AppID = "cli_attachment_" + strings.ReplaceAll(t.Name(), "/", "_")
	cfg.Attachments.Enabled = true
	writeDoctorFixtureConfig(t, path, cfg)
	if !initialize {
		return path, cfg, nil, store.AttachmentPart{}
	}
	if err := os.Chmod(filepath.Dir(cfg.Storage.DatabaseFile), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(cfg.Storage.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := db.BeginBaseline(ctx, cfg.Host.ID, cfg.Feishu.AppID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO thread_observations(host_id,thread_id,origin,state) VALUES(?,'private-thread','new','eligible')`, cfg.Host.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO create_intents(intent_id,host_id,thread_id,owner_id,name,members_json,state,chat_id,created_at) VALUES('intent',?,'private-thread','owner','name','[]','confirmed','chat',1)`, cfg.Host.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO bindings(host_id,thread_id,intent_id,chat_id) VALUES(?,'private-thread','intent','chat')`, cfg.Host.ID); err != nil {
		t.Fatal(err)
	}
	scope := store.SubmissionScope{HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, ThreadID: "private-thread"}
	b, _, err := db.ReceiveAttachmentInput(ctx, store.AttachmentInput{Scope: scope, ChatID: "chat", SenderID: "owner", MessageID: "private-message", Parts: []store.AttachmentPart{{Kind: "image", ResourceKey: "private-resource-key", Name: "private-name.png"}, {Kind: "text", Text: "private-input-body"}}}, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := db.AttachmentCacheReferences(ctx, cfg.Host.ID, cfg.Feishu.AppID)
	if err != nil || len(refs) != 2 {
		t.Fatalf("fixture refs=%#v err=%v", refs, err)
	}
	for _, p := range refs {
		if p.Kind == "image" {
			p.BatchID = b.ID
			return path, cfg, db, p
		}
	}
	t.Fatal("fixture resource absent")
	return "", cfg, nil, store.AttachmentPart{}
}

func attachmentManagementFile(t *testing.T, cfg config.Config, p store.AttachmentPart, data string) string {
	t.Helper()
	key := sha256.Sum256([]byte(p.Scope.HostID + "\x00" + p.Scope.AppID + "\x00" + p.Scope.ThreadID))
	path := filepath.Join(cfg.AttachmentLimits().Directory, hex.EncodeToString(key[:16]), p.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAttachmentsStatusIsReadOnlyWithMissingCacheAndDatabase(t *testing.T) {
	path, cfg, _, _ := attachmentManagementFixture(t, false)
	before, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	code, o := candidateCommandOutput(t, []string{"attachments", "status", "--config", path, "--json"})
	if code != 0 || !o.OK || o.Command != "attachments status" || o.State != "attachment-storage-status" || o.Checks == nil || o.NextSteps == nil {
		t.Fatalf("status=%#v code=%d", o, code)
	}
	data := o.Data.(map[string]any)
	if data["cache_bytes"] != float64(0) || data["storage_initialized"] != false || data["disk_quota_bytes"] != float64(cfg.AttachmentLimits().DiskQuotaBytes) {
		t.Fatalf("missing status=%#v", data)
	}
	for _, p := range []string{cfg.Storage.DatabaseFile, cfg.AttachmentLimits().Directory} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("status created path: %v", err)
		}
	}
	after, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(before) != len(after) {
		t.Fatal("status wrote deployment files")
	}
}

func TestAttachmentsStatusReportsOnlyAggregatePrivateData(t *testing.T) {
	path, cfg, db, p := attachmentManagementFixture(t, true)
	attachmentManagementFile(t, cfg, p, "private-file-bytes")
	if _, err := db.DB.Exec(`UPDATE attachment_batches SET state='sealed',receipt_id=(SELECT receipt_id FROM attachment_events WHERE batch_id=?) WHERE batch_id=?`, p.BatchID, p.BatchID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`UPDATE submission_inputs SET state='unknown' WHERE receipt_id=(SELECT receipt_id FROM attachment_batches WHERE batch_id=?)`, p.BatchID); err != nil {
		t.Fatal(err)
	}
	o, code := inspectAttachments(context.Background(), path, "status")
	if code != 0 || !o.OK {
		t.Fatalf("status=%#v code=%d", o, code)
	}
	data := o.Data.(map[string]any)
	usage := data["usage"].(store.AttachmentUsage)
	if data["cache_bytes"] != int64(len("private-file-bytes")) || usage.ResourceStates["pending"] != 1 || usage.BatchStates["sealed"] != 1 || usage.SubmissionStates["unknown"] != 1 {
		t.Fatalf("aggregates=%#v", data)
	}
	encoded, _ := json.Marshal(o)
	for _, private := range []string{"private-resource-key", "private-name.png", "private-input-body", "private-file-bytes", "private-thread", "private-message", p.ID, p.BatchID, "doctor-secret-never-output"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("status leaked %q", private)
		}
	}
}

func TestAttachmentsCleanupRequiresBothDeploymentLocks(t *testing.T) {
	for _, kind := range []string{"application", "listener"} {
		t.Run(kind, func(t *testing.T) {
			path, cfg, _, p := attachmentManagementFixture(t, true)
			orphan := p
			orphan.ID = strings.Repeat("f", 32)
			file := attachmentManagementFile(t, cfg, orphan, "orphan")
			var lock *os.File
			var err error
			if kind == "application" {
				lock, err = store.AcquireApplication(cfg.Feishu.AppID)
			} else {
				lock, err = store.AcquireListener(cfg.Storage.DatabaseFile)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			o, code := inspectAttachments(context.Background(), path, "cleanup")
			if code != 6 || o.OK || o.State != "listener-lock-conflict" {
				t.Fatalf("cleanup=%#v code=%d", o, code)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal("lock conflict deleted orphan")
			}
		})
	}
}

func TestAttachmentsCleanupKeepsUnknownNativeInputAndAllowsRemovedAndClearedParts(t *testing.T) {
	path, cfg, db, p := attachmentManagementFixture(t, true)
	retained := attachmentManagementFile(t, cfg, p, "keep") // publication before ready/file_id commit
	if _, err := db.DB.Exec(`UPDATE attachment_batches SET state='sealed',receipt_id=(SELECT receipt_id FROM attachment_events WHERE batch_id=?) WHERE batch_id=?`, p.BatchID, p.BatchID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`UPDATE submission_inputs SET state='unknown' WHERE receipt_id=(SELECT receipt_id FROM attachment_batches WHERE batch_id=?)`, p.BatchID); err != nil {
		t.Fatal(err)
	}
	removed := p
	removed.ID = strings.Repeat("a", 32)
	if _, err := db.DB.Exec(`INSERT INTO attachment_parts(part_id,batch_id,ordinal,kind,message_id,resource_key,state) VALUES(?,?,2,'image','removed-message','removed-key','removed')`, removed.ID, p.BatchID); err != nil {
		t.Fatal(err)
	}
	removedFile := attachmentManagementFile(t, cfg, removed, "removed")
	cleared := p
	cleared.ID = strings.Repeat("b", 32)
	clearedBatch := strings.Repeat("c", 32)
	if _, err := db.DB.Exec(`INSERT INTO attachment_batches(batch_id,host_id,app_id,thread_id,chat_id,sender_id,token,state,first_message_id,created_at_ms) VALUES(?,?,?,'private-thread','chat','owner',?,'cleared','cleared-message',1)`, clearedBatch, cfg.Host.ID, cfg.Feishu.AppID, clearedBatch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO attachment_parts(part_id,batch_id,ordinal,kind,message_id,resource_key,state) VALUES(?,?,0,'file','cleared-message','cleared-key','ready')`, cleared.ID, clearedBatch); err != nil {
		t.Fatal(err)
	}
	clearedFile := attachmentManagementFile(t, cfg, cleared, "cleared")
	partial := filepath.Join(filepath.Dir(retained), ".partial-crashed")
	if err := os.WriteFile(partial, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(cfg.AttachmentLimits().Directory, "keep.txt")
	if err := os.WriteFile(unknown, []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	code, o := candidateCommandOutput(t, []string{"attachments", "cleanup", "--config", path, "--json"})
	if code != 0 || !o.OK || o.State != "attachment-cache-cleaned" {
		t.Fatalf("cleanup=%#v code=%d", o, code)
	}
	data := o.Data.(map[string]any)
	if data["removed_files"] != float64(3) || data["removed_bytes"] != float64(21) {
		t.Fatalf("cleanup counts=%#v", data)
	}
	for _, path := range []string{retained, unknown} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("deleted referenced/unknown file: %v", err)
		}
	}
	for _, path := range []string{removedFile, clearedFile, partial} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleanup retained orphan: %v", err)
		}
	}
	var state string
	if err := db.DB.QueryRow(`SELECT state FROM submission_inputs WHERE receipt_id=(SELECT receipt_id FROM attachment_batches WHERE batch_id=?)`, p.BatchID).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("cleanup changed native journal: state=%q err=%v", state, err)
	}
}

func TestAttachmentsCommandsNeverMigrateExistingSchema(t *testing.T) {
	path, cfg, _, _ := attachmentManagementFixture(t, false)
	createLegacyVerificationDB(t, cfg.Storage.DatabaseFile)
	for _, action := range []string{"status", "cleanup"} {
		o, code := inspectAttachments(context.Background(), path, action)
		if code != 3 || o.OK || o.State != "storage-unavailable" {
			t.Fatalf("legacy %s=%#v code=%d", action, o, code)
		}
		db, schema, err := store.OpenReadOnly(context.Background(), cfg.Storage.DatabaseFile)
		if err != nil || schema != 1 {
			t.Fatalf("schema changed: %d %v", schema, err)
		}
		db.Close()
	}
}
