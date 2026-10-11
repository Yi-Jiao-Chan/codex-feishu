package main

import (
	"codex-feishu/compat"
	"codex-feishu/internal/app"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var commit = "development"

// Set true only after the complete P1 acceptance matrix has been verified.
var phase1Released = false

type check struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Required bool   `json:"required"`
	Detail   any    `json:"detail,omitempty"`
}
type output struct {
	SchemaVersion int      `json:"schema_version"`
	Command       string   `json:"command"`
	OK            bool     `json:"ok"`
	State         string   `json:"state"`
	Checks        []check  `json:"checks"`
	NextSteps     []string `json:"next_steps"`
	Data          any      `json:"data,omitempty"`
	PlanID        string   `json:"plan_id,omitempty"`
	Paths         any      `json:"paths,omitempty"`
}

func emit(o output) {
	o.SchemaVersion = 1
	if o.Checks == nil {
		o.Checks = []check{}
	}
	if o.NextSteps == nil {
		o.NextSteps = []string{}
	}
	json.NewEncoder(os.Stdout).Encode(o)
}
func main() {
	syscall.Umask(0077)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:]))
}
func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: codex-feishu version | doctor | verify | setup | service | status | serve | m0-listen (development, not phase1 release)")
		return 2
	}
	command := args[0]
	switch command {
	case "serve":
		return runServe(ctx, args[1:])
	case "setup":
		return runSetup(ctx, args[1:])
	case "service":
		return runService(ctx, args[1:])
	case "status":
		return runStatus(ctx, args[1:])
	case "doctor":
		return runDoctor(ctx, args[1:])
	case "verify":
		return runVerify(ctx, args[1:])
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	cfgPath := flags.String("config", "", "absolute TOML config path")
	_ = flags.Bool("json", false, "JSON output (default)")
	thread := flags.String("thread", "", "m0-listen: explicit App test thread ID")
	chat := flags.String("chat", "", "m0-listen: previously verified test group ID")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 {
		return 2
	}
	if command == "version" {
		emit(output{Command: command, OK: true, State: "phase1-development", Data: map[string]any{"version": "0.0.1-dev", "commit": commit, "config_schema": 1, "commands": []string{"version", "doctor", "verify", "m0-listen", "serve", "setup", "service", "status"}, "phase1_released": phase1Released, "sdk": "github.com/larksuite/oapi-sdk-go/v3@v3.12.0", "codex_protocol": "0.162.0", "feishu_permissions_manifest": compat.FeishuManifest(), "m0_app_ui_verified": true, "m0_feishu_verified": true, "phase1_service_ui_verified": false, "setup_pair_ui_verified": false, "capabilities": map[string]any{"setup_plan": true, "setup_apply": true, "setup_policy_updates": true, "setup_pair": true, "status_live_instance": true, "status_readonly_snapshot": true, "doctor_managed_instance": true, "doctor_permission_metadata": true, "feishu_mirror_rate_retry": true, "verify_readonly": true, "verify_managed_tests": false}}})
		return 0
	}
	if command != "m0-listen" {
		emit(output{Command: command, State: "unsupported-command"})
		return 2
	}
	o := output{Command: command, State: "checking"}
	callCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	d, err := codexclient.Discover(callCtx, "auto")
	if err != nil {
		o.Checks = append(o.Checks, check{"codex.daemon", "fail", true, err.Error()})
		emit(o)
		return 3
	}
	o.Checks = append(o.Checks, check{"codex.daemon", "pass", true, d})
	if d.AppServerVersion != "0.162.0" {
		o.Checks = append(o.Checks, check{"codex.compatibility", "fail", true, "unverified server version; writes disabled"})
		emit(o)
		return 3
	}
	c, err := codexclient.Connect(ctx, d.SocketPath)
	if err != nil {
		o.Checks = append(o.Checks, check{"codex.handshake", "fail", true, err.Error()})
		emit(o)
		return 3
	}
	defer c.Close()
	o.Checks = append(o.Checks, check{"codex.handshake", "pass", true, nil})
	threads, err := c.List(callCtx)
	if err != nil {
		o.Checks = append(o.Checks, check{"codex.thread_list", "fail", true, err.Error()})
		emit(o)
		return 3
	}
	o.Checks = append(o.Checks, check{"codex.thread_list", "pass", true, map[string]any{"count": len(threads), "full_pagination": true}})
	if *cfgPath == "" {
		emit(output{Command: command, State: "config-required"})
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		o.Checks = append(o.Checks, check{"config", "fail", true, err.Error()})
		emit(o)
		return 2
	}
	secret, err := cfg.Secret()
	if err != nil {
		o.Checks = append(o.Checks, check{"feishu.secret", "fail", true, err.Error()})
		emit(o)
		return 4
	}
	if command == "m0-listen" {
		applicationLock, e := store.AcquireApplication(cfg.Feishu.AppID)
		if e != nil {
			o.State = "listener-lock-conflict"
			o.Checks = append(o.Checks, check{"application.lock", "fail", true, e.Error()})
			emit(o)
			return 6
		}
		defer applicationLock.Close()
		lock, e := store.AcquireListener(cfg.Storage.DatabaseFile)
		if e != nil {
			o.State = "listener-lock-conflict"
			o.Checks = append(o.Checks, check{"deployment.lock", "fail", true, e.Error()})
			emit(o)
			return 6
		}
		defer lock.Close()
	}
	g := feishu.New(cfg.Feishu.AppID, secret)
	bot, err := g.Bot(callCtx)
	if err != nil {
		o.Checks = append(o.Checks, check{"feishu.bot", "fail", true, safeFeishu(err)})
		emit(o)
		return 4
	}
	o.Checks = append(o.Checks, check{"feishu.bot", "pass", true, map[string]string{"bot_open_id": bot}})
	db, err := store.Open(cfg.Storage.DatabaseFile)
	if err != nil {
		o.State = "storage-failed"
		emit(o)
		return 7
	}
	defer db.Close()
	if command == "m0-listen" {
		var bridge *app.M0
		if *thread != "" {
			bridge, err = app.NewM0(callCtx, cfg, c, g, db, *thread, *chat, bot)
			if err != nil {
				o.Checks = append(o.Checks, check{"m0.binding", "fail", true, err.Error()})
				emit(o)
				return 3
			}
		}
		return listenM0(ctx, cfg, g, db, &o, bridge)
	}
	return 2
}
func opStatus(s string) string {
	if s == "rejected" {
		return "fail"
	}
	if s == "confirmed" {
		return "pass"
	}
	return "pending"
}
func state(err error) string {
	var f *feishu.Rejected
	var c *codexclient.RPCError
	if errors.As(err, &f) || errors.As(err, &c) {
		return "rejected"
	}
	return "unknown"
}
func safeFeishu(err error) any {
	var f *feishu.Rejected
	if errors.As(err, &f) {
		return map[string]any{"code": f.Code, "http_status": f.HTTPStatus, "request_id": f.RequestID, "required_permissions": f.RequiredPermissions, "card_validation_detail": f.CardValidationDetail}
	}
	return map[string]string{"state": "unknown", "detail": "transport/authentication response unavailable; do not retry mutation blindly"}
}
