package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"time"

	"codex-feishu/internal/attachments"
	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
)

func runAttachments(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] != "status" && args[0] != "cleanup" {
		emit(output{Command: "attachments", State: "unsupported-command"})
		return 2
	}
	command := "attachments " + args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	path := flags.String("config", "", "absolute config path")
	_ = flags.Bool("json", false, "JSON output (default)")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 || *path == "" {
		emit(output{Command: command, State: "config-required"})
		return 2
	}
	o, code := inspectAttachments(ctx, *path, args[0])
	emit(o)
	return code
}

func inspectAttachments(ctx context.Context, path, action string) (output, int) {
	o := output{Command: "attachments " + action, State: "checking"}
	cfg, err := config.Load(path)
	if err != nil {
		o.State = "config-invalid"
		return o, 2
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	limits := cfg.AttachmentLimits()
	cache := &attachments.Cache{Root: limits.Directory, Limits: limits}
	if action == "cleanup" {
		applicationLock, err := store.AcquireApplication(cfg.Feishu.AppID)
		if err != nil {
			o.State = "listener-lock-conflict"
			o.Checks = []check{{"application.lock", "fail", true, "stop the bridge before cleaning attachment cache"}}
			return o, 6
		}
		defer applicationLock.Close()
		listenerLock, err := store.AcquireListener(cfg.Storage.DatabaseFile)
		if err != nil {
			o.State = "listener-lock-conflict"
			o.Checks = []check{{"deployment.lock", "fail", true, "stop the bridge before cleaning attachment cache"}}
			return o, 6
		}
		defer listenerLock.Close()
		db, err := store.OpenExisting(ctx, cfg.Storage.DatabaseFile, store.SchemaVersion)
		if err != nil {
			o.State = "storage-unavailable"
			return o, 3
		}
		defer db.Close()
		refs, err := db.AttachmentCacheReferences(ctx, cfg.Host.ID, cfg.Feishu.AppID)
		if err != nil {
			o.State = "attachment-scope-unverified"
			return o, 3
		}
		files, bytes, err := cache.Cleanup(ctx, refs)
		o.Data = map[string]any{"removed_files": files, "removed_bytes": bytes}
		if err != nil {
			o.State = "attachment-cleanup-failed"
			return o, 3
		}
		o.State = "attachment-cache-cleaned"
		o.OK = true
		return o, 0
	}
	if action != "status" {
		o.State = "unsupported-command"
		return o, 2
	}
	used, err := cache.ExistingUsage()
	if err != nil {
		o.State = "attachment-cache-unavailable"
		return o, 3
	}
	usage := store.AttachmentUsage{BatchStates: map[string]int64{}, ResourceStates: map[string]int64{}, SubmissionStates: map[string]int64{}}
	initialized := true
	if _, err := os.Lstat(cfg.Storage.DatabaseFile); errors.Is(err, os.ErrNotExist) {
		initialized = false
	} else {
		db, schema, err := store.OpenReadOnly(ctx, cfg.Storage.DatabaseFile)
		if err != nil || schema != store.SchemaVersion {
			if db != nil {
				db.Close()
			}
			o.State = "storage-unavailable"
			return o, 3
		}
		defer db.Close()
		usage, err = db.AttachmentUsage(ctx, cfg.Host.ID, cfg.Feishu.AppID)
		if err != nil {
			o.State = "attachment-scope-unverified"
			return o, 3
		}
	}
	o.Data = map[string]any{"enabled": limits.Enabled, "cache_bytes": used, "disk_quota_bytes": limits.DiskQuotaBytes, "storage_initialized": initialized, "usage": usage}
	o.State = "attachment-storage-status"
	o.OK = true
	return o, 0
}
