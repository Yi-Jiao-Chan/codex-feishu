package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func enabledCreationConfig(t *testing.T) (Config, string, string, string) {
	t.Helper()
	root := t.TempDir()
	inside := filepath.Join(root, "inside")
	outside := t.TempDir()
	if err := os.MkdirAll(inside, 0700); err != nil {
		t.Fatal(err)
	}
	c := validTestConfig(t)
	c.Creation.Enabled = true
	c.Creation.AllowedRoots = []string{root}
	c.Creation.EntryChatIDs = []string{"oc_entry"}
	c.Creation.WorkgroupChatIDs = []string{"oc_workgroup"}
	return c, root, inside, outside
}

func TestValidateCreationAcceptsExplicitScopeAndDefaults(t *testing.T) {
	c, root, inside, _ := enabledCreationConfig(t)
	c.Creation.CommonDirectories = []string{inside}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.CreationTTL(); got != 1800*time.Second {
		t.Fatalf("zero creation TTL = %s, want 30m", got)
	}
	if !c.CreationAllows(root) || !c.CreationAllows(inside) {
		t.Fatal("configured creation scope did not allow root and child")
	}
	if c.CreationAllows(filepath.Join(filepath.Dir(root), filepath.Base(root)+"-sibling")) {
		t.Fatal("creation scope widened across a path prefix")
	}

	c.Creation.DefaultFollowMode = ""
	if err := c.ValidateCreation(); err != nil {
		t.Fatal("empty follow mode should default to group:", err)
	}
	c.Creation.DraftTTLSeconds = 3600
	if got := c.CreationTTL(); got != time.Hour {
		t.Fatalf("explicit creation TTL = %s, want 1h", got)
	}
}

func TestValidateCreationRequiresItsOwnReadableSearchableRoots(t *testing.T) {
	c := validTestConfig(t)
	c.Creation.Enabled = true
	if err := c.ValidateCreation(); err == nil || !strings.Contains(err.Error(), "allowed_roots") {
		t.Fatalf("missing creation scope was accepted: %v", err)
	}

	root := t.TempDir()
	c.Creation.AllowedRoots = []string{root}
	c.Discovery.AllowedRoots = []string{t.TempDir()}
	if err := c.ValidateCreation(); err != nil {
		t.Fatalf("valid independent creation root rejected: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "missing")
	c.Creation.AllowedRoots = []string{missing}
	if err := c.ValidateCreation(); err == nil {
		t.Fatal("missing creation root accepted")
	}
}

func TestValidateCreationAllowsReadOnlyScopeRootButRequiresReadSearch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses ordinary directory access bits")
	}
	c, root, inside, _ := enabledCreationConfig(t)
	t.Cleanup(func() {
		if err := os.Chmod(root, 0700); err != nil {
			t.Error(err)
		}
	})
	c.Creation.CommonDirectories = []string{inside}
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateCreation(); err != nil {
		t.Fatal("read-only navigation boundary rejected", err)
	}
	if !c.CreationAllows(inside) {
		t.Fatal("read-only scope root hid its writable descendant")
	}
	for _, mode := range []os.FileMode{0300, 0600} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if err := c.ValidateCreation(); err == nil {
			t.Fatal("scope root missing read/search permission accepted", mode)
		}
	}
}

func TestValidateCreationRejectsEscapingSymlinksAndInvalidCommonDirectories(t *testing.T) {
	c, root, _, outside := enabledCreationConfig(t)
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	c.Creation.CommonDirectories = []string{escape}
	if err := c.ValidateCreation(); err == nil {
		t.Fatal("common directory symlink escaped its creation root")
	}

	c.Creation.CommonDirectories = []string{filepath.Join(outside, "missing")}
	if err := c.ValidateCreation(); err == nil {
		t.Fatal("missing common directory accepted")
	}

	linkInside := filepath.Join(root, "link-inside")
	if err := os.Symlink(root, linkInside); err != nil {
		t.Fatal(err)
	}
	if !c.CreationAllows(linkInside) {
		t.Fatal("symlink resolving inside the root was rejected")
	}
	if c.CreationAllows(escape) {
		t.Fatal("path symlink escaping the root was allowed")
	}
}

func TestValidateCreationEnforcesModesTopicsIDsAndTTL(t *testing.T) {
	c, _, _, _ := enabledCreationConfig(t)
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{name: "invalid mode", edit: func(c *Config) { c.Creation.DefaultFollowMode = "private" }},
		{name: "topic without topics", edit: func(c *Config) { c.Creation.DefaultFollowMode = "topic" }},
		{name: "topics without workgroup", edit: func(c *Config) { c.Creation.TopicsEnabled = true; c.Creation.WorkgroupChatIDs = nil }},
		{name: "duplicate entry", edit: func(c *Config) { c.Creation.EntryChatIDs = []string{"oc_same", "oc_same"} }},
		{name: "invalid chat shape", edit: func(c *Config) { c.Creation.EntryChatIDs = []string{"ou_user"} }},
		{name: "short TTL", edit: func(c *Config) { c.Creation.DraftTTLSeconds = 299 }},
		{name: "long TTL", edit: func(c *Config) { c.Creation.DraftTTLSeconds = 86401 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := c
			test.edit(&candidate)
			if err := candidate.ValidateCreation(); err == nil {
				t.Fatal("invalid creation policy accepted")
			}
		})
	}

	c.Creation.TopicsEnabled = true
	if err := c.ValidateCreation(); err != nil {
		t.Fatal("topics with a workgroup rejected:", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
