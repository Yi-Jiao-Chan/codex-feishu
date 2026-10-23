package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func cotFixture(t *testing.T, handler http.HandlerFunc) *Gateway {
	t.Helper()
	return apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Write([]byte(`{"code":0,"tenant_access_token":"cot-fixture-token","expire":7200}`))
			return
		}
		handler(w, r)
	})
}

func fixtureCOTEvent() COTEvent {
	return COTEvent{EventType: "RUN_STARTED", Content: `{"threadId":"thread_fixture","runId":"run_fixture"}`, Timestamp: 1713200000000}
}

func TestCreateCOTWire(t *testing.T) {
	for _, tc := range []struct {
		name, origin string
		inThread     bool
		want         map[string]any
	}{
		{"chat", "", false, map[string]any{"receive_id": "oc_fixture"}},
		{"reply", "om_origin", false, map[string]any{"receive_id": "oc_fixture", "origin_message_id": "om_origin"}},
		{"thread", "om_origin", true, map[string]any{"receive_id": "oc_fixture", "origin_message_id": "om_origin", "reply_in_thread": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			g := cotFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/open-apis/im/v1/message_cot" || r.URL.RawQuery != "receive_id_type=chat_id" {
					t.Error("wrong COT creation endpoint or target type")
				}
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil || !reflect.DeepEqual(body, tc.want) {
					t.Error("COT creation changed target, thread mode, or added an unsupported field")
				}
				w.Write([]byte(`{"code":0,"data":{"cot_id":"1234567890123456789","message_id":"om_cot_fixture"}}`))
			})
			got, err := g.CreateCOT(context.Background(), "oc_fixture", tc.origin, tc.inThread)
			if err != nil || got != (COTMessage{COTID: "1234567890123456789", MessageID: "om_cot_fixture"}) || calls.Load() != 1 {
				t.Fatalf("create COT: result=%+v error=%v calls=%d", got, err, calls.Load())
			}
		})
	}
}

