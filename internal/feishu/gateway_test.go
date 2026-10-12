package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func apiFixture(t *testing.T, handler http.HandlerFunc) *Gateway {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	g := New("cli_fixture_"+t.Name(), "long-secret-never-log")
	g.cfg.BaseUrl = server.URL
	return g
}
func TestAmbiguousSendDoesNotRetry(t *testing.T) {
	var sends atomic.Int32
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"test-token","expire":7200}`))
			return
		}
		sends.Add(1)
		h := w.(http.Hijacker)
		c, _, e := h.Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		c.Close()
	})
	_, e := g.SendCard(context.Background(), "chat_id", "chat", "{}", "stable-uuid")
	var unknown *Unknown
	if !errors.As(e, &unknown) || sends.Load() != 1 {
		t.Fatalf("unsafe retry: %v sends=%d", e, sends.Load())
	}
}
func TestExplicitAPIFailureKeepsOnlySafeMetadata(t *testing.T) {
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"test-token","expire":7200}`))
			return
		}
		w.Header().Set("X-Tt-Logid", "request-fixture")
		w.WriteHeader(403)
		w.Write([]byte(`{"code":99991672,"msg":"long-secret-never-log","error":{"permission_violations":[{"subject":"required:permission"}]}}`))
	})
	_, e := g.SendCard(context.Background(), "chat_id", "chat", "{}", "uuid")
	var rejected *Rejected
	if !errors.As(e, &rejected) || rejected.Code != 99991672 || len(rejected.RequiredPermissions) != 1 || strings.Contains(e.Error(), g.secret) {
		t.Fatalf("unsafe error metadata: %v", e)
	}
}
func TestBotResponseAndTenantToken(t *testing.T) {
	var auth atomic.Int32
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			auth.Add(1)
			w.Write([]byte(`{"code":0,"tenant_access_token":"test-token","expire":7200}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing tenant token")
		}
		w.Write([]byte(`{"code":0,"bot":{"open_id":"ou_fixture_bot"}}`))
	})
	id, e := g.Bot(context.Background())
	if e != nil || id != "ou_fixture_bot" || auth.Load() != 1 {
		t.Fatalf("bot identity %q %v", id, e)
	}
}
func TestCreateGroupUUIDIsQueryParameter(t *testing.T) {
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
			return
		}
		if r.URL.Query().Get("uuid") != "persistent-group-id" || r.URL.Query().Get("user_id_type") != "open_id" {
			t.Error("group UUID not in query")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if _, exists := body["uuid"]; exists {
			t.Error("ignored UUID body field used")
		}
		w.Write([]byte(`{"code":0,"data":{"chat_id":"oc_test"}}`))
	})
	id, e := g.CreateGroup(context.Background(), "test", "ou_owner", []string{"ou_owner"}, "persistent-group-id")
	if e != nil || id != "oc_test" {
		t.Fatal(e)
	}
}

func TestReactionWireAndIdentity(t *testing.T) {
	var deletes int
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
			return
		}
		if r.Method == "POST" {
			if r.URL.Path != "/open-apis/im/v1/messages/om_original/reactions" {
				t.Error("wrong reaction target")
			}
			var body struct {
				Reaction struct {
					Emoji string `json:"emoji_type"`
				} `json:"reaction_type"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Reaction.Emoji != "OnIt" {
				t.Error("wrong emoji shape")
			}
			w.Write([]byte(`{"code":0,"data":{"reaction_id":"reaction-self"}}`))
			return
		}
		if r.Method == "DELETE" && r.URL.Path == "/open-apis/im/v1/messages/om_original/reactions/reaction-self" {
			deletes++
			w.Write([]byte(`{"code":0}`))
			return
		}
		t.Error("unexpected reaction request")
	})
	id, e := g.AddReaction(context.Background(), "om_original", "OnIt")
	if e != nil || id != "reaction-self" {
		t.Fatal(e)
	}
	if e = g.DeleteReaction(context.Background(), "om_original", id); e != nil || deletes != 1 {
		t.Fatal(e)
	}
}

func TestMembersPaginationAndSecurityBoundary(t *testing.T) {
	pages := 0
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
			return
		}
		if r.URL.Path == "/open-apis/im/v1/chats/oc_test/members/is_in_chat" {
			w.Write([]byte(`{"code":0,"data":{"is_in_chat":true}}`))
			return
		}
		if r.URL.Path == "/open-apis/im/v1/chats/oc_restricted/members" {
			w.Write([]byte(`{"code":0,"data":{"trigger_security_conf_limit":true}}`))
			return
		}
		if r.URL.Query().Get("member_id_type") != "open_id" {
			t.Error("wrong identity type")
		}
		pages++
		if r.URL.Query().Get("page_token") == "" {
			w.Write([]byte(`{"code":0,"data":{"items":[{"member_id":"ou_owner"}],"has_more":true,"page_token":"opaque"}}`))
		} else {
			w.Write([]byte(`{"code":0,"data":{"items":[{"member_id":"ou_other"}],"has_more":false}}`))
		}
	})
	ids, e := g.MemberIDs(context.Background(), "oc_test")
	if e != nil || strings.Join(ids, ",") != "ou_owner,ou_other" || pages != 2 {
		t.Fatalf("truncated members %v %v", ids, e)
	}
	present, e := g.BotInChat(context.Background(), "oc_test")
	if e != nil || !present {
		t.Fatal(e)
	}
	if _, e = g.MemberIDs(context.Background(), "oc_restricted"); e == nil {
		t.Fatal("restricted member list claimed complete")
	}
}

func TestReplyCardRetainsParentAndNeverFallsBack(t *testing.T) {
	calls := 0
	reject := false
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
			return
		}
		calls++
		if r.URL.Path != "/open-apis/im/v1/messages/om_parent/reply" || r.Method != "POST" {
			t.Error("reply fell back to a normal message")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["reply_in_thread"] != false || body["uuid"] != "persistent-id" || body["msg_type"] != "interactive" {
			t.Error("reply identity or mode changed")
		}
		if reject {
			w.Write([]byte(`{"code":230011,"msg":"parent unavailable"}`))
			return
		}
		w.Write([]byte(`{"code":0,"data":{"message_id":"om_reply"}}`))
	})
	id, e := g.ReplyCard(context.Background(), "om_parent", "{}", "persistent-id")
	if e != nil || id != "om_reply" {
		t.Fatal(e)
	}
	reject = true
	if _, e = g.ReplyCard(context.Background(), "om_parent", "{}", "persistent-id"); e == nil || calls != 2 {
		t.Fatal("rejected reply was hidden or retried")
	}
}

func TestMemberListMissingPaginationOrIdentityCannotClaimCompleteness(t *testing.T) {
	for _, body := range []string{`{"code":0,"data":{"items":[{"member_id":"ou_owner"}]}}`, `{"code":0,"data":{"items":[{"member_id":"ou_owner"},{}],"has_more":false}}`} {
		g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.URL.Path, "tenant_access_token") {
				w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
				return
			}
			w.Write([]byte(body))
		})
		if ids, err := g.MemberIDs(context.Background(), "oc_test"); err == nil || ids != nil {
			t.Fatal("incomplete roster treated as complete", ids, err)
		}
	}
}
