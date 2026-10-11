package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
	"github.com/pelletier/go-toml/v2"
)

func policyApplyFixture(t *testing.T, policyOnly bool) (Planner, Input, Paths, *store.Store, rules.Policy) {
	t.Helper()
	ctx := context.Background()
	p, in, paths := fixture(t)
	in.Identity.NotificationOpenIDs = []string{*in.Identity.OwnerOpenID, "ou_setup_other"}
	if policyOnly {
		quiet := "23:00-08:00"
		in.Notifications.QuietHours = &quiet
	}
	plan, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(ctx, in, paths, plan.ID); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.BeginBaseline(ctx, plan.Config.Host.ID, in.Feishu.AppID, time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	policy, _ := rules.DefaultPolicy("Asia/Shanghai")
	policy.Enabled = false
	policy.SnoozeUntil = time.Date(2035, 1, 2, 3, 4, 5, 123456789, time.UTC)
	policy, err = db.InitNotificationPolicyAt(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID, policy, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InitNotificationPolicyAt(ctx, in.Feishu.AppID, "ou_setup_other", policy, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	return p, in, paths, db, policy
}

func policyHistoryCount(t *testing.T, db *store.Store, app, user string) int {
	t.Helper()
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM notification_policy_history WHERE app_id=? AND open_id=?`, app, user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestPolicyOnlyApplyChangesPolicyWithoutReplacingConfig(t *testing.T) {
	ctx := context.Background()
	p, in, paths, db, before := policyApplyFixture(t, true)
	raw, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.Build(ctx, in, paths)
	if err != nil || !plan.Changed || !plan.CanApply || len(plan.PolicyChanges) != 1 || string(plan.content) != string(raw) {
		t.Fatal("policy-only difference disappeared", err)
	}
	result, err := p.Apply(ctx, in, paths, plan.ID)
	if err != nil || !result.Changed || result.BackupFile != "" {
		t.Fatal(result, err)
	}
	afterInfo, _ := os.Stat(paths.ConfigFile)
	if !os.SameFile(info, afterInfo) {
		t.Fatal("policy-only apply replaced config inode")
	}
	current, err := db.NotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID)
	if err != nil || current.Enabled || !current.SnoozeUntil.Equal(before.SnoozeUntil) || current.QuietStart != "23:00" || current.QuietEnd != "08:00" || current.Revision != 1 {
		t.Fatal(current, err)
	}
	other, _ := db.NotificationPolicy(ctx, in.Feishu.AppID, "ou_setup_other")
	if !samePolicy(other, before) {
		t.Fatal("another recipient changed")
	}
	result, err = p.Apply(ctx, in, paths, plan.ID)
	if err != nil || result.Changed || result.State != "configuration-unchanged" || policyHistoryCount(t, db, in.Feishu.AppID, *in.Identity.OwnerOpenID) != 2 {
		t.Fatal("policy-only replay duplicated audit", result, err)
	}
}

func TestPolicyApplyRecoversEachDurableBoundaryWithoutResettingSnooze(t *testing.T) {
	for _, stage := range []string{"prepared", "configuration", "policies"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			p, in, paths, db, before := policyApplyFixture(t, false)
			quiet := "23:00-08:00"
			in.Notifications.QuietHours = &quiet
			in.Notifications.Timezone = "Asia/Tokyo"
			configBefore, _ := os.ReadFile(paths.ConfigFile)
			plan, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			p.checkpoint = func(at string) error {
				if at == stage {
					return errors.New("simulated crash")
				}
				return nil
			}
			if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrApplyPending) {
				t.Fatal("interruption claimed success", err)
			}
			if err := CheckApplyReady(paths.ConfigFile); !errors.Is(err, ErrApplyPending) {
				t.Fatal("unfinished state could start", err)
			}
			current, err := db.NotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "policies" {
				if current.Revision != 1 {
					t.Fatal("committed policy lost")
				}
			} else if !samePolicy(current, before) {
				t.Fatal("policy changed before commit")
			}
			journal, found, err := readApplyJournal(paths.ConfigFile)
			if err != nil || !found || journal.State != "prepared" || journal.BackupFile == "" {
				t.Fatal("recovery record lost", err)
			}
			backup, err := os.ReadFile(journal.BackupFile)
			if err != nil || string(backup) != string(configBefore) {
				t.Fatal("backup lost", err)
			}
			p.checkpoint = nil
			result, err := p.Apply(ctx, in, paths, plan.ID)
			if err != nil || stage == "policies" && result.Changed {
				t.Fatal("recovery reapplied completed transaction", result, err)
			}
			if err := CheckApplyReady(paths.ConfigFile); err != nil {
				t.Fatal("recovery remained blocked", err)
			}
			cfg, err := config.Load(paths.ConfigFile)
			if err != nil || cfg.Notifications.QuietHours != quiet || cfg.Notifications.Timezone != "Asia/Tokyo" {
				t.Fatal("configuration not recovered", err)
			}
			current, err = db.NotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID)
			if err != nil || current.Enabled || !current.SnoozeUntil.Equal(before.SnoozeUntil) || current.Timezone != "Asia/Tokyo" || current.QuietStart != "23:00" || current.Revision != 1 {
				t.Fatal("policy not recovered", current, err)
			}
			if policyHistoryCount(t, db, in.Feishu.AppID, *in.Identity.OwnerOpenID) != 2 {
				t.Fatal("recovery duplicated history")
			}
			if repeated, err := p.Apply(ctx, in, paths, plan.ID); err != nil || repeated.Changed {
				t.Fatal("repeat not idempotent", err)
			}
			fresh, err := p.Build(ctx, in, paths)
			if err != nil || fresh.Changed {
				t.Fatal("fresh plan not no-op", err)
			}
		})
	}
}

func TestPolicyApplyFailureRollsBackDBAndRequiresSamePlanRecovery(t *testing.T) {
	ctx := context.Background()
	p, in, paths, db, before := policyApplyFixture(t, false)
	quiet := "23:00-08:00"
	in.Notifications.QuietHours = &quiet
	plan, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`CREATE TRIGGER reject_setup BEFORE UPDATE ON notification_rules BEGIN SELECT RAISE(ABORT,'injected policy write failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrApplyPending) {
		t.Fatal("database failure not pending", err)
	}
	current, _ := db.NotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID)
	if !samePolicy(current, before) || policyHistoryCount(t, db, in.Feishu.AppID, *in.Identity.OwnerOpenID) != 1 {
		t.Fatal("failed transaction partially committed")
	}
	if err := CheckApplyReady(paths.ConfigFile); !errors.Is(err, ErrApplyPending) {
		t.Fatal("split state allowed startup", err)
	}
	newPlan, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if newPlan.ID == plan.ID {
		t.Fatal("test did not replace configuration")
	}
	if _, err := p.Apply(ctx, in, paths, newPlan.ID); !errors.Is(err, ErrApplyPending) {
		t.Fatal("new plan superseded pending transaction", err)
	}
	if _, err := db.DB.Exec(`DROP TRIGGER reject_setup`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(ctx, in, paths, plan.ID); err != nil {
		t.Fatal("original plan could not resume", err)
	}
	if policyHistoryCount(t, db, in.Feishu.AppID, *in.Identity.OwnerOpenID) != 2 {
		t.Fatal("recovery audit wrong")
	}
}

func TestPolicyPreviewRevisionConflictDoesNotReplaceConfig(t *testing.T) {
	ctx := context.Background()
	p, in, paths, db, before := policyApplyFixture(t, false)
	quiet := "23:00-08:00"
	in.Notifications.QuietHours = &quiet
	plan, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(paths.ConfigFile)
	next := before
	next.Revision++
	next.Enabled = true
	if err := db.SetNotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID, before.Revision, next); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrStalePlan) {
		t.Fatal("stale policy accepted", err)
	}
	after, _ := os.ReadFile(paths.ConfigFile)
	if string(after) != string(raw) {
		t.Fatal("stale policy partially wrote config")
	}
	if err := CheckApplyReady(paths.ConfigFile); err != nil {
		t.Fatal("stale rejection wrote a pending journal", err)
	}
}

func TestPendingJournalRejectsChangedInputAndTamperedPolicy(t *testing.T) {
	for _, kind := range []string{"input", "journal"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			p, in, paths, db, before := policyApplyFixture(t, false)
			quiet := "23:00-08:00"
			in.Notifications.QuietHours = &quiet
			plan, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			p.checkpoint = func(stage string) error {
				if stage == "configuration" {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrApplyPending) {
				t.Fatal(err)
			}
			p.checkpoint = nil
			if kind == "input" {
				in.Verification.SendTestCardToOwner = true
			} else {
				journal, _, err := readApplyJournal(paths.ConfigFile)
				if err != nil {
					t.Fatal(err)
				}
				journal.PolicyChanges[0].After.Enabled = true
				if err := atomicFile(paths.ConfigFile+".setup-state.json", encoded(journal)); err != nil {
					t.Fatal(err)
				}
			}
			want := ErrStalePlan
			if kind == "journal" {
				want = ErrApplyPending
			}
			if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, want) {
				t.Fatal("changed transaction accepted", err)
			}
			current, _ := db.NotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID)
			if !samePolicy(current, before) {
				t.Fatal("rejected recovery changed policy")
			}
		})
	}
}

