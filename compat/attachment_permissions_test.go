package compat

import (
	"reflect"
	"testing"
)

func capabilityByID(checks []CapabilityCheck, id string) (CapabilityCheck, bool) {
	for _, check := range checks {
		if check.ID == id {
			return check, true
		}
	}
	return CapabilityCheck{}, false
}

func TestAttachmentCapabilitiesAreDocumentedAndOptional(t *testing.T) {
	manifest := FeishuManifest()
	download, ok := capabilityByID(EvaluatePermissions(nil), "download_message_resources")
	if !ok {
		t.Fatal("download capability missing from manifest")
	}
	standalone, ok := capabilityByID(EvaluatePermissions(nil), "receive_standalone_attachments")
	if !ok {
		t.Fatal("standalone capability missing from manifest")
	}
	if download.Required || standalone.Required {
		t.Fatalf("attachment capabilities must be optional by default: download=%+v standalone=%+v", download, standalone)
	}
	if !reflect.DeepEqual(download.AnyOfPermissions, []string{"im:message", "im:message:readonly", "im:message.history:readonly"}) {
		t.Fatalf("download scope alternatives changed: %v", download.AnyOfPermissions)
	}
	if !reflect.DeepEqual(standalone.AnyOfPermissions, []string{"im:message.group_msg"}) {
		t.Fatalf("standalone scope changed: %v", standalone.AnyOfPermissions)
	}
	for _, capability := range []CapabilityCheck{download, standalone} {
		foundResearch, foundOfficial := false, false
		for _, source := range capability.Source {
			foundResearch = foundResearch || source == "docs/research/feishu-inbound-resources-2026-10-15.md"
			foundOfficial = foundOfficial || len(source) >= len("https://open.feishu.cn/") && source[:len("https://open.feishu.cn/")] == "https://open.feishu.cn/"
		}
		if !foundResearch || !foundOfficial {
			t.Fatalf("attachment capability lacks exact research and official sources: %+v", capability)
		}
	}
	if len(manifest.RecommendedTenantScopes) != 9 {
		t.Fatalf("attachment manifest changed baseline recommended scopes: %v", manifest.RecommendedTenantScopes)
	}
}

func TestAttachmentPermissionRequirementsAreConditionalAndORed(t *testing.T) {
	baseline := EvaluatePermissions(nil)
	for _, scenario := range []struct {
		name       string
		enabled    bool
		standalone bool
		granted    []string
		download   string
		receive    string
		reqD       bool
		reqR       bool
	}{
		{name: "disabled", granted: nil, download: "fail", receive: "fail"},
		{name: "enabled-post-only", enabled: true, granted: nil, download: "fail", receive: "fail", reqD: true},
		{name: "enabled-standalone", enabled: true, standalone: true, granted: []string{"im:message:readonly", "im:message.group_msg"}, download: "pass", receive: "pass", reqD: true, reqR: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			got := EvaluatePermissionsForAttachments(scenario.granted, scenario.enabled, scenario.standalone)
			for _, check := range got {
				base, ok := capabilityByID(baseline, check.ID)
				if !ok {
					continue
				}
				if check.ID != "download_message_resources" && check.ID != "receive_standalone_attachments" && check.Required != base.Required {
					t.Fatalf("baseline requirement changed for %s: got=%t want=%t", check.ID, check.Required, base.Required)
				}
			}
			download, _ := capabilityByID(got, "download_message_resources")
			receive, _ := capabilityByID(got, "receive_standalone_attachments")
			if download.Required != scenario.reqD || receive.Required != scenario.reqR || download.DocumentedPermissionSatisfied != scenario.download || receive.DocumentedPermissionSatisfied != scenario.receive {
				t.Fatalf("unexpected conditional checks: download=%+v receive=%+v", download, receive)
			}
		})
	}
	for _, scope := range []string{"im:message", "im:message:readonly", "im:message.history:readonly"} {
		check, _ := capabilityByID(EvaluatePermissionsForAttachments([]string{scope}, true, false), "download_message_resources")
		if check.DocumentedPermissionSatisfied != "pass" {
			t.Fatalf("download scope %q was not treated as an OR alternative", scope)
		}
	}
}
