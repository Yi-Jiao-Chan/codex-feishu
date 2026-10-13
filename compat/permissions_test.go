package compat

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestManifestEmbeddedAndBounded(t *testing.T) {
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	m := FeishuManifest()
	want := []string{"im:message.p2p_msg:readonly", "im:message.group_at_msg:readonly", "im:chat:create", "im:chat:read", "im:chat.members:read", "im:message:send_as_bot", "im:message.reactions:write_only", "im:resource:upload"}
	if !reflect.DeepEqual(m.RecommendedTenantScopes, want) || m.SDKVersion != "v3.12.0" || len(m.Capabilities) != 15 {
		t.Fatalf("unexpected manifest boundary: %+v", m)
	}
	for _, c := range m.Capabilities {
		if c.ID == "" || len(c.API) == 0 || len(c.Source) == 0 || c.VerificationMethod == "" || c.SDKVersion != m.SDKVersion {
			t.Fatalf("incomplete capability: %+v", c)
		}
		for _, s := range c.AnyOfPermissions {
			if strings.HasPrefix(s, "im:message.group_msg") || strings.HasPrefix(s, "contact:") || strings.HasPrefix(s, "cardkit:") || s == "application:bot.basic_info:read" {
				t.Fatalf("unrequested scope: %s", s)
			}
		}
	}
	// A caller cannot mutate the embedded configuration for later calls.
	m.RecommendedTenantScopes[0] = "changed"
	m.Capabilities[0].API[0] = "changed"
	if next := FeishuManifest(); next.RecommendedTenantScopes[0] != want[0] || next.Capabilities[0].API[0] == "changed" {
		t.Fatal("manifest accessor shares mutable storage")
	}
	if _, err := json.Marshal(FeishuManifest()); err != nil {
		t.Fatal(err)
	}
}

func TestRecommendedPermissionsOnlyPassDocumentedChecks(t *testing.T) {
	checks := EvaluatePermissions(FeishuManifest().RecommendedTenantScopes)
	for _, check := range checks {
		if check.Required && check.DocumentedPermissionSatisfied != "pass" {
			t.Fatalf("recommended baseline does not satisfy %s", check.ID)
		}
		if check.RuntimeObserved != "unknown" || !check.VerificationPending {
			t.Fatalf("permissions inferred runtime success: %+v", check)
		}
		if check.ID == "get_own_group_message" && (check.Required || check.DocumentedPermissionSatisfied != "pending") {
			t.Fatalf("documentation gap overstated: %+v", check)
		}
		if check.ID == "get_own_p2p_message" && (check.Required || check.DocumentedPermissionSatisfied != "fail") {
			t.Fatalf("readback became baseline requirement: %+v", check)
		}
	}
}

func TestPermissionAlternativesAreOR(t *testing.T) {
	for _, c := range FeishuManifest().Capabilities {
		for _, scope := range c.AnyOfPermissions {
			for _, check := range EvaluatePermissions([]string{scope}) {
				if check.ID == c.ID && check.DocumentedPermissionSatisfied != "pass" {
					t.Fatalf("%s alone did not satisfy %s", scope, c.ID)
				}
			}
		}
	}
	for _, check := range EvaluatePermissions(nil) {
		if len(check.AnyOfPermissions) > 0 && check.DocumentedPermissionSatisfied != "fail" {
			t.Fatalf("empty grants satisfied %s", check.ID)
		}
		if (check.ID == "bot_identity" || check.ID == "card_callback") && (check.DocumentedPermissionSatisfied != "pass" || check.RuntimeObserved != "unknown") {
			t.Fatalf("scope-free capability incorrectly evaluated: %+v", check)
		}
	}
}
