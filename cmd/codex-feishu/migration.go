package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"time"

	"codex-feishu/internal/setup"
	"codex-feishu/internal/store"
)

func runMigration(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("setup migrate", flag.ContinueOnError)
	path := flags.String("config", "", "absolute existing M0 TOML config path")
	preview := flags.Bool("plan", false, "save a private migration review without backup or schema writes")
	apply := flags.Bool("apply", false, "backup and apply the exact reviewed M0 migration")
	id := flags.String("plan-id", "", "reviewed migration ID")
	_ = flags.Bool("json", false, "JSON output (default)")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || !filepath.IsAbs(*path) || *preview == *apply || *preview && *id != "" || *apply && *id == "" {
		emit(output{Command: "setup migrate", State: "invalid-arguments"})
		return 2
	}
	binary, err := os.Executable()
	if err != nil {
		emit(output{Command: "setup migrate", State: "artifact-unavailable"})
		return 3
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	planner := setup.MigrationPlanner{}
	if *preview {
		plan, err := planner.Preview(ctx, *path, binary)
		if err != nil {
			return migrationFailure(err)
		}
		state, code := "migration-plan-ready", 0
		checks := []check{{"migration.evidence", "pass", true, "quiescent M0 journals, complete native reads, allowed directory and verified group members"}}
		if !plan.CanApply {
			state, code = "migration-plan-needs-release", 3
			checks = append(checks, check{"release.acceptance", "pending", true, "development artifact cannot apply a real deployment migration"})
		}
		emit(output{Command: "setup migrate", OK: plan.CanApply, State: state, PlanID: plan.ID, Data: plan, Checks: checks, Paths: map[string]string{"plan_file": plan.DatabaseFile + ".migration-state.json"}, NextSteps: []string{"stop only the existing bridge before applying the reviewed plan; do not stop the Codex daemon", "apply creates a private SQLite backup and adopts existing routes; it does not start a service or send messages"}})
		return code
	}
	result, err := planner.Apply(ctx, *path, binary, *id)
	if err != nil {
		return migrationFailure(err)
	}
	emit(output{Command: "setup migrate", OK: true, State: result.State, PlanID: result.PlanID, Data: result, NextSteps: []string{"database adoption completed only; inspect/start the bridge and verify its real App/Feishu behavior separately"}})
	return 0
}

func migrationFailure(err error) int {
	state, code := "migration-prerequisite-failed", 3
	switch {
	case errors.Is(err, setup.ErrMigrationLocked), errors.Is(err, setup.ErrStalePlan), errors.Is(err, store.ErrConflict):
		state, code = "migration-conflict", 6
	case errors.Is(err, setup.ErrMigrationPending), errors.Is(err, store.ErrLegacyPending), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		state, code = "migration-pending", 5
	case errors.Is(err, setup.ErrUnreleased):
		state = "release-acceptance-pending"
	case errors.Is(err, setup.ErrApplyPending):
		state, code = "setup-recovery-pending", 5
	}
	emit(output{Command: "setup migrate", State: state, Checks: []check{{"migration", "fail", true, err.Error()}}})
	return code
}
