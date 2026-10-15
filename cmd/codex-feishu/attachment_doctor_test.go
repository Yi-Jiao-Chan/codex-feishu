package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/compat"
	"codex-feishu/internal/config"
	"github.com/pelletier/go-toml/v2"
)

func writeDoctorFixtureConfig(t *testing.T, path string, cfg config.Config) {
	t.Helper()
	b, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func doctorCapability(t *testing.T, o output, id string) compat.CapabilityCheck {
	t.Helper()
	data := o.Data.(map[string]any)
	checks, ok := data["feishu_capabilities"].([]compat.CapabilityCheck)
	if !ok {
		t.Fatalf("doctor omitted capability checks: %#v", data["feishu_capabilities"])
	}
	for _, check := range checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("doctor omitted capability %q", id)
	return compat.CapabilityCheck{}
}

func TestDoctorReportsSafeAttachmentLimitsAndDisabledCapabilities(t *testing.T) {
	path, cfg, deps, gateway := doctorFixture(t)
	o, code := inspectDoctor(context.Background(), path, false, true, deps)
	if code != 0 || !o.OK {
		t.Fatalf("disabled attachment doctor failed: %v %#v", code, o)
	}
	download := doctorCapability(t, o, "download_message_resources")
	standalone := doctorCapability(t, o, "receive_standalone_attachments")
	if download.Required || standalone.Required || gateway.calls != 2 {
		t.Fatalf("disabled attachment capabilities became required: download=%+v standalone=%+v", download, standalone)
	}
	data, ok := o.Data.(map[string]any)
	if !ok {
		t.Fatal("doctor data missing")
	}
	attachments, ok := data["attachments"].(map[string]any)
	if !ok || attachments["enabled"] != false || attachments["receive_standalone"] != false || attachments["ui_verified"] != false {
		t.Fatalf("unsafe or incomplete attachment doctor data: %#v", data["attachments"])
	}
	if attachments["root"] != filepath.Join(filepath.Dir(cfg.Storage.DatabaseFile), "attachments") {
		t.Fatalf("unexpected attachment root: %#v", attachments["root"])
	}
	b, _ := json.Marshal(o)
	encoded := string(b)
	for _, forbidden := range []string{"doctor-secret-never-output", "input_json"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("doctor leaked attachment or credential data %q: %s", forbidden, encoded)
		}
	}
}

func TestDoctorAttachmentPermissionGatesFollowConfiguration(t *testing.T) {
	path, cfg, deps, gateway := doctorFixture(t)
	cfg.Attachments.Enabled = true
	cfg.Attachments.Directory = filepath.Join(filepath.Dir(cfg.Storage.DatabaseFile), "attachments")
	writeDoctorFixtureConfig(t, path, cfg)

	o, code := inspectDoctor(context.Background(), path, false, true, deps)
	if code != 4 || o.OK {
		t.Fatalf("enabled attachments without resource scope passed: code=%d output=%+v", code, o)
	}
	download := doctorCapability(t, o, "download_message_resources")
	standalone := doctorCapability(t, o, "receive_standalone_attachments")
	if !download.Required || standalone.Required || download.DocumentedPermissionSatisfied != "fail" {
		t.Fatalf("post-only attachment gates incorrect: download=%+v standalone=%+v", download, standalone)
	}

	cfg.Attachments.ReceiveStandalone = true
	writeDoctorFixtureConfig(t, path, cfg)
	gateway.scopes = append([]string{}, compat.FeishuManifest().RecommendedTenantScopes...)
	gateway.scopes = append(gateway.scopes, "im:message.history:readonly", "im:message.group_msg")
	o, code = inspectDoctor(context.Background(), path, false, true, deps)
	if code != 0 || !o.OK {
		t.Fatalf("enabled standalone attachment permissions did not pass: code=%d output=%+v", code, o)
	}
	if download = doctorCapability(t, o, "download_message_resources"); !download.Required || download.DocumentedPermissionSatisfied != "pass" {
		t.Fatalf("resource download gate incorrect: %+v", download)
	}
	if standalone = doctorCapability(t, o, "receive_standalone_attachments"); !standalone.Required || standalone.DocumentedPermissionSatisfied != "pass" {
		t.Fatalf("standalone receive gate incorrect: %+v", standalone)
	}
}
