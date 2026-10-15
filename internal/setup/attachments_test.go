package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/config"
)

func TestSetupAttachmentsDefaultPreserveAndExplicitReplacement(t *testing.T) {
	p, in, paths := fixture(t)

	initial, err := p.Build(context.Background(), in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Config.Attachments.Enabled || initial.Config.Attachments.ReceiveStandalone {
		t.Fatal("new setup unexpectedly enabled attachments")
	}
	limits := initial.Config.AttachmentLimits()
	if limits.MaxResources != 10 || limits.MaxImages != 5 || limits.MaxBatchBytes != 50<<20 || !strings.HasSuffix(limits.Directory, filepath.Join("", "attachments")) {
		t.Fatalf("unexpected attachment defaults: %+v", limits)
	}

	customRoot := filepath.Join(filepath.Dir(paths.DatabaseFile), "private-attachments")
	in.Attachments = &config.AttachmentConfig{
		Enabled:       true,
		Directory:     customRoot,
		MaxResources:  7,
		MaxImages:     3,
		MaxBatchBytes: 32 << 20,
	}
	custom, err := p.Build(context.Background(), in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if !custom.Config.Attachments.Enabled || custom.Config.Attachments.Directory != customRoot || custom.Config.Attachments.MaxResources != 7 {
		t.Fatalf("explicit attachment settings were not applied: %+v", custom.Config.Attachments)
	}
	if !strings.Contains(strings.Join(custom.NextSteps, "\n"), "private local storage root="+customRoot) || !strings.Contains(strings.Join(custom.NextSteps, "\n"), "only from @post messages") {
		t.Fatalf("attachment next steps omitted safe defaults or @post rule: %v", custom.NextSteps)
	}
	if _, err := p.Apply(context.Background(), in, paths, custom.ID); err != nil {
		t.Fatal(err)
	}

	in.Attachments = nil
	preserved, err := p.Build(context.Background(), in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Config.Attachments != custom.Config.Attachments {
		t.Fatalf("omitted attachment settings replaced existing config: got=%+v want=%+v", preserved.Config.Attachments, custom.Config.Attachments)
	}

	in.Attachments = &config.AttachmentConfig{Enabled: false, Directory: filepath.Join(filepath.Dir(paths.DatabaseFile), "replacement")}
	replaced, err := p.Build(context.Background(), in, paths)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Config.Attachments.Enabled || replaced.Config.Attachments.Directory == customRoot {
		t.Fatalf("explicit attachment settings did not replace existing config: %+v", replaced.Config.Attachments)
	}
}

func TestSetupRejectsStandaloneAttachmentsWhenDisabled(t *testing.T) {
	p, in, paths := fixture(t)
	in.Attachments = &config.AttachmentConfig{ReceiveStandalone: true}
	if _, err := p.Build(context.Background(), in, paths); err == nil {
		t.Fatal("disabled attachment configuration accepted standalone receive")
	}
	if _, err := os.Stat(paths.ConfigFile); !os.IsNotExist(err) {
		t.Fatal("invalid attachment setup created a config file")
	}
}
