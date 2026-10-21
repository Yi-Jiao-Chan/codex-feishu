package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeLoadFixture(t *testing.T, interactions string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	body := fmt.Sprintf(`schema_version = 1

[host]
id = "host-load-test"

[codex]
cli_path = "auto"
endpoint = "auto"

[feishu]
app_id = "cli_load_test"
app_secret_file = %q

[access]
owner_open_id = "ou_load_owner"
operator_open_ids = ["ou_load_owner"]

[discovery]
first_run = "baseline"
allowed_roots = [%q]

[notifications]
timezone = "Asia/Shanghai"

[storage]
database_file = %q

%s
`, filepath.Join(root, "secret"), root, filepath.Join(root, "bridge.db"), interactions)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadUsesInteractionDefaultsAndPreservesExplicitValues(t *testing.T) {
	defaultTypes := []string{"question", "async_question"}
	for _, test := range []struct {
		name         string
		interactions string
		wantEnabled  bool
		wantTypes    []string
		wantErr      bool
	}{
		{name: "omitted", wantEnabled: true, wantTypes: defaultTypes},
		{name: "partial-enabled", interactions: "[interactions]\nenabled = true", wantEnabled: true, wantTypes: defaultTypes},
		{name: "explicit-off", interactions: "[interactions]\nenabled = false", wantEnabled: false, wantTypes: defaultTypes},
		{name: "explicit-off-with-list", interactions: "[interactions]\nenabled = false\nenabled_types = [\"command\"]", wantEnabled: false, wantTypes: []string{"command"}},
		{name: "explicit-off-with-empty-list", interactions: "[interactions]\nenabled = false\nenabled_types = []", wantEnabled: false, wantTypes: []string{}},
		{name: "question-only", interactions: "[interactions]\nenabled_types = [\"question\"]", wantEnabled: true, wantTypes: []string{"question"}},
		{name: "command-and-question", interactions: "[interactions]\nenabled_types = [\"command\", \"question\"]", wantEnabled: true, wantTypes: []string{"command", "question"}},
		{name: "empty-while-enabled", interactions: "[interactions]\nenabled = true\nenabled_types = []", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, err := Load(writeLoadFixture(t, test.interactions))
			if test.wantErr {
				if err == nil {
					t.Fatal("explicit empty enabled_types was accepted while interactions are enabled")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Interactions.Enabled != test.wantEnabled || !reflect.DeepEqual(c.Interactions.EnabledTypes, test.wantTypes) {
				t.Fatalf("interactions = %#v, want enabled=%v types=%v", c.Interactions, test.wantEnabled, test.wantTypes)
			}
		})
	}
}

func validTestConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	var c Config
	c.SchemaVersion = 1
	c.Host.ID = "persistent-host"
	c.Codex.CLIPath = "auto"
	c.Feishu.AppID = "cli_test_application"
	c.Feishu.AppSecretFile = filepath.Join(root, "secret")
	c.Access.OwnerOpenID = "ou_authorized_owner"
	c.Access.OperatorOpenIDs = []string{c.Access.OwnerOpenID}
	c.Discovery.FirstRun = "baseline"
	c.Discovery.AllowedRoots = []string{root}
	c.Notifications.Timezone = "Asia/Shanghai"
	c.Notifications.MentionOn = []string{"completed", "failed", "needs_user"}
	c.Storage.DatabaseFile = filepath.Join(root, "bridge.db")
	return c
}

func TestValidateRejectsUnsafeIdentityAndInvalidOperationalSettings(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Config)
	}{
		{"mention-all", func(c *Config) { c.Notifications.TargetOpenIDs = []string{"all"} }},
		{"mention-markup", func(c *Config) { c.Notifications.TargetOpenIDs = []string{`ou_owner<at>`} }},
		{"application-path", func(c *Config) { c.Feishu.AppID = "cli_../../file" }},
		{"application-empty-suffix", func(c *Config) { c.Feishu.AppID = "cli_" }},
		{"quiet-equal", func(c *Config) { c.Notifications.QuietHours = "23:00-23:00" }},
		{"quiet-malformed", func(c *Config) { c.Notifications.QuietHours = "23:00-08:00-extra" }},
		{"progress-notification", func(c *Config) { c.Notifications.MentionOn = []string{"progress"} }},
		{"negative-scan", func(c *Config) { c.Discovery.ScanIntervalSeconds = -1 }},
		{"too-frequent-update", func(c *Config) { c.Sync.CardUpdateMinIntervalMS = 99 }},
		{"phase2", func(c *Config) { c.Creation.Enabled = true }},
		{"interaction-without-types", func(c *Config) { c.Interactions.Enabled = true }},
		{"unknown-interaction-type", func(c *Config) { c.Interactions.EnabledTypes = []string{"approve_everything"} }},
		{"duplicate-interaction-type", func(c *Config) { c.Interactions.EnabledTypes = []string{"question", "question"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := validTestConfig(t)
			if err := c.Validate(); err != nil {
				t.Fatal("valid fixture rejected", err)
			}
			test.edit(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid settings accepted")
			}
		})
	}
}

func TestValidateDoesNotApplyInteractionDefaults(t *testing.T) {
	c := validTestConfig(t)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Interactions.Enabled || c.Interactions.EnabledTypes != nil {
		t.Fatal("Validate applied interaction defaults")
	}
	c.Interactions.Enabled, c.Interactions.EnabledTypes = true, []string{"question"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAcceptsOvernightQuietAndConfiguredCardInterval(t *testing.T) {
	c := validTestConfig(t)
	c.Notifications.QuietHours = "23:00-08:00"
	c.Discovery.ScanIntervalSeconds = 15
	c.Sync.CardUpdateMinIntervalMS = 5000
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
