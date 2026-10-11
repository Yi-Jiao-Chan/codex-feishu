package feishu

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestGrantedTenantScopesFixture(t *testing.T) {
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
			return
		}
		if r.Method != "GET" || r.URL.Path != "/open-apis/application/v6/scopes" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("scope query endpoint or tenant authentication incorrect")
		}
		w.Write([]byte(`{"code":0,"data":{"scopes":[
			{"scope_name":"im:chat:read","grant_status":1,"scope_type":"tenant"},
			{"scope_name":"im:message:send_as_bot","grant_status":2,"scope_type":"tenant"},
			{"scope_name":"im:message:send_as_bot","grant_status":1,"scope_type":"user"},
			{"scope_name":"im:chat:create","grant_status":1,"scope_type":"tenant"},
			{"scope_name":"im:chat:create","grant_status":1,"scope_type":"tenant"}
		]}}`))
	})
	got, err := g.GrantedTenantScopes(context.Background())
	if err != nil || !reflect.DeepEqual(got, []string{"im:chat:create", "im:chat:read"}) {
		t.Fatalf("incorrect tenant grants: %v, %v", got, err)
	}
}

func TestGrantedTenantScopesConservativePayloadHandling(t *testing.T) {
	cases := []struct {
		name  string
		data  string
		valid bool
	}{
		{"empty", `{"scopes":[]}`, true},
		{"denied", `{"scopes":[{"scope_name":"im:chat:create","grant_status":2,"scope_type":"tenant"}]}`, true},
		{"user_only", `{"scopes":[{"scope_name":"im:chat:create","grant_status":1,"scope_type":"user"}]}`, true},
		{"null_data", `null`, false},
		{"absent_scopes", `{}`, false},
		{"null_scopes", `{"scopes":null}`, false},
		{"wrong_scopes_type", `{"scopes":{}}`, false},
		{"null_entry", `{"scopes":[null]}`, false},
		{"malformed", `{"scopes":[`, false},
		{"absent_status", `{"scopes":[{"scope_name":"payload-sensitive","scope_type":"tenant"}]}`, false},
		{"null_status", `{"scopes":[{"scope_name":"payload-sensitive","grant_status":null,"scope_type":"tenant"}]}`, false},
		{"zero_status", `{"scopes":[{"scope_name":"payload-sensitive","grant_status":0,"scope_type":"tenant"}]}`, false},
		{"pending_unknown_status", `{"scopes":[{"scope_name":"payload-sensitive","grant_status":3,"scope_type":"tenant"}]}`, false},
		{"string_status", `{"scopes":[{"scope_name":"payload-sensitive","grant_status":"1","scope_type":"tenant"}]}`, false},
		{"missing_identity", `{"scopes":[{"scope_name":"payload-sensitive","grant_status":1}]}`, false},
		{"null_identity", `{"scopes":[{"scope_name":"payload-sensitive","grant_status":1,"scope_type":null}]}`, false},
		{"unknown_identity", `{"scopes":[{"scope_name":"payload-sensitive","grant_status":1,"scope_type":"unknown"}]}`, false},
		{"empty_name", `{"scopes":[{"scope_name":"","grant_status":1,"scope_type":"tenant"}]}`, false},
		{"missing_name", `{"scopes":[{"grant_status":1,"scope_type":"tenant"}]}`, false},
		{"conflicting_duplicate", `{"scopes":[{"scope_name":"payload-sensitive","grant_status":1,"scope_type":"tenant"},{"scope_name":"payload-sensitive","grant_status":2,"scope_type":"tenant"}]}`, false},
		{"partial_success_unknown", `{"scopes":[{"scope_name":"im:chat:read","grant_status":1,"scope_type":"tenant"},{"scope_name":"payload-sensitive","grant_status":3,"scope_type":"tenant"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
					return
				}
				w.Write([]byte(`{"code":0,"data":` + tc.data + `}`))
			})
			got, err := g.GrantedTenantScopes(context.Background())
			if tc.valid {
				if err != nil || len(got) != 0 || got == nil {
					t.Fatalf("valid empty tenant grants: %v, %v", got, err)
				}
			} else if err == nil || got != nil || strings.Contains(err.Error(), "payload-sensitive") || strings.Contains(err.Error(), "fixture-token") {
				t.Fatalf("unsafe or nonconservative parsing: %v, %v", got, err)
			}
		})
	}
}
