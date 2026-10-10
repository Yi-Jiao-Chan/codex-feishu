package main

import (
	"codex-feishu/internal/app"
	"codex-feishu/internal/cards"
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
		fmt.Fprintln(os.Stderr, "usage: codex-feishu version | doctor | verify | m0-listen (development, not phase1 release)")
		return 2
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	cfgPath := flags.String("config", "", "absolute TOML config path")
	_ = flags.Bool("json", false, "JSON output (default)")
	codexOnly := flags.Bool("codex-only", false, "doctor: check shared daemon without Feishu config")
	allowCard := flags.Bool("allow-test-card", false, "verify: send one card to configured owner")
	allowGroup := flags.Bool("allow-test-group", false, "verify: create one private owner+bot test group")
	target := flags.String("target", "self", "verify supports only self")
	thread := flags.String("thread", "", "m0-listen: explicit App test thread ID")
	chat := flags.String("chat", "", "m0-listen: previously verified test group ID")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 {
		return 2
	}
	if command == "version" {
		emit(output{Command: command, OK: true, State: "m0-development", Data: map[string]any{"version": "0.0.1-dev", "commit": commit, "config_schema": 1, "commands": []string{"version", "doctor", "verify", "m0-listen"}, "phase1_released": false, "sdk": "github.com/larksuite/oapi-sdk-go/v3@v3.12.0", "codex_protocol": "0.162.0", "real_app_ui_verified": false, "real_feishu_verified": false}})
		return 0
	}
	if command != "doctor" && command != "verify" && command != "m0-listen" {
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
	if *codexOnly && command == "doctor" {
		o.OK = true
		o.State = "codex-readonly-ready"
		emit(o)
		return 0
	}
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
	if command == "doctor" {
		o.OK = true
		o.State = "readonly-ready-ui-unverified"
		o.NextSteps = []string{"verify live Feishu long connection, private group, card callback and App UI"}
		emit(o)
		return 0
	}
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
	if *target != "self" {
		return 2
	}
	if *allowCard {
		key := "m0-test-card"
		fresh, e := db.Begin(ctx, key, "feishu.send_card")
		if e != nil {
			return 7
		}
		if fresh {
			token := store.ID()
			if _, e = db.Begin(ctx, "m0-test-card-token", "action-token"); e != nil {
				return 7
			}
			if e = db.Finish(ctx, "m0-test-card-token", "confirmed", token); e != nil {
				return 7
			}
			id, e := g.SendCard(callCtx, "open_id", cfg.Access.OwnerOpenID, cards.Probe(token), store.ID())
			if e != nil {
				db.Finish(ctx, key, state(e), "")
				o.Checks = append(o.Checks, check{"feishu.test_card", "fail", true, safeFeishu(e)})
			} else {
				if e = db.Finish(ctx, key, "confirmed", id); e != nil {
					return 7
				}
				o.Checks = append(o.Checks, check{"feishu.test_card", "pass", true, map[string]string{"message_id": id}})
			}
		} else {
			st, id, e := db.Operation(ctx, key)
			if e != nil {
				return 7
			}
			o.Checks = append(o.Checks, check{"feishu.test_card", opStatus(st), true, map[string]string{"state": st, "message_id": id, "replayed": "true"}})
		}
	} else {
		o.Checks = append(o.Checks, check{"feishu.test_card", "skipped", false, nil})
	}
	if *allowGroup {
		key := "m0-test-group"
		fresh, e := db.Begin(ctx, key, "feishu.create_group")
		if e != nil {
			return 7
		}
		if fresh {
			id, e := g.CreateGroup(callCtx, "Codex Feishu · M0 联调", cfg.Access.OwnerOpenID, []string{cfg.Access.OwnerOpenID}, store.ID())
			if e != nil {
				db.Finish(ctx, key, state(e), "")
				o.Checks = append(o.Checks, check{"feishu.test_group", "fail", true, safeFeishu(e)})
			} else {
				if e = db.Finish(ctx, key, "confirmed", id); e != nil {
					return 7
				}
				o.Checks = append(o.Checks, check{"feishu.test_group", "pass", true, map[string]string{"chat_id": id}})
			}
		} else {
			st, id, e := db.Operation(ctx, key)
			if e != nil {
				return 7
			}
			o.Checks = append(o.Checks, check{"feishu.test_group", opStatus(st), true, map[string]string{"state": st, "chat_id": id, "replayed": "true"}})
		}
	} else {
		o.Checks = append(o.Checks, check{"feishu.test_group", "skipped", false, nil})
	}
	o.Checks = append(o.Checks, check{"m0.app_ui", "pending", true, "full App-origin queue/steer and remaining UI gate require live observation"})
	callbackState, callbackID, callbackErr := db.Operation(ctx, "m0.card_callback")
	if callbackErr == nil && callbackState == "confirmed" {
		o.Checks = append(o.Checks, check{"m0.card_callback", "pass", true, map[string]string{"message_id": callbackID, "source": "persisted actual owner callback"}})
	} else {
		o.Checks = append(o.Checks, check{"m0.card_callback", "pending", true, "click test card while m0-listen is running"})
	}
	o.State = "m0-pending"
	o.NextSteps = []string{"start m0-listen, then click the private test card; complete App round-trip validation"}
	emit(o)
	return 5
}
func opStatus(s string) string {
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
