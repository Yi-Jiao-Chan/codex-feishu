package main

import (
	"context"
	"errors"
	"flag"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
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
	o := output{Command: "status", State: "readonly-snapshot"}
	data := map[string]any{"bridge_connections": "unknown", "host_id": cfg.Host.ID}
	identity, identityErr := control.Identity(*path, cfg)
	endpoint, endpointErr := control.Path(cfg.Feishu.AppID)
	var live control.Snapshot
	liveErr := control.ErrUnavailable
	if identityErr == nil && endpointErr == nil {
		live, liveErr = control.Query(ctx, endpoint, identity)
	}
	code := 5
	if liveErr == nil {
		o, code = liveStatus(live)
		data["bridge_connections"], data["instance"], data["durable"] = "observed", live, live.Persistent
	} else {
		o.Checks = append(o.Checks, check{"instance.live_health", "pending", true, "managed endpoint unavailable; snapshot cannot prove live bridge connections"})
		o.NextSteps = []string{"start the managed bridge or inspect its control endpoint; the legacy M0 listener has no control endpoint"}
		if errors.Is(liveErr, control.ErrIdentity) || identityErr != nil {
			o.State, code = "instance-configuration-conflict", 6
			o.Checks[0].Status, o.Checks[0].Detail = "fail", "running configuration differs; explicitly stop, apply and restart the bridge"
		} else if errors.Is(liveErr, control.ErrUnsafe) || endpointErr != nil {
			o.State, code = "instance-endpoint-unsafe", 6
			o.Checks[0].Status, o.Checks[0].Detail = "fail", "control endpoint path, permissions or OS peer identity could not be verified"
		}
	}
	if manager, err := userservice.New(userservice.Options{}); err == nil {
		result := manager.ReadStatus(ctx, *path)
		data["user_service"] = result
		for _, c := range result.Checks {
			// A live foreground bridge is valid without a systemd unit. The
			// configuration workflow verifies service lifecycle separately.
			o.Checks = append(o.Checks, check{c.ID, c.Status, false, map[string]string{"message": c.Detail, "remediation": c.Remediation}})
		}
	}
	if liveErr == nil {
		o.Data = data
		emit(o)
		return code
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
	o.Data = data
	emit(o)
	return code
}

func liveStatus(s control.Snapshot) (output, int) {
	o := output{Command: "status", State: "running", OK: true, Checks: []check{{"instance.live_health", "pass", true, "read from the running same-user bridge with matching configuration"}}}
	observe := func(id string, good bool, detail string) {
		status := "pass"
		if !good {
			status, o.OK = "pending", false
		}
		o.Checks = append(o.Checks, check{id, status, true, detail})
	}
	observe("instance.running", s.Running, "current service lifecycle")
	observe("instance.codex_connection", s.NativeConnected, "existing native generation attachment")
	observe("instance.feishu_connection", s.FeishuConnected, "existing Feishu receiver ready state")
	observe("instance.bindings_reconciled", int64(s.HydratedBindings) == s.Persistent.Bindings, "bound threads hydrated in this native generation")
	// Journals can describe the same operation at several layers. Check whether
	// any result needs confirmation; adding their counts would double-count it.
	known := true
	for _, states := range []struct {
		counts  map[string]int64
		pending []string
	}{
		{s.Persistent.Inputs, []string{"unknown", "submitting", "steering", "removing"}},
		{s.Persistent.Outbound, []string{"unknown", "dispatching"}},
		{s.Persistent.OutboundAttempts, []string{"unknown", "dispatching"}},
		{s.Persistent.NativeActions, []string{"unknown", "running"}},
		{s.Persistent.NativeAttempts, []string{"awaiting_reconciliation", "dispatching"}},
		{s.Persistent.CommandReplies, []string{"unknown", "dispatching"}},
		{s.Persistent.Reactions, []string{"unknown", "changing"}},
		{s.Persistent.GroupIntents, []string{"unknown", "dispatching"}},
		{s.Persistent.TaskControls, []string{"unknown", "pending"}},
		{s.Persistent.TaskClaims, []string{"unknown", "pending"}},
		{s.Persistent.Requests, []string{"awaiting_reconciliation", "reminder_unknown", "reminder_dispatching"}},
		{s.Persistent.ReminderAttempts, []string{"unknown", "dispatching"}},
	} {
		for _, state := range states.pending {
			if states.counts[state] > 0 {
				known = false
			}
		}
	}
	observe("instance.operations_known", known, "current remote writes and recovered requests require confirmation; resolved native attempt ambiguity is retained only as historical audit counts")
	if !o.OK {
		o.State = "reconciling"
		o.NextSteps = []string{"inspect pending checks and frozen operations; do not resubmit unknown operations"}
		return o, 5
	}
	return o, 0
}
