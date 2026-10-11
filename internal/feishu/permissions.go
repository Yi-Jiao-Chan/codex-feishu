package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// GrantedTenantScopes returns only explicitly granted application-identity
// scopes. It never interprets scope possession as observed API/event success.
func (g *Gateway) GrantedTenantScopes(ctx context.Context) ([]string, error) {
	data, err := g.Scopes(ctx)
	if err != nil {
		return nil, err
	}
	return parseGrantedTenantScopes(data)
}

// Numeric grant states are verified against the official export:
// https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/application-v6/scope/list.md
// 1 = granted; 2 = not granted. No pending value is documented. The pinned
// official SDK v3.12.0 Scope.ScopeType documents tenant and user identities.
// The export omits scope_type, so an absent type is deliberately unresolved.
func parseGrantedTenantScopes(data json.RawMessage) ([]string, error) {
	var response struct {
		Scopes *[]struct {
			Name   *string `json:"scope_name"`
			Status *int    `json:"grant_status"`
			Type   *string `json:"scope_type"`
		} `json:"scopes"`
	}
	if json.Unmarshal(data, &response) != nil || response.Scopes == nil {
		return nil, errors.New("Feishu scope data malformed or absent")
	}
	// Errors contain only static reasons, never remote payloads or scope names.
	granted := map[string]bool{}
	seen := map[string]int{}
	for _, scope := range *response.Scopes {
		if scope.Name == nil || *scope.Name == "" || strings.TrimSpace(*scope.Name) != *scope.Name || scope.Status == nil || scope.Type == nil {
			return nil, errors.New("Feishu scope entry incomplete")
		}
		if *scope.Status != 1 && *scope.Status != 2 {
			return nil, errors.New("Feishu scope grant status unknown")
		}
		if *scope.Type != "tenant" && *scope.Type != "user" {
			return nil, errors.New("Feishu scope identity type unknown")
		}
		key := *scope.Type + ":" + *scope.Name
		if previous, exists := seen[key]; exists && previous != *scope.Status {
			return nil, errors.New("Feishu scope grant states conflict")
		}
		seen[key] = *scope.Status
		if *scope.Type == "tenant" && *scope.Status == 1 {
			granted[*scope.Name] = true
		}
	}
	result := make([]string, 0, len(granted))
	for scope := range granted {
		result = append(result, scope)
	}
	sort.Strings(result)
	return result, nil
}