func TestApplyReadyRejectsMalformedJournalButAllowsLaterUserPolicyChanges(t *testing.T) {
	ctx := context.Background()
	p, in, paths, db, before := policyApplyFixture(t, true)
	plan, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(ctx, in, paths, plan.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := db.NotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID)
	next := current
	next.Revision++
	next.Enabled = !before.Enabled
	if err := db.SetNotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID, current.Revision, next); err != nil {
		t.Fatal(err)
	}
	if err := CheckApplyReady(paths.ConfigFile); err != nil {
		t.Fatal("historical applied journal forbade later commands", err)
	}
	if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrStalePlan) {
		t.Fatal("original apply overwrote later command", err)
	}
	b, err := os.ReadFile(paths.ConfigFile + ".setup-state.json")
	if err != nil {
		t.Fatal(err)
	}
	var j map[string]any
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	j["unknown"] = "private marker"
	if err := os.WriteFile(paths.ConfigFile+".setup-state.json", encoded(j), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckApplyReady(paths.ConfigFile); !errors.Is(err, ErrApplyPending) || strings.Contains(err.Error(), "private marker") {
		t.Fatal("malformed journal accepted or leaked", err)
	}
}

func TestExplicitEmptyQuietClearsOnlyOwnerAndOmittedFieldsRemain(t *testing.T) {
	ctx := context.Background()
	p, in, paths, db, before := policyApplyFixture(t, false)
	existing := before
	existing.QuietStart, existing.QuietEnd, existing.Timezone = "22:00", "07:00", "Asia/Tokyo"
	existing.Revision++
	if err := db.SetNotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID, before.Revision, existing); err != nil {
		t.Fatal(err)
	}
	preview, err := p.Build(ctx, in, paths)
	if err != nil || len(preview.PolicyChanges) != 0 {
		t.Fatal("omission modified effective policy", err)
	}
	clear := ""
	in.Notifications.QuietHours = &clear
	preview, err = p.Build(ctx, in, paths)
	if err != nil || len(preview.PolicyChanges) != 1 || preview.PolicyChanges[0].After.Timezone != "Asia/Tokyo" {
		t.Fatal("explicit clearing plan incorrect", err)
	}
	if _, err := p.Apply(ctx, in, paths, preview.ID); err != nil {
		t.Fatal(err)
	}
	current, err := db.NotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID)
	if err != nil || current.QuietStart != "" || current.QuietEnd != "" || current.Timezone != "Asia/Tokyo" || current.Enabled || !current.SnoozeUntil.Equal(before.SnoozeUntil) || current.Revision != 2 {
		t.Fatal("clear reset other settings", current, err)
	}
}

