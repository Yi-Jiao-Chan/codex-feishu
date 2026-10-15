package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/config"
)

func TestSetupPreviewPreservesScopeAndBindsAccessCredential(t *testing.T) {
	ctx := context.Background()
	p, in, paths := fixture(t)
	secret := filepath.Join(filepath.Dir(in.Artifact.BinaryPath), "preview.key")
	if err := os.WriteFile(secret, []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	in.FilePreview = &config.FilePreviewConfig{Enabled: true, BaseURL: "http://10.0.0.1:8787", SecretFile: secret, AllowedRoots: in.Discovery.AllowedRoots}
	plan, err := p.Build(ctx, in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(ctx, in, paths, plan.ID); err != nil {
		t.Fatal(err)
	}
	in.FilePreview = nil
	preserved, err := p.Build(ctx, in, paths)
	if err != nil || !preserved.Config.FilePreview.Enabled || preserved.Config.FilePreview.BaseURL != plan.Config.FilePreview.BaseURL {
		t.Fatal("setup omission discarded existing preview scope", err)
	}
	if err := os.WriteFile(secret, []byte(strings.Repeat("b", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(ctx, in, paths, preserved.ID); !errors.Is(err, ErrStalePlan) {
		t.Fatal("changed preview credential did not invalidate its review plan", err)
	}
}
