package config

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/pelletier/go-toml/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	SchemaVersion int `toml:"schema_version"`
	Host          struct {
		ID string `toml:"id"`
	} `toml:"host"`
	Codex struct {
		CLIPath  string `toml:"cli_path"`
		Endpoint string `toml:"endpoint"`
	} `toml:"codex"`
	Feishu struct {
		AppID         string `toml:"app_id"`
		AppSecretFile string `toml:"app_secret_file"`
	} `toml:"feishu"`
	Access struct {
		OwnerOpenID     string   `toml:"owner_open_id"`
		OperatorOpenIDs []string `toml:"operator_open_ids"`
	} `toml:"access"`
	Groups struct {
		AdditionalMemberOpenIDs []string `toml:"additional_member_open_ids"`
	} `toml:"groups"`
	Discovery struct {
		Enabled             bool     `toml:"enabled"`
		FirstRun            string   `toml:"first_run"`
		AllowedRoots        []string `toml:"allowed_roots"`
		ScanIntervalSeconds int      `toml:"scan_interval_seconds"`
	} `toml:"discovery"`
	Notifications struct {
		TargetOpenIDs []string `toml:"target_open_ids"`
		Timezone      string   `toml:"timezone"`
		MentionOn     []string `toml:"mention_on"`
		QuietHours    string   `toml:"quiet_hours"`
	} `toml:"notifications"`
	Sync struct {
		CardUpdateMinIntervalMS int `toml:"card_update_min_interval_ms"`
	} `toml:"sync"`
	Creation struct {
		Enabled           bool     `toml:"enabled"`
		DefaultFollowMode string   `toml:"default_follow_mode"`
		WorkgroupChatIDs  []string `toml:"workgroup_chat_ids"`
	} `toml:"creation"`
	Storage struct {
		DatabaseFile string `toml:"database_file"`
	} `toml:"storage"`
}

func Load(path string) (Config, error) {
	var c Config
	if !filepath.IsAbs(path) {
		return c, errors.New("config path must be absolute")
	}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return c, err
	}
	err = toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields().Decode(&c)
	if err != nil {
		return c, errors.New("invalid config schema or unknown field")
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.SchemaVersion != 1 {
		return errors.New("unsupported config schema")
	}
	if c.Host.ID == "" || strings.Contains(c.Host.ID, "replace") {
		return errors.New("persistent host.id required")
	}
	if !strings.HasPrefix(c.Feishu.AppID, "cli_") || strings.Contains(c.Feishu.AppID, "replace") {
		return errors.New("valid feishu.app_id required")
	}
	if c.Codex.Endpoint != "auto" && c.Codex.Endpoint != "" {
		return errors.New("M0 requires endpoint=auto for shared daemon")
	}
	if c.Codex.CLIPath != "auto" && !filepath.IsAbs(c.Codex.CLIPath) {
		return errors.New("codex.cli_path must be absolute or auto")
	}
	if !filepath.IsAbs(c.Storage.DatabaseFile) || !filepath.IsAbs(c.Feishu.AppSecretFile) {
		return errors.New("database and secret paths must be absolute")
	}
	if !ValidOpenID(c.Access.OwnerOpenID) || !c.Operator(c.Access.OwnerOpenID) {
		return errors.New("owner must be a valid authorized operator")
	}
	ids := append(append(append([]string{}, c.Access.OperatorOpenIDs...), c.Groups.AdditionalMemberOpenIDs...), c.Notifications.TargetOpenIDs...)
	for _, id := range ids {
		if !ValidOpenID(id) {
			return errors.New("invalid open_id or placeholder")
		}
	}
	if c.Creation.Enabled {
		return errors.New("phase2 creation is not implemented")
	}
	if c.Discovery.FirstRun != "baseline" {
		return errors.New("first_run must be baseline")
	}
	if len(c.Discovery.AllowedRoots) == 0 {
		return errors.New("at least one allowed directory required")
	}
	for _, p := range c.Discovery.AllowedRoots {
		if !filepath.IsAbs(p) {
			return errors.New("allowed_roots must be absolute")
		}
		info, err := os.Stat(p)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("allowed directory unavailable: %s", p)
		}
	}
	if _, err := time.LoadLocation(c.Notifications.Timezone); err != nil {
		return errors.New("invalid notification timezone")
	}
	return nil
}
func ValidOpenID(s string) bool {
	return strings.HasPrefix(s, "ou_") && len(s) > 10 && !strings.Contains(s, "replace")
}
func (c Config) Operator(id string) bool {
	for _, p := range c.Access.OperatorOpenIDs {
		if id == p {
			return true
		}
	}
	return false
}
func (c Config) Allows(path string) bool {
	r, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	for _, root := range c.Discovery.AllowedRoots {
		a, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(a, r)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
func (c Config) Secret() (string, error) {
	info, err := os.Lstat(c.Feishu.AppSecretFile)
	if err != nil {
		return "", errors.New("secret file unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("secret must be a regular file with mode 0600")
	}
	b, err := os.ReadFile(c.Feishu.AppSecretFile)
	if err != nil {
		return "", errors.New("cannot read secret")
	}
	s := strings.TrimSpace(string(b))
	if s == "" || strings.ContainsAny(s, "\n\r") {
		return "", errors.New("secret file must contain one nonempty value")
	}
	return s, nil
}
