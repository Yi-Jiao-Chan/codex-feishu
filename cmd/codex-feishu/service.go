package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"time"

	"codex-feishu/internal/userservice"
)

func runService(ctx context.Context, args []string) int {
	if len(args) == 0 {
		emit(output{Command: "service", State: "action-required"})
		return 2
	}
	action := args[0]
	if action != "install" && action != "start" && action != "stop" && action != "uninstall" {
		emit(output{Command: "service", State: "unsupported-action"})
		return 2
	}
	flags := flag.NewFlagSet("service", flag.ContinueOnError)
	path := flags.String("config", "", "absolute config path")
	user := flags.Bool("user", false, "install current user's service")
	keep := flags.Bool("keep-data", true, "preserve config, credentials and database")
	_ = flags.Bool("json", false, "JSON output (default)")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 || *path == "" || action == "install" && !*user || !*keep {
		emit(output{Command: "service", State: "invalid-arguments"})
		return 2
	}
	if (action == "install" || action == "start") && !phase1Released {
		emit(output{Command: "service." + action, State: "release-acceptance-pending", NextSteps: []string{"phase1 service release acceptance is pending; development serve remains available"}})
		return 3
	}
	manager, err := userservice.New(userservice.Options{})
	if err != nil {
		emit(output{Command: "service", State: "user-manager-unavailable"})
		return 3
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result userservice.Result
	switch action {
	case "install":
		binary, err := os.Executable()
		if err != nil {
			emit(output{Command: "service", State: "binary-unavailable"})
			return 3
		}
		binary, err = filepath.EvalSymlinks(binary)
		if err != nil {
			emit(output{Command: "service", State: "binary-unavailable"})
			return 3
		}
		result = manager.Install(ctx, userservice.Request{BinaryPath: binary, ConfigPath: *path})
	case "start":
		result = manager.Start(ctx, *path)
	case "stop":
		result = manager.Stop(ctx, *path)
	case "uninstall":
		result = manager.Uninstall(ctx, *path)
	}
	o := output{Command: "service." + action, OK: result.OK, State: result.State, Data: result}
	for _, c := range result.Checks {
		o.Checks = append(o.Checks, check{c.ID, c.Status, c.Required, map[string]string{"message": c.Detail, "remediation": c.Remediation}})
	}
	emit(o)
	return result.Code
}
