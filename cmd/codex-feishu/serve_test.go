package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/setup"
	"github.com/pelletier/go-toml/v2"
)

func TestServeRevalidatesConfigAndRefusesPendingSetupBeforeSideEffects(t *testing.T) {
	for _, kind := range []string{"unchanged", "owner", "prepared", "invalid-journal"} {
		t.Run(kind, func(t *testing.T) {
			path, cfg, _, _ := doctorFixture(t)
			if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := control.Identity(path, cfg)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "owner":
				cfg.Access.OwnerOpenID = "ou_changed_owner_valid"
				b, err := toml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "prepared":
				b, _ := json.Marshal(map[string]any{"Version": 0, "PlanID": strings.Repeat("a", 64), "SourceDigest": strings.Repeat("b", 64), "BeforeDigest": strings.Repeat("c", 64), "AfterDigest": strings.Repeat("d", 64), "State": "prepared"})
				if err := os.WriteFile(path+".setup-state.json", b, 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid-journal":
				if err := os.WriteFile(path+".setup-state.json", []byte("private corruption"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err = validateLockedServeConfig(path, identity)
			if kind == "unchanged" && err != nil || kind == "owner" && !errors.Is(err, control.ErrIdentity) || (kind == "prepared" || kind == "invalid-journal") && !errors.Is(err, setup.ErrApplyPending) {
				t.Fatal("incorrect startup guard", kind, err)
			}
			if _, err := os.Stat(cfg.Storage.DatabaseFile); !os.IsNotExist(err) {
				t.Fatal("startup guard created/migrated database", err)
			}
		})
	}
}
