package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"

	"codex-feishu/internal/setup"
	"codex-feishu/internal/store"
)

func migrationOutput(t *testing.T, call func() int) (int, output) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = previous; r.Close(); w.Close() }()
	code := call()
	w.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var result output
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal("invalid command envelope", err)
	}
	if result.SchemaVersion != 1 || result.Command != "setup migrate" || result.OK || result.Checks == nil || result.NextSteps == nil {
		t.Fatal("incorrect migration envelope", result)
	}
	return code, result
}

func TestMigrationCLIRejectsInvalidArgumentsBeforeAccess(t *testing.T) {
	for _, args := range [][]string{
		{"setup", "migrate"},
		{"setup", "migrate", "--config", "relative", "--plan"},
		{"setup", "migrate", "--config", "/unused", "--plan", "--apply", "--plan-id", "unused"},
		{"setup", "migrate", "--config", "/unused", "--apply"},
		{"setup", "migrate", "--config", "/unused", "--plan", "--plan-id", "unused"},
		{"setup", "migrate", "--config", "/unused", "--plan", "extra"},
	} {
		code, result := migrationOutput(t, func() int { return run(context.Background(), args) })
		if code != 2 || result.State != "invalid-arguments" {
			t.Fatal(args, code, result.State)
		}
	}
}

func TestMigrationCLIClassifiesRecoveryAndConflicts(t *testing.T) {
	for _, tc := range []struct {
		err   error
		code  int
		state string
	}{
		{setup.ErrMigrationLocked, 6, "migration-conflict"},
		{setup.ErrStalePlan, 6, "migration-conflict"},
		{store.ErrConflict, 6, "migration-conflict"},
		{setup.ErrMigrationPending, 5, "migration-pending"},
		{store.ErrLegacyPending, 5, "migration-pending"},
		{context.Canceled, 5, "migration-pending"},
		{context.DeadlineExceeded, 5, "migration-pending"},
		{setup.ErrApplyPending, 5, "setup-recovery-pending"},
		{setup.ErrUnreleased, 3, "release-acceptance-pending"},
	} {
		code, result := migrationOutput(t, func() int { return migrationFailure(errors.Join(errors.New("fixture"), tc.err)) })
		if code != tc.code || result.State != tc.state {
			t.Fatal(tc.err, code, result.State)
		}
	}
}
