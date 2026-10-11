// Package compat records the bounded, documented compatibility requirements.
package compat

import (
	_ "embed"
	"encoding/json"
)

//go:embed feishu-permissions.json
var feishuPermissions []byte

type PermissionManifest struct {
	SchemaVersion           int          `json:"schema_version"`
	VerifiedAt              string       `json:"verified_at"`
	SourceResearch          string       `json:"source_research"`
	SDK                     string       `json:"sdk"`
	SDKVersion              string       `json:"sdk_version"`
	RecommendedTenantScopes []string     `json:"recommended_tenant_scopes"`
	Capabilities            []Capability `json:"capabilities"`
}

type Capability struct {
	ID                  string   `json:"id"`
	Required            bool     `json:"required"`
	API                 []string `json:"api"`
	AnyOfPermissions    []string `json:"any_of_permissions"`
	Source              []string `json:"source"`
	SDK                 string   `json:"sdk"`
	SDKVersion          string   `json:"sdk_version"`
	DocumentationStatus string   `json:"documentation_status"`
	NonScopeConditions  []string `json:"non_scope_conditions"`
	VerificationMethod  string   `json:"verification_method"`
}

type CapabilityCheck struct {
	Capability
	DocumentedPermissionSatisfied string `json:"documented_permission_satisfied"`
	RuntimeObserved               string `json:"runtime_observed"`
	VerificationPending           bool   `json:"verification_pending"`
}

// FeishuManifest returns an independent copy of the embedded manifest, so it is
// usable from installed binaries regardless of the process working directory.
func FeishuManifest() PermissionManifest {
	var manifest PermissionManifest
	if err := json.Unmarshal(feishuPermissions, &manifest); err != nil {
		panic("invalid embedded Feishu permission manifest")
	}
	return manifest
}

// EvaluatePermissions checks only documented scope requirements. Each
// AnyOfPermissions list is an OR, and a scope-free capability is satisfied only
// in this narrow sense. Runtime behavior and non-scope conditions remain unknown.
// The caller must supply only verified, granted tenant scopes; an unavailable
// scope query must be reported separately rather than passed as an empty list.
func EvaluatePermissions(granted []string) []CapabilityCheck {
	set := make(map[string]bool, len(granted))
	for _, scope := range granted {
		set[scope] = true
	}
	manifest := FeishuManifest()
	checks := make([]CapabilityCheck, 0, len(manifest.Capabilities))
	for _, capability := range manifest.Capabilities {
		status := "fail"
		if capability.DocumentationStatus == "documentation_gap" {
			status = "pending"
		} else if len(capability.AnyOfPermissions) == 0 {
			status = "pass"
		} else {
			for _, scope := range capability.AnyOfPermissions {
				if set[scope] {
					status = "pass"
					break
				}
			}
		}
		checks = append(checks, CapabilityCheck{
			Capability: capability, DocumentedPermissionSatisfied: status,
			RuntimeObserved: "unknown", VerificationPending: true,
		})
	}
	return checks
}
