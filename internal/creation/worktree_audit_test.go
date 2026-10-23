package creation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/store"
)

func worktreeAuditFixture(t *testing.T) (*managerHarness, WorktreeAuditor, WorktreeAuditReport, string) {
	t.Helper()
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "audit-synthetic-request")
	claimWorktreeTestIntent(t, h, d, nil)
	h.tick()
	i := h.intent(d)
	if i.State != "confirmed" {
		t.Fatalf("fixture did not complete: state=%s code=%s", i.State, i.ErrorCode)
	}
	owner := worktreeTestGit(t, i.WorktreePath, "rev-parse", "--path-format=absolute", "--git-path", worktreeOwnerName)
	if err := os.Remove(owner); err != nil {
		t.Fatal(err)
	}
	a := WorktreeAuditor{Store: h.s, Native: h.n, Config: h.m.Config, Now: func() time.Time { return h.clock }}
	plan, err := a.Audit(context.Background())
	if err != nil || len(plan.Entries) != 1 || plan.Entries[0].Status != "owner-missing" {
		t.Fatalf("fixture audit: entries=%+v err=%v", plan.Entries, err)
	}
	return h, a, plan, owner
}

func assertAuditWorkflowConfirmed(t *testing.T, h *managerHarness, entry WorktreeAuditEntry) {
	t.Helper()
	i, err := h.s.BridgeCreationThread(context.Background(), "host", "app", entry.ThreadID)
	if err != nil || i.State != "confirmed" || i.ErrorCode != "" {
		t.Fatalf("audit repair changed the production workflow: state=%s code=%s err=%v", i.State, i.ErrorCode, err)
	}
}

func auditRepairAttempt(t *testing.T, h *managerHarness, entry WorktreeAuditEntry) store.CreationAttempt {
	t.Helper()
	a, err := h.s.CreationAttempt(context.Background(), entry.DraftID, "worktree-owner-repair:"+entry.Identity)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestWorktreeAuditReadOnlyScopeAndReportPrivacy(t *testing.T) {
	h, a, plan, owner := worktreeAuditFixture(t)
	ctx := context.Background()
	var before int
	if err := h.s.DB.QueryRow(`SELECT COUNT(*) FROM creation_attempts`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	ro, schema, err := store.OpenReadOnly(ctx, h.db)
	if err != nil || schema != store.SchemaVersion {
		t.Fatal(err)
	}
	defer ro.Close()
	a.Store = ro
	got, err := a.Audit(ctx)
	if err != nil || len(got.Entries) != 1 || got.Entries[0].Identity != plan.Entries[0].Identity {
		t.Fatal("read-only audit changed identity", err)
	}
	if _, err := ro.DB.Exec(`UPDATE creation_workflows SET state='working'`); err == nil {
		t.Fatal("audit opened a writable database")
	}
	var after int
	if err := h.s.DB.QueryRow(`SELECT COUNT(*) FROM creation_attempts`).Scan(&after); err != nil || after != before {
		t.Fatal("audit wrote journal records", err)
	}
	if _, err := os.Stat(owner); !os.IsNotExist(err) {
		t.Fatal("audit created owner metadata", err)
	}
	for _, field := range []string{"host", "app"} {
		copy := a
		if field == "host" {
			copy.Config.Host.ID = "different-host"
		} else {
			copy.Config.Feishu.AppID = "different-app"
		}
		other, err := copy.Audit(ctx)
		if err != nil || len(other.Entries) != 0 {
			t.Fatal("audit crossed deployment scope", field, err)
		}
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "updated task") || strings.Contains(string(raw), "task.txt") || !strings.Contains(string(raw), WorktreeAuditWarning) {
		t.Fatal("report leaked task content or omitted cleanup limitation")
	}
	assertAuditWorkflowConfirmed(t, h, plan.Entries[0])
}

func TestWorktreeAuditNativeIdentityMismatchRefusesRepair(t *testing.T) {
	for _, change := range []string{"cwd", "source", "ephemeral", "parent", "fork"} {
		t.Run(change, func(t *testing.T) {
			h, a, plan, owner := worktreeAuditFixture(t)
			e := plan.Entries[0]
			thread := h.n.threads[e.ThreadID]
			foreign := "synthetic-other-thread"
			switch change {
			case "cwd":
				thread.CWD = h.root
			case "source":
				thread.ThreadSource = json.RawMessage(`"user"`)
			case "ephemeral":
				thread.Ephemeral = true
			case "parent":
				thread.ParentThreadID = &foreign
			case "fork":
				thread.ForkedFromID = &foreign
			}
			h.n.threads[e.ThreadID] = thread
			got, err := a.Audit(context.Background())
			if err != nil || got.Entries[0].Status != "native-unverified" {
				t.Fatal("native mismatch accepted", err)
			}
			counts, err := a.Repair(context.Background(), plan)
			if err != nil || counts["refused"] != 1 {
				t.Fatal("repair accepted native drift", counts, err)
			}
			if _, err := os.Stat(owner); !os.IsNotExist(err) {
				t.Fatal("repair wrote after native mismatch", err)
			}
		})
	}
}

func TestWorktreeAuditRepairPersistsAndIsIdempotent(t *testing.T) {
	h, a, plan, owner := worktreeAuditFixture(t)
	ctx := context.Background()
	counts, err := a.Repair(ctx, plan)
	if err != nil || counts["confirmed"] != 1 {
		t.Fatal("repair failed", counts, err)
	}
	before, err := os.Stat(owner)
	if err != nil || before.Mode().Perm() != 0600 {
		t.Fatal("owner was not privately created", err)
	}
	attempt := auditRepairAttempt(t, h, plan.Entries[0])
	if attempt.State != "confirmed" || attempt.RemoteID != plan.Entries[0].ThreadID {
		t.Fatal("repair journal did not confirm exact owner")
	}
	counts, err = a.Repair(ctx, plan)
	if err != nil || counts["confirmed"] != 1 {
		t.Fatal("idempotent repair failed", counts, err)
	}
	after, err := os.Stat(owner)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("replay rewrote owner", err)
	}
	assertAuditWorkflowConfirmed(t, h, plan.Entries[0])
}

func TestWorktreeAuditRejectsReplacedCheckoutAndMissingDirectory(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			h, a, plan, _ := worktreeAuditFixture(t)
			i, err := h.s.BridgeCreationThread(context.Background(), "host", "app", plan.Entries[0].ThreadID)
			if err != nil {
				t.Fatal(err)
			}
			worktreeTestGit(t, h.root, "worktree", "remove", "--force", i.WorktreePath)
			if replace {
				worktreeTestGit(t, h.root, "worktree", "add", "--detach", i.WorktreePath, i.BaseCommit)
			}
			got, err := a.Audit(context.Background())
			want := "missing-directory"
			if replace {
				want = "identity-unverified"
			}
			if err != nil || got.Entries[0].Status != want {
				t.Fatal("replacement/missing checkout accepted", got.Entries, err)
			}
			counts, err := a.Repair(context.Background(), plan)
			if err != nil || counts["refused"] != 1 {
				t.Fatal("repair adopted a different physical generation", counts, err)
			}
			if !replace {
				if _, err := os.Stat(i.WorktreePath); !os.IsNotExist(err) {
					t.Fatal("repair recreated missing worktree", err)
				}
			}
		})
	}
}

