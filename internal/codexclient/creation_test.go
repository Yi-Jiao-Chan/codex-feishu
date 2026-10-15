package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func creationSpec() CreationSpec {
	return CreationSpec{IntentID: strings.Repeat("a", 32), CWD: "/selected/project", ProjectID: "project-1"}
}

// Field names and union values follow the generated 0.162.1 experimental
// ThreadStartResponse/ThreadListResponse schemas, not a simulated CLI API.
func creationReply(s CreationSpec) map[string]any {
	source, _ := CreationThreadSource(s.IntentID)
	var project any
	if s.ProjectID != "" {
		project = s.ProjectID
	}
	return map[string]any{
		"thread": map[string]any{
			"id": "0198aa00-0000-7000-8000-000000000001", "cwd": s.CWD,
			"ephemeral": false, "projectId": project, "threadSource": source,
			"cliVersion": "0.162.1", "modelProvider": "inherited-provider",
			"createdAt": 1, "updatedAt": 1, "sessionId": nil, "source": "vscode",
			"status": map[string]any{"type": "idle"}, "preview": "", "turns": []any{},
		},
		"cwd": s.CWD, "model": "inherited-model", "modelProvider": "inherited-provider",
		"approvalPolicy": "on-request", "approvalsReviewer": "user",
		"sandbox":                 map[string]any{"type": "workspaceWrite", "networkAccess": false, "writableRoots": []string{s.CWD}},
		"activePermissionProfile": map[string]any{"id": ":workspace", "extends": nil},
	}
}

func creationClient(t *testing.T, handle func(context.Context, *websocket.Conn, wire)) (*Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	c, err := Connect(ctx, fixture(t, handle))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, ctx
}

func TestCreationFrozenParamsAndNativeDefaults(t *testing.T) {
	for _, project := range []string{"", "project-1"} {
		t.Run("project="+project, func(t *testing.T) {
			s := creationSpec()
			s.ProjectID = project
			frozen, err := CreationRequestJSON(s)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				calls.Add(1)
				if w.Method != "thread/start" || string(w.Params) != frozen {
					t.Error("wire request differs from journaled params")
				}
				var args map[string]any
				if json.Unmarshal(w.Params, &args) != nil {
					t.Error("invalid params")
				}
				want := map[string]any{"cwd": s.CWD, "threadSource": CreationSourcePrefix + s.IntentID, "ephemeral": false}
				if project != "" {
					want["projectId"] = project
				}
				if !reflect.DeepEqual(args, want) {
					t.Error("creation sent unexpected policy/model/config or location fields")
				}
				send(ctx, c, w.ID, creationReply(s))
			})
			r, err := c.StartCreation(ctx, s)
			if err != nil || r.Thread.ID == "" || r.CWD != s.CWD || r.Model != "inherited-model" || calls.Load() != 1 {
				t.Fatalf("creation failed: %v, calls %d", err, calls.Load())
			}
			if string(r.ApprovalPolicy) != `"on-request"` || len(r.Sandbox) == 0 || len(r.ActivePermissionProfile) == 0 {
				t.Fatal("effective native policy evidence was dropped")
			}
		})
	}
}

func TestCreationIntentMarkerAndLocalValidation(t *testing.T) {
	s := creationSpec()
	first, err := CreationThreadSource(s.IntentID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := CreationThreadSource(strings.Repeat("b", 32))
	if err != nil || first == other {
		t.Fatal("different intents share a source")
	}
	if _, err := CreationThreadSource("0198aa00-0000-7000-8000-000000000001"); err != nil {
		t.Fatal("UUID intent rejected")
	}
	var calls atomic.Int32
	c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) { calls.Add(1) })
	for _, variant := range []string{"id", "relative", "unclean", "control-cwd", "control-project", "space-project"} {
		bad := s
		switch variant {
		case "id":
			bad.IntentID = "not-a-durable-unique-intent"
		case "relative":
			bad.CWD = "relative"
		case "unclean":
			bad.CWD += "/../other"
		case "control-cwd":
			bad.CWD += "\n"
		case "control-project":
			bad.ProjectID = "project\n"
		case "space-project":
			bad.ProjectID = " project "
		}
		if _, err := CreationRequestJSON(bad); err == nil {
			t.Fatalf("invalid %s frozen", variant)
		}
		_, err := c.StartCreation(ctx, bad)
		var unknown *UnknownError
		if err == nil || errors.As(err, &unknown) {
			t.Fatalf("invalid %s dispatched or marked unknown", variant)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid local params reached RPC")
	}
}

