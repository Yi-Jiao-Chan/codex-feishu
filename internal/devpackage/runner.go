// Package devpackage builds deterministic, explicitly allowlisted development
// archives for codex-feishu.
package devpackage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strings"
)

// BuildRequest is the complete input to the build dependency.
type BuildRequest struct {
	Repository string
	BinaryPath string
	Commit     string
}

// GitState is the source snapshot observed by InspectGit. SourceHashes uses
// repository-relative source paths and is populated by CommandRunner.
type GitState struct {
	Commit       string
	SourceHashes map[string]string
}

// VersionData is the data object emitted by the built bridge's version
// command. Raw JSON values keep the observed representation intact while
// encoding/json still gives deterministic map-key ordering when packaging.
type VersionData map[string]json.RawMessage

// VersionInfo is the validated result of exactly one `version --json` query.
// Data is the command's data object; the scalar fields are normalized copies
// used for acceptance checks and generated compatibility metadata.
type VersionInfo struct {
	SchemaVersion int
	Command       string
	OK            bool
	Data          VersionData

	Version        string
	Commit         string
	Phase1Released bool
	ConfigSchema   int
	CodexProtocol  string
	SDK            string
}

// Runner is deliberately small so archive tests can use a temporary fake
// compiler and version command without replacing process-global behavior.
type Runner interface {
	Build(context.Context, BuildRequest) error
	InspectGit(context.Context, string) (GitState, error)
	Version(context.Context, string) (VersionInfo, error)
}

// CommandRunner invokes git, go, and the produced binary directly. It never
// invokes a shell and leaves the inherited environment untouched.
type CommandRunner struct{}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (CommandRunner) Build(ctx context.Context, req BuildRequest) error {
	repo := req.Repository
	binary := req.BinaryPath
	if repo == "" || binary == "" || !commitPattern.MatchString(req.Commit) {
		return failure("build-failed")
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", "-X main.commit="+req.Commit, "-o", binary, "./cmd/codex-feishu")
	cmd.Dir = repo
	// cmd.Env intentionally remains nil: Go inherits the caller's environment,
	// including its GOCACHE and GOMODCACHE settings.
	if err := cmd.Run(); err != nil {
		return failure("build-failed")
	}
	return nil
}

func (CommandRunner) InspectGit(ctx context.Context, repo string) (GitState, error) {
	commit, err := gitOutput(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return GitState{}, failure("source-inspection-failed")
	}
	commit = strings.TrimSpace(commit)
	if !commitPattern.MatchString(commit) {
		return GitState{}, failure("source-inspection-failed")
	}
	status, err := gitOutput(ctx, repo, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return GitState{}, failure("source-inspection-failed")
	}
	if strings.TrimSpace(status) != "" {
		return GitState{}, failure("dirty-source")
	}
	hashes, err := sourceHashes(repo)
	if err != nil {
		return GitState{}, preserveFailure("invalid-source", err)
	}
	return GitState{Commit: commit, SourceHashes: hashes}, nil
}

func (CommandRunner) Version(ctx context.Context, binary string) (VersionInfo, error) {
	cmd := exec.CommandContext(ctx, binary, "version", "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Do not return the command's output: a future implementation must not
		// allow a version failure to disclose credentials or user data.
		return VersionInfo{}, failure("invalid-artifact")
	}
	return parseVersion(stdout.Bytes())
}

func gitOutput(ctx context.Context, repo string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", repo}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", errors.New("git command failed")
	}
	return stdout.String(), nil
}

func parseVersion(raw []byte) (VersionInfo, error) {
	var envelope struct {
		SchemaVersion int         `json:"schema_version"`
		Command       string      `json:"command"`
		OK            *bool       `json:"ok"`
		Data          VersionData `json:"data"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&envelope); err != nil || envelope.OK == nil {
		return VersionInfo{}, failure("invalid-artifact")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return VersionInfo{}, failure("invalid-artifact")
	}
	if !*envelope.OK {
		return VersionInfo{SchemaVersion: envelope.SchemaVersion, Command: envelope.Command, OK: false, Data: envelope.Data}, failure("invalid-artifact")
	}
	info := VersionInfo{
		SchemaVersion: envelope.SchemaVersion,
		Command:       envelope.Command,
		OK:            *envelope.OK,
		Data:          envelope.Data,
	}
	info.Version, _ = dataString(info.Data, "version")
	info.Commit, _ = dataString(info.Data, "commit")
	info.CodexProtocol, _ = dataString(info.Data, "codex_protocol")
	info.SDK, _ = dataString(info.Data, "sdk")
	info.Phase1Released, _ = dataBool(info.Data, "phase1_released")
	info.ConfigSchema, _ = dataInt(info.Data, "config_schema")
	return info, nil
}

func dataString(data VersionData, key string) (string, error) {
	raw, ok := data[key]
	if !ok {
		return "", errors.New("missing version field")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("invalid version string")
	}
	return value, nil
}

func dataBool(data VersionData, key string) (bool, error) {
	raw, ok := data[key]
	if !ok {
		return false, errors.New("missing version field")
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, errors.New("invalid version boolean")
	}
	return value, nil
}

func dataInt(data VersionData, key string) (int, error) {
	raw, ok := data[key]
	if !ok {
		return 0, errors.New("missing version field")
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, errors.New("invalid version integer")
	}
	return value, nil
}
