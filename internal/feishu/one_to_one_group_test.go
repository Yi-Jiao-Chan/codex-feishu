package feishu

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type oneToOneGroupFixtureCalls struct {
	group    atomic.Int32
	bot      atomic.Int32
	members  atomic.Int32
	mutating atomic.Int32
}

func oneToOneGroupFixture(t *testing.T, group, bot, members string) (*Gateway, *oneToOneGroupFixtureCalls) {
	t.Helper()
	calls := &oneToOneGroupFixtureCalls{}
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
			return
		}
		if r.Method != http.MethodGet {
			calls.mutating.Add(1)
			http.Error(w, `{"code":99999999}`, http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/open-apis/im/v1/chats/oc_test":
			calls.group.Add(1)
			if r.URL.Query().Get("user_id_type") != "open_id" {
				t.Errorf("group lookup used wrong user_id_type: %s", r.URL.RawQuery)
			}
			w.Write([]byte(group))
		case "/open-apis/im/v1/chats/oc_test/members/is_in_chat":
			calls.bot.Add(1)
			w.Write([]byte(bot))
		case "/open-apis/im/v1/chats/oc_test/members":
			calls.members.Add(1)
			w.Write([]byte(members))
		default:
			t.Errorf("unexpected read request: %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"code":99999999}`, http.StatusNotFound)
		}
	})
	return g, calls
}

func TestOneToOneGroup(t *testing.T) {
	tests := []struct {
		name       string
		group      string
		bot        string
		members    string
		want       bool
		wantBot    int32
		wantMember int32
	}{
		{
			name:       "exactly one human and bot",
			group:      `{"code":0,"data":{"user_count":"1","bot_count":"1"}}`,
			bot:        `{"code":0,"data":{"is_in_chat":true}}`,
			members:    `{"code":0,"data":{"items":[{"member_id":"ou_user"}],"has_more":false}}`,
			want:       true,
			wantBot:    1,
			wantMember: 1,
		},
		{
			name:       "more than one human",
			group:      `{"code":0,"data":{"user_count":"2","bot_count":"1"}}`,
			wantBot:    0,
			wantMember: 0,
		},
		{
			name:       "more than one bot",
			group:      `{"code":0,"data":{"user_count":"1","bot_count":"2"}}`,
			wantBot:    0,
			wantMember: 0,
		},
		{
			name:       "bot absent",
			group:      `{"code":0,"data":{"user_count":"1","bot_count":"1"}}`,
			bot:        `{"code":0,"data":{"is_in_chat":false}}`,
			wantBot:    1,
			wantMember: 0,
		},
		{
			name:       "different sole user",
			group:      `{"code":0,"data":{"user_count":"1","bot_count":"1"}}`,
			bot:        `{"code":0,"data":{"is_in_chat":true}}`,
			members:    `{"code":0,"data":{"items":[{"member_id":"ou_other"}],"has_more":false}}`,
			wantBot:    1,
			wantMember: 1,
		},
		{
			name:       "member list has more than count",
			group:      `{"code":0,"data":{"user_count":"1","bot_count":"1"}}`,
			bot:        `{"code":0,"data":{"is_in_chat":true}}`,
			members:    `{"code":0,"data":{"items":[{"member_id":"ou_user"},{"member_id":"ou_other"}],"has_more":false}}`,
			wantBot:    1,
			wantMember: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, calls := oneToOneGroupFixture(t, tt.group, tt.bot, tt.members)
			got, err := g.IsOneToOneGroup(context.Background(), "oc_test", "ou_user")
			if err != nil || got != tt.want {
				t.Fatalf("IsOneToOneGroup() = %v, %v; want %v, nil", got, err, tt.want)
			}
			if calls.bot.Load() != tt.wantBot || calls.members.Load() != tt.wantMember || calls.mutating.Load() != 0 {
				t.Fatalf("unexpected follow-up calls: bot=%d members=%d mutating=%d", calls.bot.Load(), calls.members.Load(), calls.mutating.Load())
			}
		})
	}
}

func TestOneToOneGroupRejectsInvalidCounts(t *testing.T) {
	tests := []struct {
		name          string
		group         string
		wantStaticErr bool
	}{
		{name: "missing data", group: `{"code":0}`, wantStaticErr: true},
		{name: "missing user count", group: `{"code":0,"data":{"bot_count":"1"}}`, wantStaticErr: true},
		{name: "missing bot count", group: `{"code":0,"data":{"user_count":"1"}}`, wantStaticErr: true},
		{name: "malformed count", group: `{"code":0,"data":{"user_count":"not-a-count","bot_count":"1"}}`, wantStaticErr: true},
		{name: "negative count", group: `{"code":0,"data":{"user_count":"1","bot_count":"-1"}}`, wantStaticErr: true},
		{name: "wrong JSON count type", group: `{"code":0,"data":{"user_count":1,"bot_count":"1"}}`},
		{name: "null count", group: `{"code":0,"data":{"user_count":null,"bot_count":"1"}}`, wantStaticErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, calls := oneToOneGroupFixture(t, tt.group, `{"code":0,"data":{"is_in_chat":true}}`, `{"code":0,"data":{"items":[{"member_id":"ou_user"}],"has_more":false}}`)
			got, err := g.IsOneToOneGroup(context.Background(), "oc_test", "ou_user")
			var unknown *Unknown
			if got || !errors.As(err, &unknown) || tt.wantStaticErr && unknown.Operation != oneToOneGroupCountsUnknown {
				t.Fatalf("invalid counts accepted: %v, %v", got, err)
			}
			if calls.bot.Load() != 0 || calls.members.Load() != 0 || calls.mutating.Load() != 0 {
				t.Fatalf("invalid counts triggered follow-up calls: bot=%d members=%d mutating=%d", calls.bot.Load(), calls.members.Load(), calls.mutating.Load())
			}
		})
	}
}