func TestCreationIncompleteResponseIsUnknownWithEvidence(t *testing.T) {
	for _, variant := range []string{"source-missing", "source-other", "cwd", "response-cwd", "project", "ephemeral", "ephemeral-missing", "model", "provider", "approval-missing", "approval-invalid", "reviewer", "sandbox", "profile", "parent", "fork", "prior-input"} {
		t.Run(variant, func(t *testing.T) {
			s := creationSpec()
			var calls atomic.Int32
			c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				calls.Add(1)
				r := creationReply(s)
				th := r["thread"].(map[string]any)
				switch variant {
				case "source-missing":
					delete(th, "threadSource")
				case "source-other":
					th["threadSource"] = CreationSourcePrefix + strings.Repeat("b", 32)
				case "cwd":
					th["cwd"] = "/different"
				case "response-cwd":
					r["cwd"] = "/different"
				case "project":
					th["projectId"] = "different"
				case "ephemeral":
					th["ephemeral"] = true
				case "ephemeral-missing":
					delete(th, "ephemeral")
				case "model":
					delete(r, "model")
				case "provider":
					th["modelProvider"] = "different"
				case "approval-missing":
					delete(r, "approvalPolicy")
				case "approval-invalid":
					r["approvalPolicy"] = map[string]any{"granular": map[string]any{"rules": true}}
				case "reviewer":
					r["approvalsReviewer"] = "unknown"
				case "sandbox":
					r["sandbox"] = map[string]any{"type": "workspaceWrite", "networkAccess": nil}
				case "profile":
					r["activePermissionProfile"] = map[string]any{"id": nil}
				case "parent":
					th["parentThreadId"] = "parent"
				case "fork":
					th["forkedFromId"] = "parent"
				case "prior-input":
					th["preview"] = "existing"
				}
				send(ctx, c, w.ID, r)
			})
			r, err := c.StartCreation(ctx, s)
			var unknown *UnknownError
			var rejected *RPCError
			if !errors.As(err, &unknown) || errors.As(err, &rejected) || r.Thread.ID == "" || calls.Load() != 1 {
				t.Fatalf("unverified result lost, accepted, or retried: %v, calls %d", err, calls.Load())
			}
		})
	}
}

func TestCreationInheritsSupportedNativePolicies(t *testing.T) {
	for _, sandbox := range []string{"readOnly", "workspaceWrite", "externalSandbox", "dangerFullAccess"} {
		t.Run(sandbox, func(t *testing.T) {
			s := creationSpec()
			s.ProjectID = ""
			c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				r := creationReply(s)
				r["sandbox"] = map[string]any{"type": sandbox}
				r["approvalPolicy"] = map[string]any{"granular": map[string]any{"mcp_elicitations": true, "rules": true, "sandbox_approval": true}}
				// Omitted projectId in params leaves native assignment to defaults.
				r["thread"].(map[string]any)["projectId"] = "native-default-project"
				send(ctx, c, w.ID, r)
			})
			if _, err := c.StartCreation(ctx, s); err != nil {
				t.Fatalf("valid inherited policy rejected: %v", err)
			}
		})
	}
}

