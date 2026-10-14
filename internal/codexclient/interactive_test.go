package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"strings"
	"testing"
	"time"
)

func interactionEvent(method, id string) Event {
	p := `{"threadId":"thread","turnId":"turn","itemId":"item","startedAtMs":1,"command":"printf test","cwd":"/fixture","availableDecisions":["accept","decline","cancel"],"questions":[{"id":"choice","header":"选择","question":"怎么处理？","options":[{"label":"保留布局","description":"只修复引用"},{"label":"调整布局","description":"同时优化"}]}],"permissions":{"network":{"enabled":true},"fileSystem":{"read":["/read"],"write":["/write"]}}}`
	return Event{Generation: 1, Method: method, RequestID: json.RawMessage(id), Params: json.RawMessage(p)}
}
func TestInteractiveAnswerValidation(t *testing.T) {
	for _, tc := range []struct {
		method, decision string
		form             map[string]any
		want             string
		bad              bool
	}{
		{"item/commandExecution/requestApproval", "accept", nil, `{"decision":"accept"}`, false},
		{"item/commandExecution/requestApproval", "acceptForSession", nil, "", true},
		{"item/commandExecution/requestApproval", "", map[string]any{"threadId": "evil"}, "", true},
		{"item/tool/requestUserInput", "", map[string]any{"q_0": "0", "q_0_notes": "  Keep the full note.  "}, `{"answers":{"choice":{"answers":["保留布局","  Keep the full note.  "]}}}`, false},
		{"item/tool/requestUserInput", "", map[string]any{"q_0": "-1"}, "", true},
		{"item/tool/requestUserInput", "", map[string]any{"q_0": []string{"0", "1"}}, "", true},
		{"item/tool/requestUserInput", "", map[string]any{"q_0_notes": "\t"}, "", true},
		{"item/tool/requestUserInput", "", map[string]any{"q_0": "0", "unknown": "x"}, "", true},
		{"item/permissions/requestApproval", "decline", nil, `{"permissions":{},"scope":"turn"}`, false},
		{"item/permissions/requestApproval", "decline", map[string]any{"permissions": []any{"p_0"}}, `{"permissions":{},"scope":"turn"}`, false},
		{"item/permissions/requestApproval", "decline", map[string]any{"permissions": nil}, `{"permissions":{},"scope":"turn"}`, false},
		{"item/permissions/requestApproval", "decline", map[string]any{"unknown": nil}, "", true},
		{"item/permissions/requestApproval", "decline", map[string]any{"permissions": []string{"forged"}}, "", true},
		{"item/permissions/requestApproval", "acceptForSession", nil, "", true},
	} {
		t.Run(tc.method+tc.decision+tc.want, func(t *testing.T) {
			r, err := ParseInteractiveRequest(interactionEvent(tc.method, `"server"`))
			if err != nil {
				t.Fatal(err)
			}
			b, err := BuildInteractiveResult(r, tc.decision, tc.form)
			if tc.bad {
				if !errors.Is(err, ErrInvalidAnswer) {
					t.Fatalf("expected rejection, got %v", err)
				}
				return
			}
			if err != nil || string(b) != tc.want || !validInteractiveResult(r, b) {
				t.Fatalf("result %s err %v", b, err)
			}
		})
	}
}
func TestPermissionSubsetIsStableAndNeverExpands(t *testing.T) {
	r, err := ParseInteractiveRequest(interactionEvent("item/permissions/requestApproval", `7`))
	if err != nil {
		t.Fatal(err)
	}
	choices, _ := r.PermissionChoices()
	for i := 0; i < 10; i++ {
		again, _ := r.PermissionChoices()
		if string(again[0].Profile) != string(choices[0].Profile) {
			t.Fatal("permission mapping changed")
		}
	}
	b, err := BuildInteractiveResult(r, "", map[string]any{"permissions": []any{choices[0].Value}})
	if err != nil || !validInteractiveResult(r, b) {
		t.Fatal("subset invalid", err)
	}
	for _, bad := range []string{`{"permissions":{"network":{"enabled":false}},"scope":"turn"}`, `{"permissions":{"fileSystem":{"write":["/"]}},"scope":"turn"}`, `{"permissions":{"network":{"enabled":true}},"scope":"session"}`} {
		if validInteractiveResult(r, json.RawMessage(bad)) {
			t.Fatal("expanded or changed permissions accepted")
		}
	}
}

