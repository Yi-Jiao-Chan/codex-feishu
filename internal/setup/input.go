// Package setup builds reviewable configuration plans and applies only the
// requested local configuration. It never starts services or sends messages.
package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"codex-feishu/internal/config"
)

type Input struct {
	SchemaVersion int `json:"schema_version"`
	Artifact      struct {
		BinaryPath string `json:"binary_path"`
	} `json:"artifact"`
	Feishu struct {
		AppID         string `json:"app_id"`
		AppSecretFile string `json:"app_secret_file"`
	} `json:"feishu"`
	Identity struct {
		OwnerOpenID         *string  `json:"owner_open_id"`
		PairOwner           bool     `json:"pair_owner"`
		OperatorOpenIDs     []string `json:"operator_open_ids"`
		NotificationOpenIDs []string `json:"notification_open_ids"`
		GroupMemberOpenIDs  []string `json:"group_member_open_ids"`
	} `json:"identity"`
	Discovery struct {
		AllowedRoots []string `json:"allowed_roots"`
		AutoGroup    *bool    `json:"auto_group"`
	} `json:"discovery"`
	Notifications struct {
		Timezone   string  `json:"timezone"`
		QuietHours *string `json:"quiet_hours"`
	} `json:"notifications"`
	FilePreview *config.FilePreviewConfig `json:"file_preview,omitempty"`
	Features    struct {
		Phase    string `json:"phase"`
		Creation bool   `json:"creation"`
	} `json:"features"`
	Service struct {
		Mode string `json:"mode"`
	} `json:"service"`
	Verification struct {
		SendTestCardToOwner    bool `json:"send_test_card_to_owner"`
		CreatePrivateTestGroup bool `json:"create_private_test_group"`
		CreateCodexTestThread  bool `json:"create_codex_test_thread"`
		BindTestGroupAndThread bool `json:"bind_test_group_and_thread"`
	} `json:"verification"`
}

func Decode(r io.Reader) (Input, error) {
	var in Input
	b, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return in, errors.New("setup input unavailable or too large")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, errors.New("invalid setup input schema")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return in, errors.New("setup input must contain one JSON object")
	}
	if in.SchemaVersion != 1 {
		return in, errors.New("unsupported setup input schema")
	}
	return in, nil
}

func LoadInput(path string) (Input, error) {
	if !filepath.IsAbs(path) {
		return Input{}, errors.New("setup input path must be absolute")
	}
	f, err := os.Open(path)
	if err != nil {
		return Input{}, errors.New("setup input unavailable")
	}
	defer f.Close()
	return Decode(f)
}

type Paths struct {
	ConfigFile   string `json:"config_file"`
	DatabaseFile string `json:"database_file"`
}

func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return Paths{}, errors.New("user home unavailable")
	}
	c, d := os.Getenv("XDG_CONFIG_HOME"), os.Getenv("XDG_DATA_HOME")
	if c == "" {
		c = filepath.Join(home, ".config")
	}
	if d == "" {
		d = filepath.Join(home, ".local", "share")
	}
	if !filepath.IsAbs(c) || !filepath.IsAbs(d) {
		return Paths{}, errors.New("XDG paths must be absolute")
	}
	return Paths{ConfigFile: filepath.Join(c, "codex-feishu", "config.toml"), DatabaseFile: filepath.Join(d, "codex-feishu", "bridge.db")}, nil
}
