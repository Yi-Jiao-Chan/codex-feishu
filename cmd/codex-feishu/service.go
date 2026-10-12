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
	candidate := flags.Bool("candidate-test", false, "authorize candidate install/start without marking a release")
	_ = flags.Bool("json", false, "JSON output (default)")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 || *path == "" || action == "install" && !*user || !*keep || *candidate && action != "install" && action != "start" {
		emit(output{Command: "service", State: "invalid-arguments"})
		return 2
	}
	if (action == "install" || action == "start") && !phase1Released && !*candidate {
		emit(output{Command: "service." + action, State: "release-acceptance-pending", NextSteps: []string{"phase1 service release acceptance is pending; development serve remains available"}})
		return 3
	}
	opts := userservice.Options{CandidateTest: *candidate}
	var binary string
	if action == "install" || *candidate {
		var err error
		binary, err = os.Executable()
		if err == nil {
			binary, err = filepath.EvalSymlinks(binary)
		}
		if err != nil {
			emit(output{Command: "service", State: "binary-unavailable"})
			return 3
		}
		opts.CandidateBinaryPath = binary
	}
	manager, err := userservice.New(opts)
	if err != nil {
		emit(output{Command: "service", State: "user-manager-unavailable"})
		return 3
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result userservice.Result
	switch action {
	case "install":
		result = manager.Install(ctx, userservice.Request{BinaryPath: binary, ConfigPath: *path})
	case "start":
		result = manager.Start(ctx, *path)
	case "stop":
		result = manager.Stop(ctx, *path)
	case "uninstall":
		result = manager.Uninstall(ctx, *path)
	}
	o := output{Command: "service." + action, OK: result.OK, State: result.State, Data: result}
	if *candidate && !phase1Released {
		o.Checks = append(o.Checks, check{"release.acceptance", "pending", false, "candidate service operation only; phase1 acceptance remains incomplete"})
	}
	for _, c := range result.Checks {
		o.Checks = append(o.Checks, check{c.ID, c.Status, c.Required, map[string]string{"message": c.Detail, "remediation": c.Remediation}})
	}
	emit(o)
	return result.Code
}
