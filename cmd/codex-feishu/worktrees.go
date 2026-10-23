package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/creation"
	"codex-feishu/internal/store"
)

type worktreesDependencies struct {
	connect         func(context.Context, config.Config) (creation.WorktreeAuditNative, func(), error)
	applicationLock func(string) (io.Closer, error)
	listenerLock    func(string) (io.Closer, error)
}

func defaultWorktreesDependencies() worktreesDependencies {
	return worktreesDependencies{
		connect: func(ctx context.Context, cfg config.Config) (creation.WorktreeAuditNative, func(), error) {
			native, err := serveNativeConnect(cfg.Codex.CLIPath, codexclient.Discover, codexclient.Connect, true)(ctx)
			if err != nil {
				return nil, nil, errors.New("existing native generation unavailable")
			}
			if _, ok := native.(creation.Native); !ok {
				native.Close()
				return nil, nil, errors.New("native creation capability unavailable")
			}
			reader, ok := native.(creation.WorktreeAuditNative)
			if !ok {
				native.Close()
				return nil, nil, errors.New("native worktree compatibility unavailable")
			}
			return reader, native.Close, nil
		},
		applicationLock: func(app string) (io.Closer, error) { return store.AcquireApplication(app) },
		listenerLock:    func(path string) (io.Closer, error) { return store.AcquireListener(path) },
	}
}

func runWorktrees(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] != "audit" && args[0] != "repair" {
		emit(output{Command: "worktrees", State: "unsupported-command"})
		return 2
	}
	flags := flag.NewFlagSet("worktrees "+args[0], flag.ContinueOnError)
	path := flags.String("config", "", "absolute TOML config path")
	report := flags.String("report", "", "absolute private audit report path")
	_ = flags.Bool("json", false, "JSON output (default)")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 || !filepath.IsAbs(*path) || !filepath.IsAbs(*report) {
		emit(output{Command: "worktrees " + args[0], State: "invalid-arguments"})
		return 2
	}
	o, code := inspectWorktrees(ctx, *path, *report, args[0], defaultWorktreesDependencies())
	emit(o)
	return code
}

func inspectWorktrees(ctx context.Context, path, reportPath, action string, deps worktreesDependencies) (output, int) {
	o := output{Command: "worktrees " + action, State: "checking", NextSteps: []string{creation.WorktreeAuditWarning}}
	if action != "audit" && action != "repair" || !filepath.IsAbs(path) || !filepath.IsAbs(reportPath) {
		o.State = "invalid-arguments"
		return o, 2
	}
	cfg, err := config.Load(path)
	if err != nil {
		o.State = "config-invalid"
		return o, 2
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var plan creation.WorktreeAuditReport
	if action == "repair" {
		plan, err = creation.ReadWorktreeAuditReport(reportPath)
		if err != nil {
			o.State = "private-report-unavailable"
			return o, 3
		}
		applicationLock, err := deps.applicationLock(cfg.Feishu.AppID)
		if err != nil {
			o.State = "stop-required"
			return o, 6
		}
		defer applicationLock.Close()
		listenerLock, err := deps.listenerLock(cfg.Storage.DatabaseFile)
		if err != nil {
			o.State = "stop-required"
			return o, 6
		}
		defer listenerLock.Close()
	}
	var db *store.Store
	if action == "audit" {
		var schema int
		db, schema, err = store.OpenReadOnly(ctx, cfg.Storage.DatabaseFile)
		if err == nil && schema != store.SchemaVersion {
			db.Close()
			db, err = nil, errors.New("unsupported audit schema")
		}
	} else {
		db, err = store.OpenExisting(ctx, cfg.Storage.DatabaseFile, store.SchemaVersion)
	}
	if err != nil {
		o.State = "storage-unavailable"
		return o, 3
	}
	defer db.Close()
	native, closeNative, err := deps.connect(ctx, cfg)
	if err != nil || native == nil || closeNative == nil {
		if closeNative != nil {
			closeNative()
		}
		o.State = "native-unavailable"
		return o, 4
	}
	defer closeNative()
	auditor := creation.WorktreeAuditor{Store: db, Native: native, Config: cfg}
	if action == "audit" {
		plan, err = auditor.Audit(ctx)
		if err != nil {
			o.State = "audit-unavailable"
			return o, 4
		}
		if creation.WriteWorktreeAuditReport(reportPath, plan) != nil {
			o.State = "private-report-unavailable"
			return o, 3
		}
		counts := map[string]int{}
		for _, entry := range plan.Entries {
			counts[entry.Status]++
		}
		o.State, o.OK = "audited", true
		o.Data = map[string]any{"total": len(plan.Entries), "counts": counts}
		return o, 0
	}
	counts, err := auditor.Repair(ctx, plan)
	o.Data = map[string]any{"counts": counts}
	if err != nil {
		o.State = "repair-unavailable"
		return o, 3
	}
	if counts["unknown"] > 0 || counts["refused"] > 0 {
		o.State = "repair-pending"
		return o, 5
	}
	o.State, o.OK = "repaired", true
	return o, 0
}
