package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilePreviewRequiresSeparateScopeAndValidOrigin(t *testing.T) {
	c := validTestConfig(t)
	if err := c.ValidateFilePreview(); err != nil || c.FilePreview.Enabled {
		t.Fatal("preview default changed existing installations", err)
	}
	c.FilePreview.Enabled = true
	c.FilePreview.BaseURL = "http://10.0.0.1:8787"
	if err := c.ValidateFilePreview(); err == nil {
		t.Fatal("preview inherited discovery scope without authorization")
	}
	c.FilePreview.AllowedRoots = c.Discovery.AllowedRoots
	if err := c.ValidateFilePreview(); err != nil {
		t.Fatal("valid IP preview rejected", err)
	}
	for _, address := range []string{"file:///tmp/a", "http://user:pass@10.0.0.1", "http://10.0.0.1/path", "http://10.0.0.1?token=x", "http://10.0.0.1#fragment", "http://10.0.0.1:65536", "http://10.0.0.1:0"} {
		c.FilePreview.BaseURL = address
		if err := c.ValidateFilePreview(); err == nil {
			t.Fatalf("unsafe preview origin accepted: %s", address)
		}
	}
}

func TestFilePreviewSecretRemainsPrivateAndRejectsSymlinks(t *testing.T) {
	c := validTestConfig(t)
	path := c.FilePreviewLimits().SecretFile
	secret := strings.Repeat("a", 64)
	if err := os.WriteFile(path, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := c.FilePreviewSecret(); err != nil || got != secret {
		t.Fatal("private preview access code rejected", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FilePreviewSecret(); err == nil {
		t.Fatal("public preview access code file accepted")
	}
	os.Chmod(path, 0600)
	link := filepath.Join(filepath.Dir(path), "alias.key")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	c.FilePreview.SecretFile = link
	if _, err := c.FilePreviewSecret(); err == nil {
		t.Fatal("symlink preview access code accepted")
	}
}