func TestPermissionAllButtonIgnoresOnlyVerifiedOptionalSelection(t *testing.T) {
	r, err := ParseInteractiveRequest(interactionEvent("item/permissions/requestApproval", `7`))
	if err != nil {
		t.Fatal(err)
	}
	choices, _ := r.PermissionChoices()
	expected, err := BuildInteractiveResult(r, "accept", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, selection := range []any{nil, []any{}, []string{choices[0].Value}} {
		got, err := BuildInteractiveResult(r, "accept", map[string]any{"permissions": selection})
		if err != nil || string(got) != string(expected) || !validInteractiveResult(r, got) {
			t.Fatal("allow-all changed requested native profile", err)
		}
	}
}
func TestSensitiveQuestionAndTypedIDs(t *testing.T) {
	a := NativeRequestID(json.RawMessage(`"9007199254740993"`))
	b := NativeRequestID(json.RawMessage(`9007199254740993`))
	if a == b || a == "" || b == "" {
		t.Fatal("typed IDs conflated")
	}
	e := interactionEvent("item/tool/requestUserInput", `3`)
	var p map[string]any
	json.Unmarshal(e.Params, &p)
	p["questions"] = []any{map[string]any{"id": "secret", "question": "secret", "isSecret": true}}
	e.Params, _ = json.Marshal(p)
	r, err := ParseInteractiveRequest(e)
	if err != nil || r.Kind != "sensitive" {
		t.Fatal("secret classification", err)
	}
	if _, err := BuildInteractiveResult(r, "", map[string]any{"q_0_notes": "secret"}); err == nil {
		t.Fatal("secret result accepted")
	}
}
func TestNativeInteractiveResponseDirectionAndStaleGuard(t *testing.T) {
	received := make(chan wire, 8)
	path := fixture(t, func(ctx context.Context, c *websocket.Conn, w wire) {
		if w.Method == "trigger" {
			e := interactionEvent("item/tool/requestUserInput", `9007199254740993`)
			data, _ := json.Marshal(map[string]any{"id": e.RequestID, "method": e.Method, "params": e.Params})
			c.Write(ctx, websocket.MessageText, data)
			send(ctx, c, w.ID, map[string]any{})
			return
		}
		received <- w
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Call(ctx, "trigger", nil, nil); err != nil {
		t.Fatal(err)
	}
	e := <-c.Events()
	r, err := ParseInteractiveRequest(e)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := BuildInteractiveResult(r, "", map[string]any{"q_0": "1"})
	wrong := r
	wrong.Generation++
	if err = c.RespondServerRequest(ctx, wrong, answer); !errors.Is(err, ErrRequestStale) {
		t.Fatal("wrong generation written", err)
	}
	if err = c.RespondServerRequest(ctx, r, json.RawMessage(`{"answers":{}}`)); !errors.Is(err, ErrInvalidAnswer) {
		t.Fatal("invalid result written", err)
	}
	if err = c.RespondServerRequest(ctx, r, answer); err != nil {
		t.Fatal(err)
	}
	w := <-received
	if w.Method != "" || string(w.ID) != `9007199254740993` || string(w.Result) != string(answer) {
		t.Fatal("response used wrong RPC direction or id")
	}
	if err = c.RespondServerRequest(ctx, r, answer); !errors.Is(err, ErrRequestStale) {
		t.Fatal("repeat response written", err)
	}
	select {
	case <-received:
		t.Fatal("unexpected second write")
	default:
	}
}

func TestInteractiveClosureTombstonesSurviveLateReplays(t *testing.T) {
	for _, method := range []string{"serverRequest/resolved", "turn/completed"} {
		for _, before := range []bool{false, true} {
			t.Run(fmt.Sprint(method, before), func(t *testing.T) {
				replies := make(chan wire, 8)
				path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
					if w.Method != "trigger" {
						replies <- w
						return
					}
					e := interactionEvent("item/tool/requestUserInput", `3`)
					request, _ := json.Marshal(map[string]any{"id": e.RequestID, "method": e.Method, "params": e.Params})
					closed, _ := json.Marshal(map[string]any{"method": method, "params": map[string]any{"threadId": "thread", "requestId": 3, "turn": map[string]string{"id": "turn"}}})
					if !before {
						conn.Write(ctx, websocket.MessageText, request)
					}
					conn.Write(ctx, websocket.MessageText, closed)
					conn.Write(ctx, websocket.MessageText, request)
					send(ctx, conn, w.ID, map[string]any{})
				})
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				c, err := Connect(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				if err = c.Call(ctx, "trigger", nil, nil); err != nil {
					t.Fatal(err)
				}
				e := interactionEvent("item/tool/requestUserInput", `3`)
				e.Generation = c.GenerationID()
				req, _ := ParseInteractiveRequest(e)
				answer, _ := BuildInteractiveResult(req, "", map[string]any{"q_0": "0"})
				if err = c.RespondServerRequest(ctx, req, answer); !errors.Is(err, ErrRequestStale) {
					t.Fatal("late replay resurrected closed capability", err)
				}
				select {
				case <-replies:
					t.Fatal("stale reply sent")
				default:
				}
			})
		}
	}
	if NativeRequestID(json.RawMessage(`null`)) != "" || NativeRequestID(json.RawMessage(`""`)) == "" {
		t.Fatal("null conflated with empty string id")
	}
}
func TestPermissionDenyConstraintsAndNullableProfiles(t *testing.T) {
	for _, payload := range []string{`{"network":{"enabled":true},"fileSystem":null}`, `{"network":null,"fileSystem":{"read":["/fixture"]}}`} {
		e := interactionEvent("item/permissions/requestApproval", `4`)
		var p map[string]any
		json.Unmarshal(e.Params, &p)
		p["permissions"] = json.RawMessage(payload)
		e.Params, _ = json.Marshal(p)
		r, err := ParseInteractiveRequest(e)
		if err != nil {
			t.Fatal(err)
		}
		b, err := BuildInteractiveResult(r, "accept", nil)
		if err != nil || !validInteractiveResult(r, b) {
			t.Fatal("legal null leaf rejected", err, string(b))
		}
		if validInteractiveResult(r, json.RawMessage(`{"permissions":null,"scope":"turn"}`)) {
			t.Fatal("null object accepted")
		}
	}
	e := interactionEvent("item/permissions/requestApproval", `4`)
	var p map[string]any
	json.Unmarshal(e.Params, &p)
	p["permissions"] = json.RawMessage(`{"fileSystem":{"entries":[{"access":"write","path":{"type":"path","path":"/fixture"}},{"access":"deny","path":{"type":"path","path":"/fixture/private"}}]}}`)
	e.Params, _ = json.Marshal(p)
	r, err := ParseInteractiveRequest(e)
	if err != nil {
		t.Fatal(err)
	}
	choices, _ := r.PermissionChoices()
	if len(choices) != 1 {
		t.Fatal("deny became an optional grant")
	}
	b, err := BuildInteractiveResult(r, "", map[string]any{"permissions": []string{choices[0].Value}})
	if err != nil || !strings.Contains(string(b), `"deny"`) || !validInteractiveResult(r, b) {
		t.Fatal("deny not retained", err, string(b))
	}
	if validInteractiveResult(r, json.RawMessage(`{"permissions":{"fileSystem":{"entries":[{"access":"write","path":{"type":"path","path":"/fixture"}}]}},"scope":"turn"}`)) {
		t.Fatal("removal of deny accepted")
	}
}
func TestOriginalLongOptionLabelStillValid(t *testing.T) {
	e := interactionEvent("item/tool/requestUserInput", `5`)
	r, _ := ParseInteractiveRequest(e)
	r.Questions[0].Options[0].Label = strings.Repeat("选", 1500)
	b, err := BuildInteractiveResult(r, "", map[string]any{"q_0": "0"})
	if err != nil || !validInteractiveResult(r, b) {
		t.Fatal("original option constrained by free text limit")
	}
}

func TestInteractiveClosureWhileWaitingForWriteGate(t *testing.T) {
	server := make(chan *websocket.Conn, 1)
	responses := make(chan wire, 4)
	path := fixture(t, func(ctx context.Context, conn *websocket.Conn, w wire) {
		if w.Method == "trigger" {
			server <- conn
			e := interactionEvent("item/tool/requestUserInput", `8`)
			data, _ := json.Marshal(map[string]any{"id": e.RequestID, "method": e.Method, "params": e.Params})
			conn.Write(ctx, websocket.MessageText, data)
			send(ctx, conn, w.ID, map[string]any{})
			return
		}
		responses <- w
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Connect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Call(ctx, "trigger", nil, nil); err != nil {
		t.Fatal(err)
	}
	req, _ := ParseInteractiveRequest(<-c.Events())
	answer, _ := BuildInteractiveResult(req, "", map[string]any{"q_0": "0"})
	c.writeGate <- struct{}{}
	done := make(chan error, 1)
	go func() { done <- c.RespondServerRequest(ctx, req, answer) }()
	conn := <-server
	closed, _ := json.Marshal(map[string]any{"method": "serverRequest/resolved", "params": map[string]any{"threadId": "thread", "requestId": 8}})
	if err = conn.Write(ctx, websocket.MessageText, closed); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-c.Events():
		if e.Method != "serverRequest/resolved" {
			t.Fatal("reader did not process closure")
		}
	case <-ctx.Done():
		t.Fatal("writer blocked reader")
	}
	<-c.writeGate
	if err = <-done; !errors.Is(err, ErrRequestStale) {
		t.Fatal("reply sent after writer wait and closure", err)
	}
	select {
	case <-responses:
		t.Fatal("closed result written")
	default:
	}
}