func TestAppendCOTWirePreservesEventStringsAndOrder(t *testing.T) {
	events := []COTEvent{fixtureCOTEvent(), {EventType: "RUN_FINISHED", Content: `{"runId":"run_fixture","status":"done"}`, Timestamp: 1713200000001}}
	g := cotFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/open-apis/im/v1/message_cot" || r.URL.RawQuery != "" {
			t.Error("wrong COT append endpoint")
		}
		var body struct {
			MessageID string `json:"message_id"`
			COTID     string `json:"cot_id"`
			Events    []struct {
				EventType string          `json:"event_type"`
				Content   json.RawMessage `json:"content"`
				Timestamp int64           `json:"timestamp"`
			} `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body.MessageID != "om_cot_fixture" || body.COTID != "cot_fixture" || len(body.Events) != len(events) {
			t.Error("COT event binding or batch changed")
			return
		}
		for i, event := range body.Events {
			var content string
			if json.Unmarshal(event.Content, &content) != nil || content != events[i].Content || event.EventType != events[i].EventType || event.Timestamp != events[i].Timestamp {
				t.Error("COT event content is not an unchanged JSON string, or event order changed")
			}
		}
		w.Write([]byte(`{"code":0}`))
	})
	if err := g.AppendCOT(context.Background(), "om_cot_fixture", "cot_fixture", events); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteCOTWire(t *testing.T) {
	for _, reason := range []string{"done", "error"} {
		t.Run(reason, func(t *testing.T) {
			g := cotFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/open-apis/im/v1/message_cot/complete/cot_fixture" {
					t.Error("wrong COT completion endpoint")
				}
				q := r.URL.Query()
				if len(q) != 2 || q.Get("message_id") != "om_cot_fixture" || q.Get("reason") != reason {
					t.Error("COT completion identity or reason missing from query")
				}
				if r.ContentLength > 0 {
					t.Error("COT completion unexpectedly has a body")
				}
				w.Write([]byte(`{"code":0}`))
			})
			if err := g.CompleteCOT(context.Background(), "om_cot_fixture", "cot_fixture", reason); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreateCOTRequiresCompleteValidResponseIdentity(t *testing.T) {
	for _, data := range []string{
		`{}`, `null`, `{"cot_id":"cot_fixture"}`, `{"message_id":"om_fixture"}`,
		`{"cot_id":"../private-fixture-body","message_id":"om_fixture"}`,
		`{"cot_id":"cot_fixture","message_id":"om_fixture\nprivate-fixture-body"}`,
		`{"cot_id":123,"message_id":"om_fixture"}`,
	} {
		t.Run(data, func(t *testing.T) {
			var calls atomic.Int32
			g := cotFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Write([]byte(`{"code":0,"data":` + data + `}`))
			})
			got, err := g.CreateCOT(context.Background(), "oc_fixture", "", false)
			var unknown *Unknown
			if !errors.As(err, &unknown) || got != (COTMessage{}) || calls.Load() != 1 {
				t.Fatalf("invalid successful response was confirmed or retried: result=%+v error=%v calls=%d", got, err, calls.Load())
			}
			if strings.Contains(err.Error(), "private-fixture-body") {
				t.Fatal("response content leaked into error")
			}
		})
	}
}

func TestCOTFailuresAreSafeAndNeverRetried(t *testing.T) {
	operations := []struct {
		name string
		call func(*Gateway) error
	}{
		{"create", func(g *Gateway) error {
			_, err := g.CreateCOT(context.Background(), "oc_fixture", "om_origin", true)
			return err
		}},
		{"append", func(g *Gateway) error {
			return g.AppendCOT(context.Background(), "om_fixture", "cot_fixture", []COTEvent{fixtureCOTEvent()})
		}},
		{"complete", func(g *Gateway) error {
			return g.CompleteCOT(context.Background(), "om_fixture", "cot_fixture", "done")
		}},
	}
	responses := []struct {
		name, body string
		status     int
		rejected   bool
		disconnect bool
	}{
		{"rejected", `{"code":99991672,"msg":"private-fixture-body long-secret-never-log"}`, 403, true, false},
		{"server", `{"code":10013,"msg":"private-fixture-body"}`, 500, false, false},
		{"malformed", `private-fixture-body`, 200, false, false},
		{"missing-code", `{"msg":"private-fixture-body"}`, 200, false, false},
		{"ambiguous-limit", `{"code":10013}`, 429, false, false},
		{"sending", `{"code":230049}`, 400, false, false},
		{"lost-response", "", 0, false, true},
	}
	for _, operation := range operations {
		for _, response := range responses {
			t.Run(operation.name+"/"+response.name, func(t *testing.T) {
				var calls atomic.Int32
				g := cotFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if response.disconnect {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						connection.Close()
						return
					}
					w.WriteHeader(response.status)
					w.Write([]byte(response.body))
				})
				err := operation.call(g)
				var rejected *Rejected
				var unknown *Unknown
				if response.rejected && !errors.As(err, &rejected) || !response.rejected && !errors.As(err, &unknown) || calls.Load() != 1 {
					t.Fatalf("failure misclassified or retried: error=%v calls=%d", err, calls.Load())
				}
				if strings.Contains(err.Error(), "private-fixture-body") || strings.Contains(err.Error(), g.secret) {
					t.Fatal("unsafe COT error metadata")
				}
			})
		}
	}
}

func TestCOTRejectsInvalidParametersBeforeNetwork(t *testing.T) {
	valid := fixtureCOTEvent()
	invalidEvents := []COTEvent{
		{EventType: "", Content: `{}`, Timestamp: 1},
		{EventType: "RUN_STARTED\nprivate-fixture-body", Content: `{}`, Timestamp: 1},
		{EventType: "RUN_STARTED", Content: `{}`, Timestamp: 0},
		{EventType: "RUN_STARTED", Content: `{}`, Timestamp: -1},
		{EventType: "RUN_STARTED", Content: `{"private-fixture-body":`, Timestamp: 1},
		{EventType: "RUN_STARTED", Content: `[]`, Timestamp: 1},
		{EventType: "RUN_STARTED", Content: `null`, Timestamp: 1},
		{EventType: "RUN_STARTED", Content: `"private-fixture-body"`, Timestamp: 1},
		{EventType: "RUN_STARTED", Content: "{\"text\":\"\xff\"}", Timestamp: 1},
	}
	var calls atomic.Int32
	g := apiFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	checks := []func() error{
		func() error { _, err := g.CreateCOT(context.Background(), "", "", false); return err },
		func() error { _, err := g.CreateCOT(context.Background(), "oc_fixture", "", true); return err },
		func() error {
			_, err := g.CreateCOT(context.Background(), "oc_fixture", "om/invalid", false)
			return err
		},
		func() error { _, err := g.CreateCOT(context.Background(), "oc_fixture?invalid", "", false); return err },
		func() error { return g.AppendCOT(context.Background(), "", "cot_fixture", []COTEvent{valid}) },
		func() error { return g.AppendCOT(context.Background(), "om_fixture", "../invalid", []COTEvent{valid}) },
		func() error { return g.AppendCOT(context.Background(), "om_fixture", "cot_fixture", nil) },
		func() error {
			return g.AppendCOT(context.Background(), "om_fixture", "cot_fixture", make([]COTEvent, MaxCOTEvents+1))
		},
		func() error { return g.CompleteCOT(context.Background(), "", "cot_fixture", "done") },
		func() error { return g.CompleteCOT(context.Background(), "om_fixture", "cot_fixture?invalid", "done") },
		func() error { return g.CompleteCOT(context.Background(), "om_fixture", "cot_fixture", "timeout") },
		func() error { return g.CompleteCOT(context.Background(), "om_fixture", "cot_fixture", "") },
	}
	for _, event := range invalidEvents {
		checks = append(checks, func() error { return g.AppendCOT(context.Background(), "om_fixture", "cot_fixture", []COTEvent{event}) })
	}
	for i, check := range checks {
		if err := check(); err == nil || strings.Contains(err.Error(), "private-fixture-body") {
			t.Errorf("invalid parameter case %d accepted or leaked content", i)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid parameters sent %d requests", calls.Load())
	}
}

func TestCOTSerializedEventAndBatchLimits(t *testing.T) {
	var calls atomic.Int32
	g := cotFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":0}`))
	})
	event := fixtureCOTEvent()
	event.Content = `{"text":""}`
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	event.Content = `{"text":"` + strings.Repeat("x", MaxCOTEventBytes-len(encoded)) + `"}`
	encoded, _ = json.Marshal(event)
	if len(encoded) != MaxCOTEventBytes {
		t.Fatal("fixture does not reach the serialized boundary")
	}
	if err := g.AppendCOT(context.Background(), "om_fixture", "cot_fixture", []COTEvent{event}); err != nil {
		t.Fatal(err)
	}
	event.Content = strings.Replace(event.Content, `"}`, `x"}`, 1)
	if err := g.AppendCOT(context.Background(), "om_fixture", "cot_fixture", []COTEvent{event}); err == nil || calls.Load() != 1 {
		t.Fatal("oversized serialized event reached network")
	}
	batch := make([]COTEvent, MaxCOTEvents)
	for i := range batch {
		batch[i] = fixtureCOTEvent()
		batch[i].Timestamp += int64(i)
	}
	if err := g.AppendCOT(context.Background(), "om_fixture", "cot_fixture", batch); err != nil || calls.Load() != 2 {
		t.Fatalf("maximum event batch refused: %v", err)
	}
}
