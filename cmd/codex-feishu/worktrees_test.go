package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/creation"
	"codex-feishu/internal/store"
)

type worktreesFixtureNative struct{ home, version string }

func (n worktreesFixtureNative) Metadata(context.Context, string) (codexclient.Thread, error) {
	return codexclient.Thread{}, errors.New("empty fixture must not read threads")
}
func (n worktreesFixtureNative) CodexHome() string        { return n.home }
func (n worktreesFixtureNative) AppServerVersion() string { return n.version }

type worktreesFixtureCloser struct{ close func() }

func (c worktreesFixtureCloser) Close() error { c.close(); return nil }

func worktreesFixture(t *testing.T) (string, config.Config, string, worktreesDependencies, *[]string) {
	t.Helper()
	path, cfg, _, _ := doctorFixture(t)
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(cfg.Storage.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	var calls []string
	native := worktreesFixtureNative{home: t.TempDir(), version: "0.162.1"}
	deps := worktreesDependencies{
		connect: func(context.Context, config.Config) (creation.WorktreeAuditNative, func(), error) {
			calls = append(calls, "connect")
			return native, func() { calls = append(calls, "close-native") }, nil
		},
		applicationLock: func(app string) (io.Closer, error) {
			if app != cfg.Feishu.AppID {
				t.Fatal("wrong application lock scope")
			}
			calls = append(calls, "application")
			return worktreesFixtureCloser{func() { calls = append(calls, "close-application") }}, nil
		},
		listenerLock: func(db string) (io.Closer, error) {
			if db != cfg.Storage.DatabaseFile {
				t.Fatal("wrong listener lock scope")
			}
			calls = append(calls, "listener")
			return worktreesFixtureCloser{func() { calls = append(calls, "close-listener") }}, nil
		},
	}
	return path, cfg, filepath.Join(filepath.Dir(path), "private-plan.json"), deps, &calls
}

func TestWorktreesAuditPrivateReportAndStatisticsOnly(t *testing.T) {
	path, cfg, report, deps, calls := worktreesFixture(t)
	o, code := inspectWorktrees(context.Background(), path, report, "audit", deps)
	if code != 0 || !o.OK || o.State != "audited" || strings.Join(*calls, ",") != "connect,close-native" {
		t.Fatalf("audit failed or acquired mutation locks: code=%d output=%+v calls=%v", code, o, *calls)
	}
	plan, err := creation.ReadWorktreeAuditReport(report)
	if err != nil || len(plan.Entries) != 0 || plan.HostID != cfg.Host.ID || plan.AppID != cfg.Feishu.AppID {
		t.Fatal("invalid private scoped plan", err)
	}
	info, err := os.Stat(report)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("report permissions", err)
	}
	b, _ := json.Marshal(o)
	for _, forbidden := range []string{cfg.Host.ID, cfg.Feishu.AppID, cfg.Storage.DatabaseFile, cfg.Access.OwnerOpenID, "doctor-secret-never-output"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatal("public output leaked private identity")
		}
	}
	before, _ := os.ReadFile(report)
	o, code = inspectWorktrees(context.Background(), path, report, "audit", deps)
	after, _ := os.ReadFile(report)
	if code != 3 || o.State != "private-report-unavailable" || string(before) != string(after) {
		t.Fatal("audit overwrote an existing plan", code, o)
	}
}

func TestWorktreesRepairRequiresStoppedServiceBeforeNativeConnection(t *testing.T) {
	for _, busy := range []string{"application", "listener"} {
		t.Run(busy, func(t *testing.T) {
			path, _, report, deps, calls := worktreesFixture(t)
			if _, code := inspectWorktrees(context.Background(), path, report, "audit", deps); code != 0 {
				t.Fatal("cannot create isolated plan")
			}
			*calls = nil
			if busy == "application" {
				deps.applicationLock = func(string) (io.Closer, error) { return nil, errors.New("busy") }
			} else {
				deps.listenerLock = func(string) (io.Closer, error) { return nil, errors.New("busy") }
			}
			o, code := inspectWorktrees(context.Background(), path, report, "repair", deps)
			wantCalls := ""
			if busy == "listener" {
				wantCalls = "application,close-application"
			}
			if code != 6 || o.State != "stop-required" || strings.Join(*calls, ",") != wantCalls {
				t.Fatalf("repair bypassed service exclusion: code=%d state=%s calls=%v", code, o.State, *calls)
			}
		})
	}
}

func TestWorktreesRepairLocksExistingDatabaseAndPreservesSchema(t *testing.T) {
	path, cfg, report, deps, calls := worktreesFixture(t)
	if _, code := inspectWorktrees(context.Background(), path, report, "audit", deps); code != 0 {
		t.Fatal("cannot create isolated plan")
	}
	*calls = nil
	o, code := inspectWorktrees(context.Background(), path, report, "repair", deps)
	if code != 0 || !o.OK || o.State != "repaired" || strings.Join(*calls, ",") != "application,listener,connect,close-native,close-listener,close-application" {
		t.Fatalf("repair lock/close order: code=%d state=%s calls=%v", code, o.State, *calls)
	}
	db, schema, err := store.OpenReadOnly(context.Background(), cfg.Storage.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var attempts int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM creation_attempts`).Scan(&attempts); err != nil || schema != 31 || attempts != 0 {
		t.Fatal("empty repair changed schema or created unrelated attempts", schema, attempts, err)
	}
}

func TestWorktreesRepairRefusesPublicPlanBeforeAcquiringLocks(t *testing.T) {
	path, _, report, deps, calls := worktreesFixture(t)
	if _, code := inspectWorktrees(context.Background(), path, report, "audit", deps); code != 0 {
		t.Fatal("cannot create isolated plan")
	}
	if err := os.Chmod(report, 0644); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	o, code := inspectWorktrees(context.Background(), path, report, "repair", deps)
	if code != 3 || o.State != "private-report-unavailable" || len(*calls) != 0 {
		t.Fatal("untrusted plan reached locks or native", code, o.State, *calls)
	}
}

func TestWorktreesAuditRefusesUnverifiedNativeVersion(t *testing.T) {
	path, _, report, deps, _ := worktreesFixture(t)
	deps.connect = func(context.Context, config.Config) (creation.WorktreeAuditNative, func(), error) {
		return worktreesFixtureNative{home: t.TempDir(), version: "0.162.2"}, func() {}, nil
	}
	o, code := inspectWorktrees(context.Background(), path, report, "audit", deps)
	if code != 4 || o.State != "audit-unavailable" {
		t.Fatal("future protocol was accepted", code, o)
	}
	if _, err := os.Stat(report); !os.IsNotExist(err) {
		t.Fatal("unsupported generation wrote a plan")
	}
}
