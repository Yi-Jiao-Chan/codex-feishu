package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"

	"codex-feishu/internal/devpackage"
)

type cliResult struct {
	State         string `json:"state"`
	Commit        string `json:"commit,omitempty"`
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
	ArchiveName   string `json:"archive_name,omitempty"`
	Noop          bool   `json:"noop"`
}

func main() {
	os.Exit(run())
}

func run() int {
	flags := flag.NewFlagSet("dev-package", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	output := flags.String("output", "", "absolute output directory")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(os.Args[1:]); err != nil || flags.NArg() != 0 || *output == "" {
		emit(cliResult{State: "invalid-arguments"})
		return 2
	}
	// The command is intentionally JSON-only even when --json is omitted. The
	// flag remains accepted as part of the stable invocation contract.
	_ = jsonOutput
	cwd, err := os.Getwd()
	if err != nil {
		emit(cliResult{State: devpackage.FailureState(err)})
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result, err := devpackage.Build(ctx, devpackage.Options{Repository: cwd, Output: *output})
	if err != nil {
		// Error details intentionally stay out of stdout. In particular, version
		// command output can contain data that must never become package output.
		emit(cliResult{State: devpackage.FailureState(err)})
		return 1
	}
	emit(cliResult{State: result.State, Commit: result.Commit, ArchiveSHA256: result.ArchiveSHA256, ArchiveName: result.ArchiveName, Noop: result.Noop})
	return 0
}

func emit(result cliResult) {
	_ = json.NewEncoder(os.Stdout).Encode(result)
}