func TestCreationRejectAndTransportUnknownAreNotRetried(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport", true: "reject"}[reject], func(t *testing.T) {
			var calls atomic.Int32
			c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				calls.Add(1)
				if !reject {
					c.CloseNow()
					return
				}
				b, _ := json.Marshal(map[string]any{"id": w.ID, "error": map[string]any{"code": -32602, "message": "invalid project"}})
				c.Write(ctx, websocket.MessageText, b)
			})
			_, err := c.StartCreation(ctx, creationSpec())
			var unknown *UnknownError
			var rejected *RPCError
			if errors.As(err, &unknown) == reject || errors.As(err, &rejected) != reject || calls.Load() != 1 {
				t.Fatalf("incorrect outcome or retry: %v, calls %d", err, calls.Load())
			}
		})
	}
}

func projectFixture(id string) map[string]any {
	return map[string]any{"id": id, "name": "Project", "roots": []map[string]string{{"path": "/selected/project"}}, "metadata": map[string]string{}, "position": 0, "createdAt": 1, "updatedAt": 1, "recencyAt": nil}
}

func TestCreationProjectPagination(t *testing.T) {
	var calls atomic.Int32
	c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		calls.Add(1)
		var p map[string]any
		json.Unmarshal(w.Params, &p)
		if w.Method != "project/list" || p["sortKey"] != "position" || p["sortDirection"] != "asc" || p["limit"] != float64(100) {
			t.Error("incorrect schema project list params")
		}
		switch p["cursor"] {
		case nil:
			send(ctx, c, w.ID, map[string]any{"data": []any{projectFixture("p1")}, "nextCursor": "opaque+/one="})
		case "opaque+/one=":
			send(ctx, c, w.ID, map[string]any{"data": []any{}, "nextCursor": "opaque/two"})
		case "opaque/two":
			send(ctx, c, w.ID, map[string]any{"data": []any{projectFixture("p2")}, "nextCursor": nil})
		default:
			t.Error("cursor modified")
		}
	})
	projects, err := c.ListProjects(ctx)
	if err != nil || len(projects) != 2 || projects[0].ID != "p1" || projects[1].ID != "p2" || projects[0].Roots[0].Path != "/selected/project" || calls.Load() != 3 {
		t.Fatalf("pagination failed: %v, calls %d", err, calls.Load())
	}
}

func TestCreationProjectPaginationLoop(t *testing.T) {
	var calls atomic.Int32
	c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		calls.Add(1)
		send(ctx, c, w.ID, map[string]any{"data": []any{projectFixture("p1")}, "nextCursor": "same"})
	})
	projects, err := c.ListProjects(ctx)
	if err == nil || projects != nil || calls.Load() != 2 {
		t.Fatal("repeating cursor accepted or returned partial data")
	}
}

func TestCreationProjectInvalidResults(t *testing.T) {
	for _, variant := range []string{"missing-data", "null-data", "missing-id", "missing-roots", "missing-metadata", "relative-root"} {
		t.Run(variant, func(t *testing.T) {
			c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				p := projectFixture("p1")
				r := map[string]any{"data": []any{p}, "nextCursor": nil}
				switch variant {
				case "missing-data":
					delete(r, "data")
				case "null-data":
					r["data"] = nil
				case "missing-id":
					delete(p, "id")
				case "missing-roots":
					delete(p, "roots")
				case "missing-metadata":
					delete(p, "metadata")
				case "relative-root":
					p["roots"] = []map[string]string{{"path": "relative"}}
				}
				send(ctx, c, w.ID, r)
			})
			projects, err := c.ListProjects(ctx)
			if err == nil || projects != nil {
				t.Fatal("invalid project metadata accepted")
			}
		})
	}
}

func TestCreationSourceSurvivesHistoryDTO(t *testing.T) {
	s := creationSpec()
	c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		if w.Method != "thread/list" {
			t.Error("unexpected history lookup")
		}
		send(ctx, c, w.ID, map[string]any{"data": []any{creationReply(s)["thread"]}, "nextCursor": nil})
	})
	threads, err := c.List(ctx)
	if err != nil || len(threads) != 1 {
		t.Fatal("history lookup failed", err)
	}
	var source string
	if json.Unmarshal(threads[0].ThreadSource, &source) != nil || source != CreationSourcePrefix+s.IntentID {
		t.Fatal("exact intent marker lost from history DTO")
	}
	// This fixture proves DTO/framing retention only. It does not establish
	// daemon persistence or absence of a thread when history omits the marker.
}