func TestWorktreeAuditConflictAndMalformedOwnerAreNeverOverwritten(t *testing.T) {
	for _, raw := range []string{`{"version":1,"ownerThreadId":"another-chat"}`, `{"version":1,`} {
		t.Run(fmt.Sprint(len(raw)), func(t *testing.T) {
			_, a, plan, owner := worktreeAuditFixture(t)
			if err := os.WriteFile(owner, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := a.Audit(context.Background())
			want := "owner-conflict"
			if !json.Valid([]byte(raw)) {
				want = "identity-unverified"
			}
			if err != nil || got.Entries[0].Status != want {
				t.Fatal("unsafe owner not reported", got.Entries, err)
			}
			if counts, err := a.Repair(context.Background(), got); err != nil || counts["refused"] != 1 {
				t.Fatal("repair reported an unsafe audit entry as successfully repaired", counts, err)
			}
			counts, err := a.Repair(context.Background(), plan)
			if err != nil || counts["refused"] != 1 {
				t.Fatal("unsafe owner not refused", counts, err)
			}
			after, err := os.ReadFile(owner)
			if err != nil || string(after) != raw {
				t.Fatal("conflicting/malformed owner overwritten", err)
			}
		})
	}
}

func TestWorktreeAuditRepairRequiresDurableJournalAndOnlyReadsUnknown(t *testing.T) {
	t.Run("prepare-failure", func(t *testing.T) {
		h, a, plan, owner := worktreeAuditFixture(t)
		_, err := h.s.DB.Exec(`CREATE TRIGGER fail_audit_prepare BEFORE INSERT ON creation_attempts WHEN NEW.step LIKE 'worktree-owner-repair:%' BEGIN SELECT RAISE(FAIL,'synthetic journal failure'); END`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Repair(context.Background(), plan); err == nil {
			t.Fatal("repair ignored journal failure")
		}
		if _, err := os.Stat(owner); !os.IsNotExist(err) {
			t.Fatal("owner published before journal", err)
		}
		assertAuditWorkflowConfirmed(t, h, plan.Entries[0])
	})
	t.Run("unknown-missing", func(t *testing.T) {
		h, a, plan, owner := worktreeAuditFixture(t)
		_, err := h.s.DB.Exec(`CREATE TRIGGER fail_audit_dispatch BEFORE UPDATE OF state ON creation_attempts WHEN NEW.step LIKE 'worktree-owner-repair:%' AND NEW.state='dispatching' BEGIN SELECT RAISE(FAIL,'synthetic dispatch failure'); END`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Repair(context.Background(), plan); err == nil {
			t.Fatal("repair ignored dispatch failure")
		}
		if _, err := h.s.DB.Exec(`DROP TRIGGER fail_audit_dispatch`); err != nil {
			t.Fatal(err)
		}
		attempt := auditRepairAttempt(t, h, plan.Entries[0])
		if ok, err := h.s.DispatchCreationAttempt(context.Background(), attempt); err != nil || !ok {
			t.Fatal(err)
		}
		unrelated, err := h.s.PrepareCreationAttempt(context.Background(), plan.Entries[0].DraftID, "unrelated-readback", `{}`, h.clock.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := h.s.DispatchCreationAttempt(context.Background(), unrelated); err != nil || !ok {
			t.Fatal(err)
		}
		for n := 0; n < 2; n++ {
			counts, err := a.Repair(context.Background(), plan)
			if err != nil || counts["unknown"] != 1 {
				t.Fatal("unknown repair did not remain pending", counts, err)
			}
			if _, err := os.Stat(owner); !os.IsNotExist(err) {
				t.Fatal("unknown repair blindly retried owner publication", err)
			}
		}
		other, err := h.s.CreationAttempt(context.Background(), unrelated.DraftID, unrelated.Step)
		if err != nil || other.State != "dispatching" || auditRepairAttempt(t, h, plan.Entries[0]).State != "unknown" {
			t.Fatal("repair recovered another step or failed to retain its own unknown", err)
		}
		assertAuditWorkflowConfirmed(t, h, plan.Entries[0])
	})
	t.Run("confirmation-failure-readback", func(t *testing.T) {
		h, a, plan, owner := worktreeAuditFixture(t)
		_, err := h.s.DB.Exec(`CREATE TRIGGER fail_audit_confirm BEFORE UPDATE OF state ON creation_attempts WHEN NEW.step LIKE 'worktree-owner-repair:%' AND NEW.state='confirmed' BEGIN SELECT RAISE(FAIL,'synthetic confirmation failure'); END`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Repair(context.Background(), plan); err == nil {
			t.Fatal("repair ignored confirmation failure")
		}
		before, err := os.Stat(owner)
		if err != nil || auditRepairAttempt(t, h, plan.Entries[0]).State != "dispatching" {
			t.Fatal("lost confirmation did not retain dispatch journal", err)
		}
		if _, err := h.s.DB.Exec(`DROP TRIGGER fail_audit_confirm`); err != nil {
			t.Fatal(err)
		}
		counts, err := a.Repair(context.Background(), plan)
		if err != nil || counts["confirmed"] != 1 {
			t.Fatal("lost confirmation was not reconciled", counts, err)
		}
		after, err := os.Stat(owner)
		if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
			t.Fatal("readback recreated owner metadata", err)
		}
		assertAuditWorkflowConfirmed(t, h, plan.Entries[0])
	})
}

func TestWorktreeAuditReportPermissionsAndNoOverwrite(t *testing.T) {
	_, a, report, _ := worktreeAuditFixture(t)
	path := filepath.Join(t.TempDir(), "private.json")
	if err := WriteWorktreeAuditReport(path, report); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("report is not private", err)
	}
	got, err := ReadWorktreeAuditReport(path)
	if err != nil || got.Entries[0] != report.Entries[0] {
		t.Fatal("private report did not round trip", err)
	}
	if err := WriteWorktreeAuditReport(path, report); err == nil {
		t.Fatal("report was overwritten")
	}
	link := filepath.Join(filepath.Dir(path), "alias.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteWorktreeAuditReport(link, report); err == nil {
		t.Fatal("report symlink accepted for writing")
	}
	if _, err := ReadWorktreeAuditReport(link); err == nil {
		t.Fatal("report symlink accepted for reading")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWorktreeAuditReport(path); err == nil {
		t.Fatal("public report accepted")
	}
	copy := report
	copy.HostID = "other-host"
	if _, err := a.Repair(context.Background(), copy); err == nil {
		t.Fatal("report crossed deployment scope")
	}
}
