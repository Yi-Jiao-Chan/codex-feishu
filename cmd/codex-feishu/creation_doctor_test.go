package main

import (
	"codex-feishu/internal/codexclient"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestDoctorReportsCreationCountsWithoutReadinessOrIDs(t *testing.T) {
	path, cfg, deps, _ := doctorFixture(t)
	discover := deps.discover
	deps.discover = func(ctx context.Context, path string) (codexclient.Daemon, error) {
		d, err := discover(ctx, path)
		d.AppServerVersion = "0.162.1"
		return d, err
	}
	cfg.Creation.Enabled = true
	cfg.Creation.TopicsEnabled = true
	cfg.Creation.EntryChatIDs = []string{"oc_entry_secret"}
	cfg.Creation.WorkgroupChatIDs = []string{"oc_workgroup_secret"}
	cfg.Creation.AllowedRoots = []string{cfg.Discovery.AllowedRoots[0]}
	cfg.Creation.CommonDirectories = []string{cfg.Discovery.AllowedRoots[0]}
	b, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}

	o, code := inspectDoctor(context.Background(), path, false, true, deps)
	if code != 0 || !o.OK {
		t.Fatalf("doctor failed with valid creation policy: code=%d output=%+v", code, o)
	}
	data, ok := o.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected doctor data: %#v", o.Data)
	}
	creation, ok := data["creation"].(map[string]any)
	if !ok {
		t.Fatalf("missing creation diagnostics: %#v", data)
	}
	for key, want := range map[string]any{
		"enabled":                  true,
		"topics_enabled":           true,
		"entry_chat_ids_count":     1,
		"workgroup_chat_ids_count": 1,
		"allowed_roots_count":      1,
		"common_directories_count": 1,
		"ui_verified":              false,
	} {
		if creation[key] != want {
			t.Fatalf("creation diagnostic %s=%v, want %v", key, creation[key], want)
		}
	}
	encoded, _ := json.Marshal(o)
	for _, forbidden := range []string{"oc_entry_secret", "oc_workgroup_secret", `"ready":true`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("doctor leaked creation data or readiness: %s", encoded)
		}
	}
	var found bool
	for _, check := range o.Checks {
		if check.ID == "creation.live_ui" {
			found = check.Status == "pending" && !check.Required
		}
	}
	if !found {
		t.Fatalf("creation UI was not left pending: %+v", o.Checks)
	}
}
