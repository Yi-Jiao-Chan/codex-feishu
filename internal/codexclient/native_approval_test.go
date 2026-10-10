package codexclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Approval depends on the host's permissionRequest hooks reaching the native
// UI. Keep this separate from the ordinary native fixture opt-in: this host's
// inherited hook did not finish within the bounded diagnostic window.
func TestNativeApprovalSurvivesSilentBridgeDisconnect(t *testing.T) {
	if os.Getenv("CODEX_FEISHU_APPROVAL_TEST") != "1" {
		t.Skip("set CODEX_FEISHU_APPROVAL_TEST=1 to validate native approval after host hooks")
	}
	testNativeRequestSurvivesSilentBridgeDisconnect(t, false)
}

func TestNativeUserInputSurvivesSilentBridgeDisconnect(t *testing.T) {
	if os.Getenv("CODEX_FEISHU_NATIVE_TEST") != "1" {
		t.Skip("set CODEX_FEISHU_NATIVE_TEST=1 to create an isolated native test thread")
	}
	testNativeRequestSurvivesSilentBridgeDisconnect(t, true)
}

// Only the independent fixture owner answers, never the observing bridge.
func testNativeRequestSurvivesSilentBridgeDisconnect(t *testing.T, question bool) {
	t.Helper()
	method, tool, approval := "item/commandExecution/requestApproval", "exec_command", "untrusted"
	if question {
		method, tool, approval = "item/tool/requestUserInput", "request_user_input", "never"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	daemon, err := Discover(ctx, "auto")
	if err != nil || daemon.AppServerVersion != "0.162.0" {
		t.Fatal("verified running daemon required")
	}
	cwd := t.TempDir()
	script := filepath.Join(cwd, "m0-approval-fixture")
	if !question {
		err = os.WriteFile(script, []byte("#!/bin/sh\nprintf 'M0_APPROVAL_COMMAND_DONE\\n'\n"), 0700)
	}
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"list","data":[]}`)
			return
		}
		var request struct {
			Tools []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"tools"`
		}
		if e := json.NewDecoder(r.Body).Decode(&request); e != nil {
			t.Errorf("invalid isolated model request: %v", e)
		}
		var toolAvailable bool
		for _, offered := range request.Tools {
			toolAvailable = toolAvailable || offered.Type == "function" && offered.Name == tool
			for _, inner := range offered.Tools {
				toolAvailable = toolAvailable || offered.Name == "functions" && inner.Name == tool
			}
		}
		if !toolAvailable {
			t.Errorf("isolated model request missing %s", tool)
		}
		n := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "close")
		emit := func(v map[string]any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", v["type"], b)
			w.(http.Flusher).Flush()
		}
		emit(map[string]any{"type": "response.created", "response": map[string]string{"id": fmt.Sprint("approval-fixture-", n)}})
		var item map[string]any
		if n == 1 {
			command := "'" + strings.ReplaceAll(script, "'", "'\"'\"'") + "'"
			args, _ := json.Marshal(map[string]any{"cmd": command, "workdir": nil, "yield_time_ms": 1000, "max_output_tokens": 1000})
			if question {
				args, _ = json.Marshal(map[string]any{"questions": []map[string]any{{"id": "fixture-choice", "header": "Fixture", "question": "Choose an isolated test result.", "options": []map[string]string{{"label": "Continue", "description": "Finish the isolated test."}, {"label": "Stop", "description": "An unused test option."}}}}})
			}
			item = map[string]any{"type": "function_call", "call_id": "approval-call", "name": tool, "arguments": string(args)}
		} else {
			item = map[string]any{"type": "message", "id": "approval-final", "role": "assistant", "phase": "final_answer", "content": []map[string]any{{"type": "output_text", "text": "M0_APPROVAL_ROUTING_DONE", "annotations": []any{}}}}
			emit(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "approval-final", "role": "assistant", "phase": "final_answer", "content": []any{}}})
			emit(map[string]any{"type": "response.output_text.delta", "item_id": "approval-final", "output_index": 0, "content_index": 0, "delta": "M0_APPROVAL_ROUTING_DONE"})
		}
		emit(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		emit(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprint("approval-fixture-", n), "usage": map[string]any{"input_tokens": 0, "input_tokens_details": nil, "output_tokens": 0, "output_tokens_details": nil, "total_tokens": 0}}})
	}))
	defer fixture.Close()
	owner, err := Connect(ctx, daemon.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	bridge, err := Connect(ctx, daemon.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	provider := fmt.Sprintf("cfb_approval_fixture_%d", time.Now().UnixNano())
	var created struct {
		Thread Thread `json:"thread"`
	}
	config := map[string]any{"model_providers." + provider: map[string]any{"name": "Isolated approval fixture", "base_url": fixture.URL + "/v1", "wire_api": "responses", "requires_openai_auth": false, "supports_websockets": false, "request_max_retries": 0}, "model_reasoning_effort": "low"}
	if question {
		// 0.162.0 advertises this tool even when Default-mode handling is off.
		// Enable it only on this disposable fixture; preserve all native hooks.
		config["features.default_mode_request_user_input"] = true
	}
	if err = owner.Call(ctx, "thread/start", map[string]any{"cwd": cwd, "model": "fixture-model", "modelProvider": provider, "approvalPolicy": approval, "approvalsReviewer": "user", "sandbox": "read-only", "threadSource": "feishu.bridge.validation", "config": config}, &created); err != nil {
		t.Fatal(err)
	}
	id := created.Thread.ID
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if e := owner.Call(cleanup, "thread/archive", map[string]string{"threadId": id}, nil); e != nil {
			t.Errorf("archive isolated approval fixture: %v", e)
		}
	}()
	// First input materializes history; an empty draft cannot yet be resumed.
	if _, err = bridge.Add(ctx, id, "run the isolated harmless approval fixture", "cfb-approval-fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err = bridge.Attach(ctx, id); err != nil {
		t.Fatal(err)
	}
	awaitRequest := func(client *Client) Event {
		t.Helper()
		wait, stop := context.WithTimeout(ctx, 55*time.Second)
		defer stop()
		var requestMethods, eventMethods []string
		for {
			select {
			case event, ok := <-client.Events():
				if !ok {
					t.Fatal("client closed while waiting for approval")
				}
				eventMethods = append(eventMethods, event.Method)
				if event.Method == "hook/started" || event.Method == "hook/completed" {
					var hook struct {
						Run struct {
							EventName, Status string
							DurationMS        *int64 `json:"durationMs"`
						} `json:"run"`
					}
					if json.Unmarshal(event.Params, &hook) == nil {
						var duration int64
						if hook.Run.DurationMS != nil {
							duration = *hook.Run.DurationMS
						}
						t.Logf("isolated hook method=%s event=%s status=%s duration_ms=%d", event.Method, hook.Run.EventName, hook.Run.Status, duration)
					}
				}
				if len(event.RequestID) > 0 {
					requestMethods = append(requestMethods, event.Method)
				}
				if event.Method == method && len(event.RequestID) > 0 {
					var p struct {
						ThreadID         string          `json:"threadId"`
						IsBlocking       *bool           `json:"isBlocking"`
						AutoResolutionMS json.RawMessage `json:"autoResolutionMs"`
					}
					if json.Unmarshal(event.Params, &p) != nil || p.ThreadID != id {
						t.Fatal("request routed to wrong thread")
					}
					if question && (p.IsBlocking == nil || *p.IsBlocking || string(p.AutoResolutionMS) != "null") {
						t.Fatal("Default-mode question contract differs")
					}
					return event
				}
			case <-wait.Done():
				if turns, e := owner.Turns(ctx, id); e == nil {
					for _, turn := range turns {
						var types []string
						for _, item := range turn.Items {
							types = append(types, item.Type+":"+item.Status)
						}
						t.Logf("isolated turn status=%s item_types=%v", turn.Status, types)
					}
				}
				t.Fatalf("native request %s not observed; model calls=%d request_methods=%v event_methods=%v", method, requests.Load(), requestMethods, eventMethods)
			}
		}
	}
	ownerRequest, bridgeRequest := awaitRequest(owner), awaitRequest(bridge)
	if string(ownerRequest.RequestID) != string(bridgeRequest.RequestID) || requests.Load() != 1 {
		t.Fatal("request not shared or silent bridge auto-resolved it")
	}
	bridge.Close()
	var result any = map[string]string{"decision": "accept"}
	if question {
		result = map[string]any{"answers": map[string]any{"fixture-choice": map[string]any{"answers": []string{"Continue"}}}}
	}
	answer, _ := json.Marshal(map[string]any{"id": ownerRequest.RequestID, "result": result})
	if err = owner.conn.Write(ctx, websocket.MessageText, answer); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		turns, e := owner.Turns(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		if len(turns) == 1 && turns[0].Status == "completed" {
			var commandDone, final bool
			for _, item := range turns[0].Items {
				commandDone = commandDone || (item.Type == "commandExecution" && item.Status == "completed")
				final = final || (item.Type == "agentMessage" && item.Text == "M0_APPROVAL_ROUTING_DONE")
			}
			if (!question && !commandDone) || !final || requests.Load() != 2 {
				t.Fatal("owner answer did not complete the expected fixture and final reply")
			}
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("turn failed to resume after bridge disconnect")
		}
	}
	resolved, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	for {
		select {
		case event, ok := <-owner.Events():
			if !ok {
				t.Fatal("owner closed before request resolved")
			}
			if event.Method != "serverRequest/resolved" {
				continue
			}
			var p struct {
				ThreadID  string          `json:"threadId"`
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(event.Params, &p) == nil && p.ThreadID == id && string(p.RequestID) == string(ownerRequest.RequestID) {
				t.Logf("daemon=%s thread=%s method=%s checks=bridge queue/add, shared request/replay, no observer reply, disconnect retains callback, owner answer resumes turn, exact request resolved; App UI NOT asserted", daemon.AppServerVersion, id, method)
				return
			}
		case <-resolved.Done():
			t.Fatal("matching serverRequest/resolved was not observed")
		}
	}
}
