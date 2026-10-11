package setup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/config"
	"github.com/pelletier/go-toml/v2"
)

const (
	pairAppID       = "cli_pair_application"
	pairOwner       = "ou_pair_owner_1"
	pairOtherOwner  = "ou_pair_owner_2"
	pairSecretValue = "pair-secret-value"
)

func pairInput(t *testing.T) (Input, string, string) {
	t.Helper()
	root := t.TempDir()
	secretPath := filepath.Join(root, "credentials", "secret")
	if err := os.MkdirAll(filepath.Dir(secretPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte(pairSecretValue), 0600); err != nil {
		t.Fatal(err)
	}
	var in Input
	in.SchemaVersion = 1
	in.Feishu.AppID = pairAppID
	in.Feishu.AppSecretFile = secretPath
	return in, filepath.Join(root, "config", "config.toml"), root
}

func writePairConfig(t *testing.T, path, appID, secretPath, owner string) []byte {
	t.Helper()
	root := filepath.Dir(filepath.Dir(path))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	var c config.Config
	c.SchemaVersion = 1
	c.Host.ID = "persistent-pair-host"
	c.Codex.CLIPath = "auto"
	c.Codex.Endpoint = "auto"
	c.Feishu.AppID = appID
	c.Feishu.AppSecretFile = secretPath
	c.Access.OwnerOpenID = owner
	c.Access.OperatorOpenIDs = []string{owner}
	c.Discovery.FirstRun = "baseline"
	c.Discovery.AllowedRoots = []string{root}
	c.Notifications.Timezone = "Asia/Shanghai"
	c.Notifications.MentionOn = []string{"completed", "failed", "needs_user"}
	c.Storage.DatabaseFile = filepath.Join(root, "bridge.db")
	if err := c.Validate(); err != nil {
		t.Fatalf("pair fixture config: %v", err)
	}
	b, err := toml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPreparePairAcceptsPartialInputAndHidesSecret(t *testing.T) {
	in, configPath, root := pairInput(t)
	prepared, err := PreparePair(in, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.AppID != pairAppID || prepared.Secret != pairSecretValue || prepared.Existing != nil || prepared.ExpectedOwner != "" {
		t.Fatalf("unexpected preparation: %+v", prepared)
	}
	if !filepath.IsAbs(prepared.ConfigPath) {
		t.Fatalf("configuration path is not absolute: %q", prepared.ConfigPath)
	}
	if _, err := os.Stat(filepath.Dir(configPath)); !os.IsNotExist(err) {
		t.Fatalf("pair created a configuration directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bridge.db")); !os.IsNotExist(err) {
		t.Fatalf("pair created a database: %v", err)
	}
	encoded, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), pairSecretValue) {
		t.Fatalf("pair JSON exposed the secret: %s", encoded)
	}
}

func TestPreparePairValidOwnerRestrictsExpectedOwner(t *testing.T) {
	in, configPath, _ := pairInput(t)
	in.Identity.OwnerOpenID = new(string)
	*in.Identity.OwnerOpenID = pairOwner
	prepared, err := PreparePair(in, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ExpectedOwner != pairOwner {
		t.Fatalf("expected owner %q, got %q", pairOwner, prepared.ExpectedOwner)
	}
}

func TestPreparePairRejectsStaticInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Input, string)
	}{
		{name: "bad app id", edit: func(in *Input, _ string) { in.Feishu.AppID = "bad app id" }},
		{name: "placeholder app id", edit: func(in *Input, _ string) { in.Feishu.AppID = "cli_replace_me" }},
		{name: "relative secret path", edit: func(in *Input, _ string) { in.Feishu.AppSecretFile = "secret" }},
		{name: "invalid owner", edit: func(in *Input, _ string) {
			in.Identity.OwnerOpenID = new(string)
			*in.Identity.OwnerOpenID = "ou_replace_owner"
		}},
		{name: "phase2", edit: func(in *Input, _ string) { in.Features.Phase = "phase2" }},
		{name: "creation", edit: func(in *Input, _ string) { in.Features.Creation = true }},
		{name: "unknown schema", edit: func(in *Input, _ string) { in.SchemaVersion = 2 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in, configPath, _ := pairInput(t)
			test.edit(&in, configPath)
			if _, err := PreparePair(in, configPath); err == nil {
				t.Fatal("invalid pair input was accepted")
			}
		})
	}
}

