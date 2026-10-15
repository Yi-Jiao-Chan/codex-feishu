package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachmentDefaultsPreserveDisabledInstallationIdentity(t *testing.T) {
	c := validTestConfig(t)
	a := c.AttachmentLimits()
	if a.Enabled || a.ReceiveStandalone || a.MaxResources != 10 || a.MaxImages != 5 || a.MaxImageBytes != 10<<20 || a.MaxImagesBytes != 20<<20 || a.MaxFileBytes != 20<<20 || a.MaxBatchBytes != 50<<20 || a.MaxImagePixels != 40_000_000 || a.DiskQuotaBytes != 1<<30 || a.DownloadWorkers != 2 || a.Directory != filepath.Join(filepath.Dir(c.Storage.DatabaseFile), "attachments") {
		t.Fatal("attachment defaults changed")
	}
	raw, err := json.Marshal(c)
	if err != nil || strings.Contains(string(raw), `"Attachments"`) {
		t.Fatal("zero new feature changes an existing control identity", err)
	}
	c.Attachments.Enabled = true
	raw, err = json.Marshal(c)
	if err != nil || !strings.Contains(string(raw), `"Attachments"`) {
		t.Fatal("enabled attachment settings omitted from control identity", err)
	}
}

func TestAttachmentConfigurationRejectsUnsafeBudgetsAndPaths(t *testing.T) {
	for _, edit := range []func(*AttachmentConfig){
		func(a *AttachmentConfig) { a.Directory = "relative" },
		func(a *AttachmentConfig) { a.ReceiveStandalone = true },
		func(a *AttachmentConfig) { a.MaxImageBytes = 11 << 20 },
		func(a *AttachmentConfig) { a.MaxImagesBytes = 21 << 20 },
		func(a *AttachmentConfig) { a.MaxResources = -1 },
		func(a *AttachmentConfig) { a.MaxImagePixels = 100_000_001 },
		func(a *AttachmentConfig) { a.DownloadWorkers = 9 },
	} {
		c := validTestConfig(t)
		edit(&c.Attachments)
		if err := c.Validate(); err == nil {
			t.Fatal("unsafe attachment configuration accepted")
		}
	}
}
