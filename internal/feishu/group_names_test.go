package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGroupNameWirePreservesOtherFields(t *testing.T) {
	name := strings.Repeat("完整标题", 25)
	var calls atomic.Int32
	actual := "original"
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
			return
		}
		calls.Add(1)
		if r.URL.Path != "/open-apis/im/v1/chats/oc_fixture" || r.URL.Query().Get("user_id_type") != "open_id" {
			t.Error("group identity/query changed")
		}
		if r.Method == "PUT" {
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if len(payload) != 1 || payload["name"] != name {
				t.Error("update changed extra fields or truncated name")
			}
			actual = payload["name"]
			w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		if r.Method != "GET" {
			t.Error("unexpected method")
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"name": actual, "owner_id": "ou_owner", "chat_mode": "group", "description": "工作目录：/original/worktree"}})
	})
	ctx := context.Background()
	if err := g.UpdateGroupName(ctx, "oc_fixture", name); err != nil {
		t.Fatal(err)
	}
	info, err := g.GetGroupInfo(ctx, "oc_fixture")
	if err != nil || info.Name != name || info.OwnerID != "ou_owner" || info.Description != "工作目录：/original/worktree" || info.ChatMode != "group" || calls.Load() != 2 {
		t.Fatal(info, err, calls.Load())
	}
}

func TestGroupNameIncompleteResponsesRemainUnknown(t *testing.T) {
	for _, tc := range []struct{ method, body string }{
		{"GET", `{"code":0,"data":{"name":"title","owner_id":"owner","chat_mode":"group"}}`},
		{"GET", `{"code":0,"data":null}`},
		{"GET", `{"code":0,"data":{"name":"title","owner_id":"","chat_mode":"group","description":""}}`},
		{"PUT", `{"code":0}`}, {"PUT", `{"code":0,"data":null}`}, {"PUT", `{"code":0,"data":[]}`},
	} {
		t.Run(tc.method+tc.body, func(t *testing.T) {
			g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
					return
				}
				w.Write([]byte(tc.body))
			})
			var err error
			if tc.method == "GET" {
				_, err = g.GetGroupInfo(context.Background(), "chat")
			} else {
				err = g.UpdateGroupName(context.Background(), "chat", "title")
			}
			var unknown *Unknown
			if !errors.As(err, &unknown) {
				t.Fatal("incomplete success trusted", err)
			}
		})
	}
}

func TestGroupNameValidationAndAmbiguousMutation(t *testing.T) {
	var calls atomic.Int32
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
			return
		}
		calls.Add(1)
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		c.Close()
	})
	ctx := context.Background()
	for _, args := range [][2]string{{"", "title"}, {"chat", " "}, {"chat", string([]byte{0xff})}} {
		if err := g.UpdateGroupName(ctx, args[0], args[1]); err == nil {
			t.Fatal("invalid update accepted")
		}
	}
	if _, err := g.GetGroupInfo(ctx, ""); err == nil {
		t.Fatal("invalid read accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input called API")
	}
	err := g.UpdateGroupName(ctx, "chat", "private title")
	var unknown *Unknown
	if !errors.As(err, &unknown) || calls.Load() != 1 || strings.Contains(err.Error(), "private title") {
		t.Fatal("ambiguous update retried or leaked", err, calls.Load())
	}
}

func TestGroupNameOnlyGenericRateLimitIsRetryable(t *testing.T) {
	for _, code := range []int{99991400, 230020, 99991403} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
					return
				}
				w.Header().Set("x-ogw-ratelimit-reset", "5")
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": "private title"})
			})
			err := g.UpdateGroupName(context.Background(), "chat", "title")
			var limited *RateLimited
			if errors.As(err, &limited) != (code == 99991400) {
				t.Fatal("unsafe rate classification", err)
			}
			if strings.Contains(err.Error(), "private title") {
				t.Fatal("response body leaked")
			}
		})
	}
}
