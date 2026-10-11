package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCardMessageReadbackSelectsExactMessageAndRequestsOriginalCard(t *testing.T) {
	calls := 0
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture","expire":7200}`))
			return
		}
		calls++
		if r.Method != "GET" || r.URL.Path != "/open-apis/im/v1/messages/om_original" || r.URL.Query().Get("card_msg_content_type") != "user_card_content" {
			t.Fatal("wrong readback request", r.Method, r.URL)
		}
		w.Write([]byte(`{"code":0,"data":{"items":[{"message_id":"om_child","msg_type":"text"},{"message_id":"om_original","chat_id":"oc_original","msg_type":"interactive","sender":{"id":"cli_original","id_type":"app_id","sender_type":"app"},"deleted":false,"parent_id":"om_parent","body":{"content":"{\"schema\":\"2.0\"}"}}]}}`))
	})
	m, err := g.ReadCardMessage(context.Background(), "om_original")
	if err != nil || calls != 1 || m.ID != "om_original" || m.Type != "interactive" || m.SenderID != "cli_original" || m.Deleted == nil || *m.Deleted || m.ParentID == nil || *m.ParentID != "om_parent" || m.Content != `{"schema":"2.0"}` {
		t.Fatal(m, err, calls)
	}
	encoded, _ := json.Marshal(m)
	if string(encoded) != "{}" {
		t.Fatal("private evidence exposed by serialization")
	}
}

func TestCardReadbackMissingDuplicateAndIncompletePagesDoNotInventEvidence(t *testing.T) {
	for _, body := range []string{
		`{"code":0,"data":{"items":[]}}`,
		`{"code":0,"data":{"items":[{"message_id":"om_one"},{"message_id":"om_one"}]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					w.Write([]byte(`{"code":0,"tenant_access_token":"fixture","expire":7200}`))
					return
				}
				w.Write([]byte(body))
			})
			_, err := g.ReadCardMessage(context.Background(), "om_one")
			var unknown *Unknown
			if !errors.As(err, &unknown) {
				t.Fatal("missing/duplicate identity accepted", err)
			}
		})
	}
	for _, body := range []string{
		`{"code":0,"data":{"items":[]}}`,
		`{"code":0,"data":{"items":[],"has_more":true}}`,
		`{"code":0,"data":{"items":[],"has_more":true,"page_token":"same"}}`,
	} {
		t.Run("page"+body, func(t *testing.T) {
			g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "tenant_access_token") {
					w.Write([]byte(`{"code":0,"tenant_access_token":"fixture","expire":7200}`))
					return
				}
				w.Write([]byte(body))
			})
			_, err := g.ListCardMessages(context.Background(), "chat", "oc_one", "same")
			var unknown *Unknown
			if !errors.As(err, &unknown) {
				t.Fatal("incomplete page accepted", err)
			}
		})
	}
}

func TestCardMessagePagesPreserveUnknownFieldsAndStrictQuery(t *testing.T) {
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture","expire":7200}`))
			return
		}
		q := r.URL.Query()
		if r.Method != "GET" || q.Get("container_id_type") != "thread" || q.Get("container_id") != "omt_one" || q.Get("page_size") != "50" || q.Get("sort_type") != "ByCreateTimeDesc" || q.Get("page_token") != "cursor" || q.Get("card_msg_content_type") != "user_card_content" || q.Has("start_time") || q.Has("end_time") {
			t.Fatal("wrong list query", r.Method, q)
		}
		w.Write([]byte(`{"code":0,"data":{"items":[{"message_id":"om_one","msg_type":"interactive"}],"has_more":false}}`))
	})
	p, err := g.ListCardMessages(context.Background(), "thread", "omt_one", "cursor")
	if err != nil || p.HasMore || len(p.Items) != 1 || p.Items[0].Deleted != nil || p.Items[0].ParentID != nil {
		t.Fatal("missing facts invented", p, err)
	}
}