func TestPolicyOnlyApplyAndRecoveryCannotBypassLiveReceiverLocks(t *testing.T) {
	for _, kind := range []string{"application", "database"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			p, in, paths, db, before := policyApplyFixture(t, true)
			plan, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			var lock *os.File
			if kind == "application" {
				lock, err = store.AcquireApplication(in.Feishu.AppID)
			} else {
				lock, err = store.AcquireListener(paths.DatabaseFile)
			}
			if err != nil {
				t.Fatal(err)
			}
			result, applyErr := p.Apply(ctx, in, paths, plan.ID)
			lock.Close()
			if applyErr == nil || !strings.Contains(applyErr.Error(), "stop") || result.Changed {
				t.Fatal("policy-only change bypassed lock", applyErr)
			}
			current, _ := db.NotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID)
			if !samePolicy(current, before) {
				t.Fatal("locked policy changed")
			}
			p.checkpoint = func(stage string) error {
				if stage == "policies" {
					return errors.New("after commit")
				}
				return nil
			}
			if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrApplyPending) {
				t.Fatal(err)
			}
			p.checkpoint = nil
			if kind == "application" {
				lock, err = store.AcquireApplication(in.Feishu.AppID)
			} else {
				lock, err = store.AcquireListener(paths.DatabaseFile)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, applyErr = p.Apply(ctx, in, paths, plan.ID)
			lock.Close()
			if applyErr == nil || !strings.Contains(applyErr.Error(), "stop") {
				t.Fatal("after-commit recovery bypassed lock", applyErr)
			}
			journal, _, err := readApplyJournal(paths.ConfigFile)
			if err != nil || journal.State != "prepared" {
				t.Fatal("live recovery finalized journal", err)
			}
			if _, err := p.Apply(ctx, in, paths, plan.ID); err != nil {
				t.Fatal("unlocked recovery failed", err)
			}
		})
	}
}

