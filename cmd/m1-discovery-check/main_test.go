package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeRefusesDeploymentDatabaseAndAliasesBeforeConnecting(t *testing.T) {
	base := t.TempDir()
	database := filepath.Join(base, "deployment.db")
	if err := os.WriteFile(database, []byte("existing deployment sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias.db")
	if err := os.Symlink(database, alias); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`schema_version = 1
[host]
id = "fixture-host"
[codex]
cli_path = "auto"
endpoint = "auto"
[feishu]
app_id = "cli_fixture"
app_secret_file = %q
[access]
owner_open_id = "ou_fixture_owner"
operator_open_ids = ["ou_fixture_owner"]
[discovery]
enabled = false
first_run = "baseline"
allowed_roots = [%q]
[notifications]
timezone = "Asia/Shanghai"
[storage]
database_file = %q
`, filepath.Join(base, "unused-secret"), base, database)
	configPath := filepath.Join(base, "config.toml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{database, alias} {
		if code := run(context.Background(), []string{"--config", configPath, "--database", path}); code != 2 {
			t.Fatalf("deployment DB accepted: %d", code)
		}
	}
	contents, err := os.ReadFile(database)
	if err != nil || string(contents) != "existing deployment sentinel" {
		t.Fatal("production database was opened or changed")
	}
}
