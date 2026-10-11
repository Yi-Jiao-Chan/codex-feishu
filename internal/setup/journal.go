package setup

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"codex-feishu/internal/config"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32 && strings.ToLower(value) == value
}

func readApplyJournal(target string) (applyJournal, bool, error) {
	var journal applyJournal
	path := target + ".setup-state.json"
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return journal, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return journal, true, ErrApplyPending
	}
	b, err := regularFile(path, false)
	if err != nil {
		return journal, true, ErrApplyPending
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&journal) != nil {
		return journal, true, ErrApplyPending
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return journal, true, ErrApplyPending
	}
	if journal.Version < 0 || journal.Version > 1 || !validDigest(journal.PlanID) || !validDigest(journal.SourceDigest) || !validDigest(journal.AfterDigest) || journal.BeforeDigest != "missing" && !validDigest(journal.BeforeDigest) || journal.State != "prepared" && journal.State != "applied" {
		return journal, true, ErrApplyPending
	}
	if journal.BackupFile != "" && journal.BackupFile != target+".backup."+journal.PlanID[:16] {
		return journal, true, ErrApplyPending
	}
	if journal.Version == 0 {
		if len(journal.PolicyChanges) > 0 || len(journal.ExistingPolicies) > 0 || journal.DatabaseSchema != 0 || journal.HostID != "" || journal.AppID != "" || journal.DatabaseFile != "" {
			return journal, true, ErrApplyPending
		}
	} else {
		if journal.BeforeDigest != "missing" && journal.BeforeDigest != journal.AfterDigest && journal.BackupFile == "" {
			return journal, true, ErrApplyPending
		}
		if journal.HostID == "" || !config.ValidAppID(journal.AppID) || !filepath.IsAbs(journal.DatabaseFile) {
			return journal, true, ErrApplyPending
		}
		if journal.DatabaseSchema != 0 && (journal.DatabaseSchema < 5 || journal.DatabaseSchema > store.SchemaVersion) || len(journal.PolicyChanges) > 0 && journal.DatabaseSchema != store.SchemaVersion {
			return journal, true, ErrApplyPending
		}
		if digest(encoded([]any{journal.SourceDigest, journal.BeforeDigest, journal.AfterDigest, journal.ExistingPolicies, journal.PolicyChanges, journal.DatabaseSchema})) != journal.PlanID {
			return journal, true, ErrApplyPending
		}
		snapshots := map[string]rules.Policy{}
		for _, snapshot := range journal.ExistingPolicies {
			if _, duplicate := snapshots[snapshot.OpenID]; duplicate || !config.ValidOpenID(snapshot.OpenID) || snapshot.Policy.Validate() != nil {
				return journal, true, ErrApplyPending
			}
			snapshots[snapshot.OpenID] = snapshot.Policy
		}
		seen := map[string]bool{}
		for _, change := range journal.PolicyChanges {
			if !config.ValidOpenID(change.OpenID) || seen[change.OpenID] || change.Before.Validate() != nil || change.After.Validate() != nil || change.Before.Revision == 1<<63-1 || change.After.Revision != change.Before.Revision+1 {
				return journal, true, ErrApplyPending
			}
			before, found := snapshots[change.OpenID]
			if !found || !samePolicy(before, change.Before) {
				return journal, true, ErrApplyPending
			}
			seen[change.OpenID] = true
		}
	}
	return journal, true, nil
}

// CheckApplyReady is read-only. The caller must hold receiver/deployment locks
// and revalidate its config snapshot before starting connections or migrations.
// Applied policies may later change through normal commands; only unfinished
// or malformed journals block startup, not historical after-policy values.
func CheckApplyReady(configPath string) error {
	target, err := canonicalTarget(configPath)
	if err != nil {
		return ErrApplyPending
	}
	journal, found, err := readApplyJournal(target)
	if err != nil {
		return err
	}
	if found && journal.State != "applied" {
		return ErrApplyPending
	}
	c, err := config.Load(target)
	if err != nil {
		return ErrApplyPending
	}
	marker, found, err := readDeploymentApply(c.Storage.DatabaseFile)
	if err != nil {
		return err
	}
	if found && (marker.HostID != c.Host.ID || marker.AppID != c.Feishu.AppID || !deploymentApplyComplete(marker)) {
		return ErrApplyPending
	}
	return nil
}

func validateJournalPlan(journal applyJournal, plan Plan, in Input) error {
	source := plan.sourceDigest
	if journal.Version == 0 {
		source = plan.legacySourceDigest
	}
	if journal.SourceDigest != source || journal.AfterDigest != digest(plan.content) || plan.previousDigest != journal.BeforeDigest && plan.previousDigest != journal.AfterDigest {
		return ErrStalePlan
	}
	if journal.BackupFile != "" {
		backup, err := regularFile(journal.BackupFile, false)
		if err != nil || digest(backup) != journal.BeforeDigest {
			return ErrApplyPending
		}
	}
	if journal.Version == 0 {
		return nil
	}
	if journal.HostID != plan.Config.Host.ID || journal.AppID != plan.Config.Feishu.AppID || journal.DatabaseFile != plan.Config.Storage.DatabaseFile {
		return ErrStalePlan
	}
	if len(journal.PolicyChanges) > 1 {
		return ErrStalePlan
	}
	for _, change := range journal.PolicyChanges {
		// Setup changes the owner's explicitly selected quiet/timezone only.
		// A journal may not add recipients, enable mentions or extend snooze.
		if change.OpenID != plan.Config.Access.OwnerOpenID || !has(plan.Config.Notifications.TargetOpenIDs, change.OpenID) || change.Before.Enabled != change.After.Enabled || !change.Before.SnoozeUntil.Equal(change.After.SnoozeUntil) {
			return ErrStalePlan
		}
		next := change.Before
		if in.Notifications.QuietHours != nil {
			next.QuietStart, next.QuietEnd, _ = strings.Cut(*in.Notifications.QuietHours, "-")
		}
		if in.Notifications.Timezone != "" {
			next.Timezone = in.Notifications.Timezone
		}
		next.Revision++
		if !samePolicy(next, change.After) {
			return ErrStalePlan
		}
	}
	return nil
}

func samePolicy(a, b rules.Policy) bool {
	return a.Enabled == b.Enabled && a.SnoozeUntil.Equal(b.SnoozeUntil) && a.QuietStart == b.QuietStart && a.QuietEnd == b.QuietEnd && a.Timezone == b.Timezone && a.Revision == b.Revision
}

// Preflight precedes config replacement. The transactional store method repeats
// the complete CAS check so changes between preflight and commit cannot be lost.
func journalPolicyState(ctx context.Context, db *store.Store, journal applyJournal) (bool, error) {
	var app string
	if err := db.DB.QueryRowContext(ctx, "SELECT app_id FROM deployment WHERE host_id=?", journal.HostID).Scan(&app); err != nil || app != journal.AppID {
		return false, store.ErrConflict
	}
	allBefore, allAfter := true, true
	for _, change := range journal.PolicyChanges {
		current, err := db.NotificationPolicy(ctx, journal.AppID, change.OpenID)
		if err != nil {
			return false, err
		}
		allBefore = allBefore && samePolicy(current, change.Before)
		allAfter = allAfter && samePolicy(current, change.After)
	}
	if !allBefore && !allAfter {
		return false, store.ErrConflict
	}
	return allBefore, nil
}