func TestFrozenJournalRejectsDroppedPolicyIntent(t *testing.T) {
	for _, field := range []string{"changes", "snapshots", "schema", "backup"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			p, in, paths, db, before := policyApplyFixture(t, false)
			quiet := "23:00-08:00"
			in.Notifications.QuietHours = &quiet
			plan, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			p.checkpoint = func(stage string) error {
				if stage == "configuration" {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrApplyPending) {
				t.Fatal(err)
			}
			journal, _, err := readApplyJournal(paths.ConfigFile)
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "changes":
				journal.PolicyChanges = nil
			case "snapshots":
				journal.ExistingPolicies = nil
			case "schema":
				journal.DatabaseSchema = 0
			case "backup":
				journal.BackupFile = ""
			}
			if err := atomicFile(paths.ConfigFile+".setup-state.json", encoded(journal)); err != nil {
				t.Fatal(err)
			}
			p.checkpoint = nil
			if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrApplyPending) {
				t.Fatal("lost intent accepted", err)
			}
			if err := CheckApplyReady(paths.ConfigFile); !errors.Is(err, ErrApplyPending) {
				t.Fatal("malformed journal allowed startup", err)
			}
			current, err := db.NotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID)
			if err != nil || !samePolicy(current, before) || policyHistoryCount(t, db, in.Feishu.AppID, *in.Identity.OwnerOpenID) != 1 {
				t.Fatal("invalid recovery changed DB", err)
			}
		})
	}
}

