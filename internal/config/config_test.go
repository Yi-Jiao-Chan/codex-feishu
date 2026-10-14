package config

import (
	"path/filepath"
	"testing"
)

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

func TestInteractionsRequireExplicitRequestTypesAndDefaultOff(t *testing.T) {
	c := validTestConfig(t)
	if c.Interactions.Enabled {
		t.Fatal("interaction controls enabled implicitly")
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
