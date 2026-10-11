package main

import (
	"context"
	"errors"
	"flag"
	"time"

	"codex-feishu/internal/setup"
)

func runSetup(ctx context.Context, args []string) int {
	if len(args) > 0 && args[0] == "pair" {
		return runPair(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "migrate" {
		return runMigration(ctx, args[1:])
	}
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	input := flags.String("input", "", "absolute setup JSON path")
	preview := flags.Bool("plan", false, "preview local configuration changes")
	apply := flags.Bool("apply", false, "apply a reviewed plan without starting the service")
	id := flags.String("plan-id", "", "reviewed plan ID")
	configPath := flags.String("config", "", "optional absolute target config path")
	_ = flags.Bool("json", false, "JSON output (default)")
	if err := flags.Parse(args); err != nil || len(flags.Args()) > 0 || *input == "" || *preview == *apply || *preview && *id != "" || *apply && *id == "" {
		emit(output{Command: "setup", State: "invalid-arguments"})
		return 2
	}
	in, err := setup.LoadInput(*input)
	if err != nil {
		emit(output{Command: "setup", State: "invalid-input", Checks: []check{{"input", "fail", true, err.Error()}}})
		return 2
	}
	paths, err := setup.DefaultPaths()
	if err != nil {
		emit(output{Command: "setup", State: "paths-unavailable"})
		return 3
	}
	if *configPath != "" {
		paths.ConfigFile = *configPath
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	planner := setup.Planner{}
	if *preview {
		plan, err := planner.Build(ctx, in, paths)
		if err != nil {
			return setupFailure(err)
		}
		state, code := "plan-ready", 0
		if !plan.CanApply {
			state, code = "plan-needs-prerequisites", 3
		}
		checks := []check{{"setup.configuration", "pass", true, "validated paths, identity, artifact and existing native daemon"}}
		if !plan.Environment.Artifact.Released {
			checks = append(checks, check{"release.acceptance", "pending", true, "phase1 acceptance remains incomplete"})
		}
		if len(plan.PolicyChanges) > 0 {
			checks = append(checks, check{"setup.notification_apply", "pass", true, "explicit existing policy changes isolated in the plan; apply uses locked revision CAS and a recoverable journal"})
		}
		emit(output{Command: "setup", OK: plan.CanApply, State: state, Data: plan, PlanID: plan.ID, Paths: plan.Paths, Checks: checks, NextSteps: plan.NextSteps})
		return code
	}
	result, err := planner.Apply(ctx, in, paths, *id)
	if err != nil {
		return setupFailure(err)
	}
	emit(output{Command: "setup", OK: true, State: result.State, Data: result, PlanID: result.PlanID, Paths: map[string]string{"config_file": result.ConfigFile}, NextSteps: []string{"configuration saved only; install/start the bridge and complete real acceptance separately"}})
	return 0
}

func setupFailure(err error) int {
	state, code := "setup-prerequisite-failed", 3
	switch {
	case errors.Is(err, setup.ErrStalePlan):
		state, code = "plan-stale", 6
	case errors.Is(err, setup.ErrNeedsIdentity):
		state = "needs-identity"
	case errors.Is(err, setup.ErrUnreleased):
		state = "release-acceptance-pending"
	case errors.Is(err, setup.ErrApplyPending):
		state, code = "setup-recovery-pending", 5
	case errors.Is(err, setup.ErrMigrationPending):
		state, code = "migration-recovery-pending", 5
	}
	emit(output{Command: "setup", State: state, Checks: []check{{"setup", "fail", true, err.Error()}}})
	return code
}
