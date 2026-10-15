package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTopicReplyFreezesModeAndNeverFallsBack(t *testing.T) {
	calls := 0
	reject := false
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"fixture-token","expire":7200}`))
			return
		}
		calls++
		if r.Method != "POST" || r.URL.Path != "/open-apis/im/v1/messages/root/reply" {
			t.Error("topic escaped reply endpoint")
		}
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		if b["reply_in_thread"] != true || b["uuid"] != "attempt" {
			t.Error("topic identity missing")
		}
		if reject {
			w.WriteHeader(400)
			w.Write([]byte(`{"code":230011}`))
			return
		}
		w.Write([]byte(`{"code":0,"data":{"message_id":"reply","thread_id":"platform-topic"}}`))
	})
	result, err := g.ReplyTopicCardResult(context.Background(), "root", "{}", "attempt")
	if err != nil || result.MessageID != "reply" || result.ThreadID != "platform-topic" {
		t.Fatal(result, err)
	}
	reject = true
	if _, err = g.ReplyTopicCard(context.Background(), "root", "{}", "attempt"); !TopicTargetRejected(err) || calls != 2 {
		t.Fatal("rejected topic retried/fell back", calls, err)
	}
	for _, err := range []error{&RateLimited{Rejection: &Rejected{Code: 230020}, RetryAfter: time.Second}, &Rejected{Code: 230099}, &Unknown{Operation: "reply"}} {
		if TopicTargetRejected(err) {
			t.Fatal("transient/content error marked repair", err)
		}
	}
}
