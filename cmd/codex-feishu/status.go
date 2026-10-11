package main

import (
	"context"
	"flag"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"codex-feishu/internal/userservice"
)

func runStatus(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	path := flags.String("config", "", "absolute config path")
	_ = flags.Bool("json", false, "JSON output (default)")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || *path == "" {
		emit(output{Command: "status", State: "config-required"})
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		emit(output{Command: "status", State: "config-invalid"})
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	o := output{Command: "status", State: "readonly-snapshot", Checks: []check{{"instance.live_health", "pending", true, "managed instance control channel is not yet integrated; this snapshot does not prove live bridge connections"}}}
	data := map[string]any{"bridge_connections": "unknown", "host_id": cfg.Host.ID}
	if manager, err := userservice.New(userservice.Options{}); err == nil {
		result := manager.ReadStatus(ctx, *path)
		data["user_service"] = result
		for _, c := range result.Checks {
			o.Checks = append(o.Checks, check{c.ID, c.Status, c.Required, map[string]string{"message": c.Detail, "remediation": c.Remediation}})
		}
	}
	if daemon, err := codexclient.Discover(ctx, cfg.Codex.CLIPath); err == nil {
		data["codex_daemon"] = daemon
		o.Checks = append(o.Checks, check{"codex.daemon", "pass", true, "existing daemon inspected; bridge attachment unknown"})
	} else {
		o.Checks = append(o.Checks, check{"codex.daemon", "fail", true, "existing daemon unavailable"})
	}
	if db, version, err := store.OpenReadOnly(ctx, cfg.Storage.DatabaseFile); err == nil {
		health, healthErr := db.Health(ctx, version, cfg.Host.ID, cfg.Feishu.AppID)
		db.Close()
		if healthErr == nil {
			data["durable"] = health
			o.Checks = append(o.Checks, check{"store.snapshot", "pass", true, "read-only aggregate snapshot; no schema migration"})
		} else {
			o.Checks = append(o.Checks, check{"store.snapshot", "fail", true, "database health unavailable or deployment mismatch"})
		}
	} else {
		o.Checks = append(o.Checks, check{"store.snapshot", "pending", true, "database missing or schema unavailable; nothing created"})
	}
	o.Data, o.NextSteps = data, []string{"live instance health remains pending; no additional Feishu receiver was opened"}
	emit(o)
	return 5
}