func TestPreparePairRejectsUnsafeOrSymlinkSecretWithoutChangingIt(t *testing.T) {
	t.Run("unsafe permissions", func(t *testing.T) {
		in, configPath, _ := pairInput(t)
		if err := os.Chmod(in.Feishu.AppSecretFile, 0644); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(in.Feishu.AppSecretFile)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := PreparePair(in, configPath); err == nil {
			t.Fatal("unsafe secret permissions were accepted")
		}
		after, err := os.Stat(in.Feishu.AppSecretFile)
		if err != nil || after.Mode().Perm() != before.Mode().Perm() {
			t.Fatalf("pair changed secret permissions: before=%v after=%v err=%v", before.Mode().Perm(), after.Mode().Perm(), err)
		}
	})

	t.Run("final symlink", func(t *testing.T) {
		in, configPath, root := pairInput(t)
		symlink := filepath.Join(root, "credentials", "secret-link")
		if err := os.Symlink(in.Feishu.AppSecretFile, symlink); err != nil {
			t.Fatal(err)
		}
		in.Feishu.AppSecretFile = symlink
		if _, err := PreparePair(in, configPath); err == nil {
			t.Fatal("secret symlink was accepted")
		}
	})
}

func TestPreparePairExistingConfigPreservedAndParentAliasesMatch(t *testing.T) {
	in, configPath, root := pairInput(t)
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	configBytes := writePairConfig(t, configPath, pairAppID, in.Feishu.AppSecretFile, pairOwner)
	configAlias := filepath.Join(root, "config-alias")
	if err := os.Symlink(filepath.Dir(configPath), configAlias); err != nil {
		t.Fatal(err)
	}
	secretAliasDir := filepath.Join(root, "credentials-alias")
	if err := os.Symlink(filepath.Dir(in.Feishu.AppSecretFile), secretAliasDir); err != nil {
		t.Fatal(err)
	}
	in.Feishu.AppSecretFile = filepath.Join(secretAliasDir, filepath.Base(in.Feishu.AppSecretFile))
	configAliasPath := filepath.Join(configAlias, filepath.Base(configPath))
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PreparePair(in, configAliasPath)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Existing == nil || prepared.ExpectedOwner != pairOwner || prepared.ConfigPath != configPath {
		t.Fatalf("unexpected existing preparation: %+v", prepared)
	}
	if string(before) != string(configBytes) {
		t.Fatal("fixture changed before pairing")
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("pair changed existing configuration: %v", err)
	}
	info, err := os.Stat(configPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("pair changed configuration permissions: %v", err)
	}
}

func TestPreparePairRejectsExistingIdentityAndSecretReferenceChanges(t *testing.T) {
	tests := []struct {
		name string
		edit func(Input, string) Input
	}{
		{name: "app id", edit: func(in Input, _ string) Input {
			in.Feishu.AppID = "cli_other_application"
			return in
		}},
		{name: "owner", edit: func(in Input, _ string) Input {
			in.Identity.OwnerOpenID = new(string)
			*in.Identity.OwnerOpenID = pairOtherOwner
			return in
		}},
		{name: "secret reference", edit: func(in Input, configPath string) Input {
			other := filepath.Join(filepath.Dir(filepath.Dir(configPath)), "other-secret")
			if err := os.WriteFile(other, []byte(pairSecretValue+"-other"), 0600); err != nil {
				t.Fatal(err)
			}
			in.Feishu.AppSecretFile = other
			return in
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in, configPath, _ := pairInput(t)
			writePairConfig(t, configPath, pairAppID, in.Feishu.AppSecretFile, pairOwner)
			in = test.edit(in, configPath)
			if _, err := PreparePair(in, configPath); !errors.Is(err, ErrPairIdentityConflict) {
				t.Fatalf("expected identity conflict, got %v", err)
			}
		})
	}
}

func TestPreparePairRejectsMalformedExistingConfigWithoutOverwrite(t *testing.T) {
	in, configPath, _ := pairInput(t)
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	before := []byte("not valid setup configuration\n")
	if err := os.WriteFile(configPath, before, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PreparePair(in, configPath); err == nil {
		t.Fatal("malformed existing configuration was accepted")
	} else if !errors.Is(err, errPairExistingConfig) {
		t.Fatalf("unexpected malformed config error: %v", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("pair overwrote malformed configuration: %v", err)
	}
}