func TestPolicyRecoverySurvivesDaemonSocketRotation(t *testing.T) {
	for _, stage := range []string{"deployment", "prepared", "configuration", "policies", "applied"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			p, in, paths, db, before := policyApplyFixture(t, false)
			quiet := "23:00-08:00"
			in.Notifications.QuietHours = &quiet
			plan, err := p.Build(ctx, in, paths)
			if err != nil {
				t.Fatal(err)
			}
			p.checkpoint = func(at string) error {
				if at == stage {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrApplyPending) {
				t.Fatal(err)
			}
			if ready := CheckApplyReady(paths.ConfigFile); stage == "applied" && ready != nil || stage != "applied" && !errors.Is(ready, ErrApplyPending) {
				t.Fatal("incorrect startup boundary", ready)
			}
			probe := p.Probe
			p.Probe = func(ctx context.Context, binary string) (Environment, error) {
				env, err := probe(ctx, binary)
				env.Daemon.SocketPath = "/different/daemon/socket.sock"
				return env, err
			}
			p.checkpoint = nil
			if _, err := p.Apply(ctx, in, paths, plan.ID); err != nil {
				t.Fatal("same authorization failed after socket rotation", err)
			}
			if err := CheckApplyReady(paths.ConfigFile); err != nil {
				t.Fatal(err)
			}
			if _, exists, err := readDeploymentApply(paths.DatabaseFile); err != nil || exists {
				t.Fatal("completed pointer not retired", err)
			}
			current, err := db.NotificationPolicy(ctx, in.Feishu.AppID, *in.Identity.OwnerOpenID)
			if err != nil || current.Enabled || !current.SnoozeUntil.Equal(before.SnoozeUntil) || current.QuietStart != "23:00" || current.Revision != 1 || policyHistoryCount(t, db, in.Feishu.AppID, *in.Identity.OwnerOpenID) != 2 {
				t.Fatal("recovery lost settings or duplicated audit", err)
			}
		})
	}
}

func TestPendingPolicyDeploymentBlocksBackupAndDatabaseAliases(t *testing.T) {
	ctx := context.Background()
	p, in, paths, _, _ := policyApplyFixture(t, false)
	quiet := "23:00-08:00"
	in.Notifications.QuietHours = &quiet
	plan, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	p.checkpoint = func(stage string) error {
		if stage == "configuration" {
			return errors.New("interrupted")
		}
		return nil
	}
	if _, err := p.Apply(ctx, in, paths, plan.ID); !errors.Is(err, ErrApplyPending) {
		t.Fatal(err)
	}
	journal, _, err := readApplyJournal(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckApplyReady(journal.BackupFile); !errors.Is(err, ErrApplyPending) {
		t.Fatal("backup bypassed pending deployment", err)
	}
	aliases := []string{paths.DatabaseFile, filepath.Join(filepath.Dir(paths.ConfigFile), "database-alias")}
	if err := os.Symlink(paths.DatabaseFile, aliases[1]); err != nil {
		t.Fatal(err)
	}
	parentAlias := filepath.Join(filepath.Dir(paths.ConfigFile), "data-alias")
	if err := os.Symlink(filepath.Dir(paths.DatabaseFile), parentAlias); err != nil {
		t.Fatal(err)
	}
	aliases = append(aliases, filepath.Join(parentAlias, filepath.Base(paths.DatabaseFile)))
	for i, alias := range aliases {
		c, err := config.Load(paths.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		c.Storage.DatabaseFile = alias
		b, err := toml.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		alternate := filepath.Join(t.TempDir(), "copied.toml")
		if err := os.WriteFile(alternate, b, 0600); err != nil {
			t.Fatal(err)
		}
		if err := CheckApplyReady(alternate); !errors.Is(err, ErrApplyPending) {
			t.Fatalf("alias %d bypassed pending deployment: %v", i, err)
		}
	}
	p.checkpoint = nil
	if _, err := p.Apply(ctx, in, paths, plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := CheckApplyReady(journal.BackupFile); err != nil {
		t.Fatal("completed deployment remained blocked", err)
	}
}
