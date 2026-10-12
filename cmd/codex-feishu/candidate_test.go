package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
)

func candidateCommandOutput(t *testing.T, args []string) (int, output) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = previous; r.Close(); w.Close() }()
	code := run(context.Background(), args)
	w.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var result output
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal("invalid command envelope", err)
	}
	return code, result
}

func TestCandidateCLIRequiresExplicitPathsAndCannotEnableMigration(t *testing.T) {
	for _, args := range [][]string{
		{"setup", "--input", "/unused", "--plan", "--candidate-test"},
		{"setup", "--input", "/unused", "--plan", "--candidate-test", "--config", "/unused"},
		{"setup", "--input", "/unused", "--plan", "--database", "/unused"},
		{"setup", "migrate", "--config", "/unused", "--apply", "--plan-id", "unused", "--candidate-test"},
		{"service", "stop", "--config", "/unused", "--candidate-test"},
		{"service", "uninstall", "--config", "/unused", "--candidate-test"},
	} {
		code, result := candidateCommandOutput(t, args)
		if code != 2 || result.OK || result.State != "invalid-arguments" {
			t.Fatal(args, code, result.State)
		}
	}
	for _, action := range []string{"install", "start"} {
		args := []string{"service", action, "--config", "/unused"}
		if action == "install" {
			args = append(args, "--user")
		}
		code, result := candidateCommandOutput(t, args)
		if code != 3 || result.OK || result.State != "release-acceptance-pending" {
			t.Fatal("default release gate bypassed", action, code, result.State)
		}
	}
}

func TestCandidateServeRefusesLegacyBeforeNetworkOrMigration(t *testing.T) {
	path, cfg, _, _ := doctorFixture(t)
	createLegacyVerificationDB(t, cfg.Storage.DatabaseFile)
	before, err := os.ReadFile(cfg.Storage.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	code, result := candidateCommandOutput(t, []string{"serve", "--config", path, "--candidate-test"})
	if code != 3 || result.OK || result.State != "candidate-database-invalid" {
		t.Fatal("candidate serve did not stop before connections", code, result.State)
	}
	after, err := os.ReadFile(cfg.Storage.DatabaseFile)
	if err != nil || string(before) != string(after) {
		t.Fatal("candidate serve changed legacy database", err)
	}
}
