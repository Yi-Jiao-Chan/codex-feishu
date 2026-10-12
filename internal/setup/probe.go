package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"

	"codex-feishu/internal/codexclient"
)

type Capabilities struct {
	Version       string   `json:"version"`
	Commit        string   `json:"commit"`
	ConfigSchema  int      `json:"config_schema"`
	Commands      []string `json:"commands"`
	Released      bool     `json:"phase1_released"`
	CodexProtocol string   `json:"codex_protocol"`
	Capabilities  struct {
		CandidateDeployment bool `json:"candidate_deployment"`
	} `json:"capabilities"`
}

type Environment struct {
	CLIPath  string             `json:"cli_path"`
	Daemon   codexclient.Daemon `json:"daemon"`
	Artifact Capabilities       `json:"artifact"`
}

func Probe(ctx context.Context, binary string) (Environment, error) {
	var env Environment
	b, err := exec.CommandContext(ctx, binary, "version", "--json").Output()
	if err != nil || len(b) > 1<<20 {
		return env, errors.New("artifact version inspection failed")
	}
	var version struct {
		OK   bool         `json:"ok"`
		Data Capabilities `json:"data"`
	}
	if json.Unmarshal(b, &version) != nil || !version.OK || version.Data.ConfigSchema != 1 {
		return env, errors.New("artifact capabilities are unsupported")
	}
	env.Artifact = version.Data
	cli, err := exec.LookPath("codex")
	if err != nil {
		return env, errors.New("Codex CLI unavailable")
	}
	env.CLIPath, err = filepath.Abs(cli)
	if err != nil {
		return env, errors.New("Codex CLI path unavailable")
	}
	env.CLIPath, err = filepath.EvalSymlinks(env.CLIPath)
	if err != nil {
		return env, errors.New("Codex CLI path unavailable")
	}
	env.Daemon, err = codexclient.Discover(ctx, env.CLIPath)
	if err != nil {
		return env, errors.New("existing Codex App daemon unavailable")
	}
	return env, nil
}