func TestOneToOneGroupPropagatesReadErrors(t *testing.T) {
	tests := []struct {
		name        string
		group       string
		bot         string
		members     string
		wantBot     int32
		wantMembers int32
		wantCode    int
	}{
		{
			name:     "group permission error",
			group:    `{"code":99991672,"msg":"permission denied"}`,
			wantCode: 99991672,
		},
		{
			name:        "bot read error",
			group:       `{"code":0,"data":{"user_count":"1","bot_count":"1"}}`,
			bot:         `{"code":99991672,"msg":"permission denied"}`,
			wantBot:     1,
			wantMembers: 0,
			wantCode:    99991672,
		},
		{
			name:        "member read error",
			group:       `{"code":0,"data":{"user_count":"1","bot_count":"1"}}`,
			bot:         `{"code":0,"data":{"is_in_chat":true}}`,
			members:     `{"code":99991672,"msg":"permission denied"}`,
			wantBot:     1,
			wantMembers: 1,
			wantCode:    99991672,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, calls := oneToOneGroupFixture(t, tt.group, tt.bot, tt.members)
			got, err := g.IsOneToOneGroup(context.Background(), "oc_test", "ou_user")
			var rejected *Rejected
			if got || !errors.As(err, &rejected) || rejected.Code != tt.wantCode {
				t.Fatalf("read error not propagated: %v, %v", got, err)
			}
			if calls.bot.Load() != tt.wantBot || calls.members.Load() != tt.wantMembers || calls.mutating.Load() != 0 {
				t.Fatalf("unexpected calls after read error: bot=%d members=%d mutating=%d", calls.bot.Load(), calls.members.Load(), calls.mutating.Load())
			}
		})
	}
}

func TestOneToOneGroupRejectsIncompleteMembers(t *testing.T) {
	g, calls := oneToOneGroupFixture(t,
		`{"code":0,"data":{"user_count":"1","bot_count":"1"}}`,
		`{"code":0,"data":{"is_in_chat":true}}`,
		`{"code":0,"data":{"items":[{"member_id":"ou_user"}],"trigger_security_conf_limit":true}}`,
	)
	got, err := g.IsOneToOneGroup(context.Background(), "oc_test", "ou_user")
	if got || err == nil || !strings.Contains(err.Error(), "member list restricted by security configuration") {
		t.Fatalf("incomplete members treated as evidence: %v, %v", got, err)
	}
	if calls.bot.Load() != 1 || calls.members.Load() != 1 || calls.mutating.Load() != 0 {
		t.Fatalf("unexpected calls for incomplete members: bot=%d members=%d mutating=%d", calls.bot.Load(), calls.members.Load(), calls.mutating.Load())
	}
}

func TestOneToOneGroupHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g, calls := oneToOneGroupFixture(t,
		`{"code":0,"data":{"user_count":"1","bot_count":"1"}}`,
		`{"code":0,"data":{"is_in_chat":true}}`,
		`{"code":0,"data":{"items":[{"member_id":"ou_user"}],"has_more":false}}`,
	)
	got, err := g.IsOneToOneGroup(ctx, "oc_test", "ou_user")
	if got || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context not preserved: %v, %v", got, err)
	}
	if calls.group.Load() != 0 || calls.bot.Load() != 0 || calls.members.Load() != 0 || calls.mutating.Load() != 0 {
		t.Fatalf("canceled context made calls: group=%d bot=%d members=%d mutating=%d", calls.group.Load(), calls.bot.Load(), calls.members.Load(), calls.mutating.Load())
	}
}

func TestOneToOneGroupValidatesIDs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		chatID string
		userID string
	}{
		{name: "missing chat", userID: "ou_user"},
		{name: "missing user", chatID: "oc_test"},
		{name: "blank chat", chatID: " \t", userID: "ou_user"},
		{name: "blank user", chatID: "oc_test", userID: " \n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := New("cli_fixture_"+t.Name(), "long-secret-never-log")
			if got, err := g.IsOneToOneGroup(context.Background(), tt.chatID, tt.userID); got || err == nil {
				t.Fatalf("invalid IDs accepted: %v, %v", got, err)
			}
		})
	}
}