func TestCreationReadResumesExactThreadWithoutOverrides(t *testing.T) {
	s := creationSpec()
	const id = "0198aa00-0000-7000-8000-000000000001"
	var calls atomic.Int32
	c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		calls.Add(1)
		var args map[string]any
		if json.Unmarshal(w.Params, &args) != nil || w.Method != "thread/resume" || !reflect.DeepEqual(args, map[string]any{"threadId": id, "excludeTurns": true}) {
			t.Error("creation verification sent extra policy/config/location params or started a thread")
		}
		r := creationReply(s)
		thread := r["thread"].(map[string]any)
		thread["preview"] = "already materialized"
		thread["turns"] = []any{map[string]any{"id": "existing", "status": "completed", "items": []any{}}}
		r["approvalPolicy"] = "never"
		r["sandbox"] = map[string]any{"type": "dangerFullAccess"}
		send(ctx, c, w.ID, r)
	})
	r, err := c.ReadCreation(ctx, s, id)
	if err != nil || r.Thread.ID != id || r.Thread.Preview == "" || len(r.Thread.Turns) != 1 || calls.Load() != 1 {
		t.Fatal("valid recovered thread could not be verified", err)
	}
}

func TestCreationReadRequiresIdentityAndEffectivePolicies(t *testing.T) {
	for _, variant := range []string{"id", "cwd", "response-cwd", "source", "project", "ephemeral", "model", "provider", "approval", "sandbox", "profile", "reviewer"} {
		t.Run(variant, func(t *testing.T) {
			s := creationSpec()
			var calls atomic.Int32
			c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				calls.Add(1)
				r := creationReply(s)
				thread := r["thread"].(map[string]any)
				switch variant {
				case "id":
					thread["id"] = "other"
				case "cwd":
					thread["cwd"] = "/other"
				case "response-cwd":
					r["cwd"] = "/other"
				case "source":
					thread["threadSource"] = nil
				case "project":
					thread["projectId"] = nil
				case "ephemeral":
					thread["ephemeral"] = true
				case "model":
					delete(r, "model")
				case "provider":
					delete(r, "modelProvider")
				case "approval":
					delete(r, "approvalPolicy")
				case "sandbox":
					delete(r, "sandbox")
				case "profile":
					r["activePermissionProfile"] = map[string]any{}
				case "reviewer":
					delete(r, "approvalsReviewer")
				}
				send(ctx, c, w.ID, r)
			})
			r, err := c.ReadCreation(ctx, s, "0198aa00-0000-7000-8000-000000000001")
			var unknown *UnknownError
			if !errors.As(err, &unknown) || unknown.Method != "thread/resume" || calls.Load() != 1 || r.Thread.ID == "" {
				t.Fatal("unproven recovery accepted, retried or evidence dropped", err)
			}
		})
	}
}

func TestCreationReadRemoteRejectionAndLossRemainUnknown(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport", true: "resume-rejected"}[reject], func(t *testing.T) {
			var calls atomic.Int32
			c, ctx := creationClient(t, func(ctx context.Context, c *websocket.Conn, w wire) {
				calls.Add(1)
				if w.Method != "thread/resume" {
					t.Error("verification created replacement thread")
				}
				if !reject {
					c.CloseNow()
					return
				}
				b, _ := json.Marshal(map[string]any{"id": w.ID, "error": map[string]any{"code": -32600, "message": "before rollout exists"}})
				c.Write(ctx, websocket.MessageText, b)
			})
			_, err := c.ReadCreation(ctx, creationSpec(), "known-native-thread")
			unknown, ok := err.(*UnknownError)
			if !ok || unknown.Method != "thread/resume" || calls.Load() != 1 {
				t.Fatal("resume failure rejected original creation or retried", err)
			}
		})
	}
}
