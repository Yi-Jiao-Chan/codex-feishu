package sync

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type cotOutboundCall struct {
	step, chat, origin, message, cot, reason string
	inThread                                 bool
	events                                   []feishu.COTEvent
}

type cotOutboundGateway struct {
	cardGateway
	s           *store.Store
	t           *testing.T
	cotCalls    []cotOutboundCall
	failStep    string
	failure     error
	rootReads   int
	rootDeleted bool
	onCOTWrite  func(context.Context)
}

func (g *cotOutboundGateway) writesCount() int { return len(g.writes) + len(g.cotCalls) }

func (g *cotOutboundGateway) recordCOT(ctx context.Context, call cotOutboundCall) error {
	g.t.Helper()
	var id string
	if err := g.s.DB.QueryRowContext(ctx, `SELECT attempt_id FROM outbound_attempts WHERE host_id='host' AND thread_id='thread' AND state='dispatching' AND cot_step=?`, call.step).Scan(&id); err != nil {
		g.t.Fatal("COT write without durable claim", err)
	}
	a, err := g.s.MirrorAttempt(ctx, id)
	if err != nil || a.COTStep != call.step {
		g.t.Fatal("COT claim unavailable", err)
	}
	if call.step == "create" {
		if a.ChatID != call.chat || a.ParentMessageID != call.origin || (a.FollowMode == "topic") != call.inThread {
			g.t.Fatal("COT create escaped its frozen target")
		}
	} else if a.TargetMessageID != call.message || a.COTID != call.cot {
		g.t.Fatal("COT mutation changed the stable remote identity")
	}
	if call.step == "append" {
		var frozen []feishu.COTEvent
		if json.Unmarshal([]byte(a.COTEvents), &frozen) != nil || !reflect.DeepEqual(call.events, frozen) {
			g.t.Fatal("COT append did not use the exact frozen events")
		}
		if len(call.events) == 0 || len(call.events) > feishu.MaxCOTEvents {
			g.t.Fatal("COT append batch limit violated")
		}
		for _, event := range call.events {
			wire, err := json.Marshal(event)
			if err != nil || len(wire) > feishu.MaxCOTEventBytes || !json.Valid([]byte(event.Content)) || strings.Contains(event.Content, "PRIVATE_") {
				g.t.Fatal("invalid, oversized, or private COT event")
			}
		}
	}
	if g.onCOTWrite != nil {
		g.onCOTWrite(ctx)
	}
	call.events = append([]feishu.COTEvent(nil), call.events...)
	g.cotCalls = append(g.cotCalls, call)
	if call.step == g.failStep {
		return g.failure
	}
	return nil
}

func (g *cotOutboundGateway) CreateCOT(ctx context.Context, chat, origin string, inThread bool) (feishu.COTMessage, error) {
	err := g.recordCOT(ctx, cotOutboundCall{step: "create", chat: chat, origin: origin, inThread: inThread})
	return feishu.COTMessage{COTID: "cot_fixture", MessageID: "om_cot_fixture"}, err
}

func (g *cotOutboundGateway) AppendCOT(ctx context.Context, message, id string, events []feishu.COTEvent) error {
	return g.recordCOT(ctx, cotOutboundCall{step: "append", message: message, cot: id, events: events})
}

func (g *cotOutboundGateway) CompleteCOT(ctx context.Context, message, id, reason string) error {
	return g.recordCOT(ctx, cotOutboundCall{step: "complete", message: message, cot: id, reason: reason})
}

func (g *cotOutboundGateway) ReplyTopicCard(_ context.Context, parent, card, _ string) (string, error) {
	return g.created("topic_reply", parent, card)
}

func (g *cotOutboundGateway) ReadCardMessage(_ context.Context, id string) (feishu.CardMessage, error) {
	if id != "root" {
		g.t.Fatal("COT guard read a message other than its bound topic root")
	}
	g.rootReads++
	_, root := validTopicRoot()
	root.ID, root.Deleted = id, &g.rootDeleted
	return root, nil
}

type cotOutboundFixture struct {
	e    Engine
	path string
	turn codexclient.Turn
	g    *cotOutboundGateway
	o    Outbound
	now  int64
}

func newCOTOutboundFixture(t *testing.T, topic bool, text string) *cotOutboundFixture {
	t.Helper()
	e, path := engineFixture(t)
	e.NativeCOT = true
	bindEngine(t, e)
	if topic {
		makeTopic(t, e.Store)
	}
	turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{
		input("user", "synthetic request"), agent("public", text, "commentary"),
		{ID: "reasoning", Type: "reasoning", Text: "PRIVATE_REASONING"},
		{ID: "tool", Type: "mcpToolCall", Server: "fixture", Tool: "inspect", Status: "inProgress", Text: "PRIVATE_TOOL_OUTPUT"},
	}}
	apply(t, e, 200, false, turn)
	m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if _, native, parseErr := cot.Parse(m.Card); err != nil || parseErr != nil || !native {
		t.Fatal("NativeCOT engine did not stage a native document", err, parseErr)
	}
	g := &cotOutboundGateway{s: e.Store, t: t}
	f := &cotOutboundFixture{e: e, path: path, turn: turn, g: g, o: Outbound{Store: e.Store, Gateway: g}, now: 1000}
	f.dispatch(t, "") // The actual initiating human mirror must exist first.
	return f
}

func (f *cotOutboundFixture) dispatch(t *testing.T, step string) DeliveryResult {
	t.Helper()
	before := f.g.writesCount()
	r, err := f.o.DispatchOne(context.Background(), "host", "thread", f.now)
	f.now += 3000
	if err != nil || r.AttemptID == "" || r.State != "confirmed" || f.g.writesCount() != before+1 {
		t.Fatalf("dispatch %s: result=%+v error=%v writes=%d->%d", step, r, err, before, f.g.writesCount())
	}
	a, err := f.e.Store.MirrorAttempt(context.Background(), r.AttemptID)
	if err != nil || a.COTStep != step {
		t.Fatalf("wrong dispatch step: got=%s want=%s error=%v", a.COTStep, step, err)
	}
	return r
}

func (f *cotOutboundFixture) terminal(t *testing.T) {
	t.Helper()
	f.turn.Status = "completed"
	for i := range f.turn.Items {
		if f.turn.Items[i].ID == "tool" {
			f.turn.Items[i].Status = "completed"
		}
	}
	f.turn.Items = append(f.turn.Items, agent("final", "synthetic final answer", "final_answer"))
	apply(t, f.e, 300, false, f.turn)
}

func prepareCOTOutboundStep(t *testing.T, step string, topic bool) *cotOutboundFixture {
	t.Helper()
	f := newCOTOutboundFixture(t, topic, "synthetic public progress")
	if step == "create" {
		return f
	}
	f.dispatch(t, "create")
	if step == "append" {
		return f
	}
	f.dispatch(t, "append")
	f.terminal(t)
	f.dispatch(t, "append")
	return f
}

func (f *cotOutboundFixture) recover(t *testing.T) {
	t.Helper()
	if err := f.e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.RecoverMirrorDispatches(context.Background(), "host"); err != nil {
		t.Fatal(err)
	}
	f.e.Store, f.o.Store, f.g.s = s, s, s
}

func (f *cotOutboundFixture) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 100; i++ {
		before := f.g.writesCount()
		r, err := f.o.DispatchOne(context.Background(), "host", "thread", f.now)
		f.now += 3000
		if err != nil || f.g.writesCount()-before > 1 || r.AttemptID != "" && r.State != "confirmed" {
			t.Fatalf("unsafe COT drain: %+v %v", r, err)
		}
		if r.AttemptID == "" {
			if f.g.writesCount() != before {
				t.Fatal("remote write without a returned attempt")
			}
			return
		}
	}
	t.Fatal("COT outbound did not settle")
}

func TestCOTOutboundToolDetailsSurvivePersistenceAndRestart(t *testing.T) {
	f := newCOTOutboundFixture(t, false, "synthetic progress")
	f.turn.Items[3] = codexclient.Item{
		ID: "tool", Type: "commandExecution", Status: "inProgress", Command: "cat /project/first.go /project/second.go --token PRIVATE_ARGUMENT", CWD: "/project",
		CommandActions: []codexclient.CommandAction{{Type: "read", Path: "/PRIVATE_DIRECTORY/first.go"}, {Type: "read", Path: "/PRIVATE_DIRECTORY/second.go"}},
		Text:           "PRIVATE_OUTPUT",
	}
	f.turn.Items = append(f.turn.Items, codexclient.Item{ID: "failed-tool", Type: "commandExecution", Status: "inProgress", Command: "go test ./... -run SyntheticCase"})
	apply(t, f.e, 201, false, f.turn)
	f.drain(t)
	f.recover(t)
	zero, failure, elapsed := 0, 2, int64(25)
	output, failedOutput := "first.go: synthetic source\nsecond.go: synthetic source\naccess_token=PRIVATE_OUTPUT", "FAIL SyntheticCase: expected 2, got 3"
	f.turn.Items[3].AggregatedOutput, f.turn.Items[4].AggregatedOutput = &output, &failedOutput
	f.turn.Items[3].ExitCode, f.turn.Items[3].DurationMS = &zero, &elapsed
	f.turn.Items[4].Status, f.turn.Items[4].ExitCode = "failed", &failure
	f.terminal(t)
	f.drain(t)
	starts, results := make(map[string]bool), make(map[string]map[string]any)
	for _, call := range f.g.cotCalls {
		for _, event := range call.events {
			var content map[string]any
			if err := json.Unmarshal([]byte(event.Content), &content); err != nil {
				t.Fatal(err)
			}
			switch event.EventType {
			case "TOOL_CALL_START":
				id := content["toolCallId"].(string)
				if starts[id] || content["toolCallName"] != "commandExecution" {
					t.Fatal("tool identity changed or invocation was duplicated")
				}
				starts[id] = true
			case "TOOL_CALL_RESULT":
				id := content["toolCallId"].(string)
				if !starts[id] || results[id] != nil {
					t.Fatal("tool detail lost its invocation or was repeated")
				}
				results[id] = content
			}
		}
	}
	if len(starts) != 2 || len(results) != 2 {
		t.Fatal("missing independent native tools and expandable details")
	}
	for _, result := range results {
		body := result["content"].(string)
		if strings.Contains(body, "仅同步状态") || strings.Contains(body, "原始输出未同步") || strings.Contains(body, "用途未识别") {
			t.Fatal("normal detail contained legacy diagnostic boilerplate")
		}
		if result["exitCode"] == float64(0) {
			if result["isError"] != false || result["durationMs"] != float64(25) || !strings.Contains(body, "cat first.go second.go") || !strings.Contains(body, "first.go: synthetic source") || !strings.Contains(body, "second.go: synthetic source") {
				t.Fatal("completed tool lost safe file details or native execution facts")
			}
		} else if result["exitCode"] != float64(2) || result["isError"] != true || !strings.Contains(body, "执行失败") || !strings.Contains(body, failedOutput) {
			t.Fatal("failed tool result was misrepresented")
		}
	}
	before := f.g.writesCount()
	apply(t, f.e, 400, false, f.turn)
	f.drain(t)
	if f.g.writesCount() != before {
		t.Fatal("replayed terminal history repeated completed tool details")
	}
}

func TestCOTOutboundLegacyDocumentKeepsPresentationAfterUpgradeAndRestart(t *testing.T) {
	f := newCOTOutboundFixture(t, false, "synthetic progress")
	legacy, err := cotDocumentVersion("thread", f.turn, nil, 200, cot.LegacyVersion)
	if err != nil {
		t.Fatal(err)
	}
	// Seed the persisted document produced before presentation version 2.
	if _, err := f.e.Store.DB.Exec(`UPDATE mirrors SET card_json=? WHERE host_id='host' AND thread_id='thread' AND mirror_key=?`, legacy, mirrorKey("progress", "turn")); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	f.recover(t)
	f.e.NativeCOT = false // An existing native stream must still finish normally.
	elapsed := int64(25)
	f.turn.Items[3].DurationMS = &elapsed
	f.terminal(t)
	m, err := f.e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	doc, native, parseErr := cot.Parse(m.Card)
	if err != nil || parseErr != nil || !native || doc.Version != cot.LegacyVersion {
		t.Fatal("existing COT document changed presentation version", err, parseErr)
	}
	f.drain(t)
	results := 0
	for _, call := range f.g.cotCalls {
		for _, event := range call.events {
			if event.EventType != "TOOL_CALL_RESULT" {
				continue
			}
			results++
			var content map[string]any
			if err := json.Unmarshal([]byte(event.Content), &content); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(content["content"].(string), "已完成（仅同步状态，原始输出未同步）") || content["durationMs"] != nil || content["isError"] != nil {
				t.Fatal("legacy acknowledged stream received new detail rendering")
			}
		}
	}
	if results != 1 {
		t.Fatal("legacy stream did not finish exactly once")
	}
}

func TestCOTOutboundEngineRunsChunkedLifecycleOnOneMessage(t *testing.T) {
	f := newCOTOutboundFixture(t, false, strings.Repeat("公开进度😀<>&", 10000))
	grants, releases := 0, 0
	activeLease := false
	f.o.BeginWrite = func(ctx context.Context, b store.Binding) (context.Context, func(), error) {
		if b.HostID != "host" || b.ThreadID != "thread" || b.ChatID != "chat" || activeLease {
			t.Fatal("COT lease used wrong scope or overlapped")
		}
		grants++
		activeLease = true
		return ctx, func() {
			var dispatching int
			if err := f.e.Store.DB.QueryRow(`SELECT COUNT(*) FROM outbound_attempts WHERE state='dispatching'`).Scan(&dispatching); err != nil || dispatching != 0 {
				t.Fatal("COT lease released before local confirmation", err)
			}
			activeLease = false
			releases++
		}, nil
	}
	f.g.onCOTWrite = func(context.Context) {
		if !activeLease {
			t.Fatal("COT write without its active lease")
		}
	}
	f.drain(t)
	appends := 0
	lastTimestamp := int64(0)
	for _, call := range f.g.cotCalls {
		if call.step == "append" {
			appends++
			for _, event := range call.events {
				if event.Timestamp <= lastTimestamp {
					t.Fatal("event chunks were reordered or retimed")
				}
				lastTimestamp = event.Timestamp
			}
		}
	}
	if appends < 2 || len(f.g.cotCalls) != appends+1 || f.g.cotCalls[0].step != "create" || f.g.cotCalls[0].origin != f.g.writes[0].ID {
		t.Fatal("long progress did not use one create and multiple bounded append batches")
	}
	f.terminal(t)
	f.drain(t)
	m, err := f.e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if err != nil || m.RemoteID != "om_cot_fixture" || m.State != "confirmed" || m.DeliveredRevision != m.Revision {
		t.Fatal("terminal COT changed identity or did not settle", m, err)
	}
	last := f.g.cotCalls[len(f.g.cotCalls)-1]
	if last.step != "complete" || last.reason != "done" || last.message != m.RemoteID || last.cot != "cot_fixture" || len(f.g.writes) != 2 || f.g.writes[1].Parent != f.g.writes[0].ID {
		t.Fatal("completion or independent final answer lost its original message anchor")
	}
	creates, completes := 0, 0
	for _, call := range f.g.cotCalls {
		if call.step == "create" {
			creates++
		}
		if call.step == "complete" {
			completes++
		}
	}
	if creates != 1 || completes != 1 {
		t.Fatal("native lifecycle was created or completed more than once")
	}
	var completed bool
	if err := f.e.Store.DB.QueryRow(`SELECT completed FROM cot_deliveries WHERE mirror_key=?`, m.Key).Scan(&completed); err != nil || !completed {
		t.Fatal("COT lifecycle completion not persisted", err)
	}
	writes := f.g.writesCount()
	apply(t, f.e, 400, true, f.turn)
	f.drain(t)
	if f.g.writesCount() != writes || grants != releases || activeLease {
		t.Fatal("replay reopened COT or lease was released incorrectly")
	}
}

func TestCOTOutboundFailuresFreezeEveryStepAcrossRecoveryWithoutBlockingFinal(t *testing.T) {
	for _, step := range []string{"create", "append", "complete"} {
		for _, state := range []string{"unknown", "rejected"} {
			t.Run(step+"/"+state, func(t *testing.T) {
				f := prepareCOTOutboundStep(t, step, false)
				f.g.failStep = step
				f.g.failure = &feishu.Unknown{Operation: "synthetic " + step}
				if state == "rejected" {
					f.g.failure = &feishu.Rejected{Code: 99991672, HTTPStatus: 403}
				}
				before := f.g.writesCount()
				r, err := f.o.DispatchOne(context.Background(), "host", "thread", f.now)
				f.now += 3000
				if err != nil || r.State != state || r.AttemptID == "" || f.g.writesCount() != before+1 {
					t.Fatal("COT failure misclassified or retried", r, err)
				}
				if step != "complete" {
					f.terminal(t)
				}
				f.recover(t)
				f.g.failStep, f.g.failure = "", nil
				cotWrites := len(f.g.cotCalls)
				f.drain(t)
				a, err := f.e.Store.MirrorAttempt(context.Background(), r.AttemptID)
				if err != nil || a.State != state || len(f.g.cotCalls) != cotWrites || len(f.g.writes) != 2 || !strings.Contains(f.g.writes[1].Card, "synthetic final answer") {
					t.Fatal("recovery resent COT or blocked the independent final", a.State, err)
				}
			})
		}
	}
}

func TestCOTOutboundAcceptedRPCWithIgnoredFinishStaysFrozen(t *testing.T) {
	for _, step := range []string{"create", "append", "complete"} {
		t.Run(step, func(t *testing.T) {
			f := prepareCOTOutboundStep(t, step, false)
			if _, err := f.e.Store.DB.Exec(`CREATE TRIGGER ignore_cot_finish BEFORE UPDATE ON cot_deliveries BEGIN SELECT RAISE(IGNORE); END`); err != nil {
				t.Fatal(err)
			}
			before := f.g.writesCount()
			r, err := f.o.DispatchOne(context.Background(), "host", "thread", f.now)
			f.now += 3000
			if !errors.Is(err, store.ErrConflict) || r.State != "unknown" || f.g.writesCount() != before+1 {
				t.Fatal("accepted COT write lost its conservative failure state", r, err)
			}
			a, err := f.e.Store.MirrorAttempt(context.Background(), r.AttemptID)
			if err != nil || a.State != "dispatching" {
				t.Fatal("failed finish partially acknowledged the remote write", a.State, err)
			}
			if _, err := f.e.Store.DB.Exec(`DROP TRIGGER ignore_cot_finish`); err != nil {
				t.Fatal(err)
			}
			if step != "complete" {
				f.terminal(t)
			}
			f.recover(t)
			cotWrites := len(f.g.cotCalls)
			f.drain(t)
			a, err = f.e.Store.MirrorAttempt(context.Background(), r.AttemptID)
			if err != nil || a.State != "unknown" || len(f.g.cotCalls) != cotWrites || len(f.g.writes) != 2 {
				t.Fatal("recovery resent accepted COT or starved final answer", a.State, err)
			}
		})
	}
}

func TestCOTOutboundTopicRootProofRunsBeforeEveryMutation(t *testing.T) {
	for _, step := range []string{"create", "append", "complete"} {
		t.Run(step, func(t *testing.T) {
			f := prepareCOTOutboundStep(t, step, true)
			for _, call := range f.g.cotCalls {
				if call.step == "create" && (call.origin != "root" || !call.inThread || call.chat != "chat") {
					t.Fatal("COT create used a group target instead of the topic root")
				}
			}
			before, reads := f.g.writesCount(), f.g.rootReads
			attempts := mirrorAttemptCount(t, f.e.Store, mirrorKey("progress", "turn"))
			f.g.rootDeleted = true
			r, err := f.o.DispatchOne(context.Background(), "host", "thread", f.now)
			if !errors.Is(err, ErrTopicRootGone) || r.AttemptID != "" || f.g.writesCount() != before || f.g.rootReads != reads+1 || mirrorAttemptCount(t, f.e.Store, mirrorKey("progress", "turn")) != attempts {
				t.Fatal("native progress bypassed topic root proof", r, err)
			}
			b, err := f.e.Store.Binding(context.Background(), "host", "thread")
			if err != nil || !b.RepairRequired {
				t.Fatal("deleted COT topic root was not quarantined", err)
			}
		})
	}
}

func TestCOTOutboundLeaseDenialAndRevocationCannotWrite(t *testing.T) {
	for _, step := range []string{"create", "append", "complete"} {
		for _, revoke := range []bool{false, true} {
			name := step + "/denied"
			if revoke {
				name = step + "/revoked_after_claim"
			}
			t.Run(name, func(t *testing.T) {
				f := prepareCOTOutboundStep(t, step, false)
				denied := errors.New("synthetic lease denial")
				releases := 0
				f.o.BeginWrite = func(ctx context.Context, _ store.Binding) (context.Context, func(), error) {
					if revoke {
						return &cancelOnSecondErrContext{Context: ctx}, func() { releases++ }, nil
					}
					return ctx, func() { releases++ }, denied
				}
				before := f.g.writesCount()
				attempts := mirrorAttemptCount(t, f.e.Store, mirrorKey("progress", "turn"))
				r, err := f.o.DispatchOne(context.Background(), "host", "thread", f.now)
				wantErr := denied
				wantAttempts := attempts
				if revoke {
					wantErr, wantAttempts = context.Canceled, attempts+1
					if r.State != "unknown" || r.AttemptID == "" {
						t.Fatal("post-claim lease revocation did not remain conservative")
					}
				} else if r.AttemptID != "" {
					t.Fatal("lease denial consumed a COT attempt")
				}
				if !errors.Is(err, wantErr) || f.g.writesCount() != before || releases != 1 || mirrorAttemptCount(t, f.e.Store, mirrorKey("progress", "turn")) != wantAttempts {
					t.Fatal("native progress bypassed write lease", r, err)
				}
			})
		}
	}
}

func TestCOTOutboundLegacyProgressRemainsCardAndTerminalHistoryCreatesNoCOT(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "terminal_history"
		if legacy {
			name = "legacy_progress"
		}
		t.Run(name, func(t *testing.T) {
			e, _ := engineFixture(t)
			bindEngine(t, e)
			turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{input("user", "synthetic request"), agent("public", "synthetic progress", "commentary")}}
			g := &cotOutboundGateway{s: e.Store, t: t}
			f := &cotOutboundFixture{e: e, g: g, o: Outbound{Store: e.Store, Gateway: g}, now: 1000}
			legacyRemote := ""
			if legacy {
				apply(t, e, 200, false, turn)
				f.drain(t)
				m, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
				if err != nil {
					t.Fatal(err)
				}
				legacyRemote = m.RemoteID
			}
			e.NativeCOT, f.e.NativeCOT = true, true
			if legacy {
				turn.Items = append(turn.Items, agent("next-public", "synthetic next progress", "commentary"))
				apply(t, e, 250, false, turn)
				f.drain(t)
			}
			turn.Status = "completed"
			turn.Items = append(turn.Items, agent("final", "synthetic final answer", "final_answer"))
			apply(t, e, 300, true, turn)
			f.drain(t)
			mirrors, err := e.Store.Mirrors(context.Background(), "host", "thread")
			if err != nil {
				t.Fatal(err)
			}
			progress := 0
			for _, m := range mirrors {
				if m.Kind == "progress" {
					progress++
					if _, native, err := cot.Parse(m.Card); err != nil || native || m.RemoteID != legacyRemote {
						t.Fatal("legacy card was migrated to COT", err)
					}
				}
			}
			if len(g.cotCalls) != 0 || legacy && progress != 1 || !legacy && progress != 0 {
				t.Fatal("legacy or terminal history acquired a new native COT")
			}
		})
	}
}

func TestCOTOutboundMissingGatewayCapabilityCannotFallBackToCard(t *testing.T) {
	f := prepareCOTOutboundStep(t, "create", false)
	legacyGateway := &cardGateway{}
	f.o.Gateway = legacyGateway
	r, err := f.o.DispatchOne(context.Background(), "host", "thread", f.now)
	if err == nil || r.AttemptID != "" || len(legacyGateway.writes) != 0 || mirrorAttemptCount(t, f.e.Store, mirrorKey("progress", "turn")) != 0 {
		t.Fatal("missing COT capability dispatched as an ordinary card", r, err)
	}
	m, err := f.e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if err != nil || m.State != "ready" {
		t.Fatal("missing COT capability consumed the pending document", m.State, err)
	}
}

func TestCOTInterruptedEmptyBodyKeepsTerminalCOTWithoutFallback(t *testing.T) {
	f := newCOTOutboundFixture(t, false, "synthetic public progress")
	f.dispatch(t, "create")
	f.turn.Status = "interrupted"
	for i := range f.turn.Items {
		if f.turn.Items[i].ID == "tool" {
			f.turn.Items[i].Status = "interrupted"
		}
	}
	apply(t, f.e, 300, false, f.turn)

	progress, err := f.e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("progress", "turn"))
	if err != nil || progress.RemoteID == "" || !progress.Terminal {
		t.Fatalf("interrupted native progress was not retained: %+v %v", progress, err)
	}
	doc, native, err := cot.Parse(progress.Card)
	if err != nil || !native || doc.Status != "interrupted" {
		t.Fatalf("terminal COT update was not staged: native=%v status=%s err=%v", native, doc.Status, err)
	}
	mirrors, err := f.e.Store.Mirrors(context.Background(), "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	for _, mirror := range mirrors {
		if mirror.TurnID == "turn" && mirror.Kind == "final" && !mirror.Retired {
			t.Fatalf("confirmed native COT interrupted turn acquired fallback final: %+v", mirror)
		}
	}

	f.drain(t)
	completed := false
	for _, call := range f.g.cotCalls {
		if call.step == "complete" {
			completed = true
			break
		}
	}
	if !completed || len(f.g.writes) != 1 {
		t.Fatalf("terminal COT was not completed independently of final fallback: calls=%+v cards=%+v", f.g.cotCalls, f.g.writes)
	}
}

func TestCOTInterruptedNonEmptyFinalStillCreatesResult(t *testing.T) {
	f := newCOTOutboundFixture(t, false, "synthetic public progress")
	f.dispatch(t, "create")
	f.turn.Status = "interrupted"
	f.turn.Items = append(f.turn.Items, agent("final", "synthetic final answer", "final_answer"))
	apply(t, f.e, 300, false, f.turn)

	final, err := f.e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
	if err != nil || final.Retired || !strings.Contains(final.Card, "synthetic final answer") {
		t.Fatalf("non-empty interrupted final was suppressed: %+v %v", final, err)
	}
}

func TestCOTInterruptedEmptyBodyFallbackRequiresConfirmedNativeProgress(t *testing.T) {
	t.Run("unconfirmed_create", func(t *testing.T) {
		f := newCOTOutboundFixture(t, false, "synthetic public progress")
		f.g.failStep = "create"
		f.g.failure = &feishu.Unknown{Operation: "synthetic create"}
		r, err := f.o.DispatchOne(context.Background(), "host", "thread", f.now)
		f.now += 3000
		if err != nil || r.State != "unknown" || r.AttemptID == "" {
			t.Fatalf("COT create was not left unknown: %+v %v", r, err)
		}
		f.turn.Status = "interrupted"
		apply(t, f.e, 300, false, f.turn)
		final, err := f.e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
		if err != nil || final.Retired || !strings.Contains(final.Card, "本轮已停止") {
			t.Fatalf("unconfirmed COT suppressed the required fallback: %+v %v", final, err)
		}
	})

	t.Run("legacy_progress", func(t *testing.T) {
		e, _ := engineFixture(t)
		turn := codexclient.Turn{ID: "turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{input("user", "synthetic request"), agent("public", "synthetic progress", "commentary")}}
		apply(t, e, 200, false, turn)
		turn.Status = "interrupted"
		apply(t, e, 300, false, turn)
		final, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
		if err != nil || final.Retired || !strings.Contains(final.Card, "本轮已停止") {
			t.Fatalf("legacy progress suppressed the required fallback: %+v %v", final, err)
		}
	})

	t.Run("terminal_history", func(t *testing.T) {
		e, _ := engineFixture(t)
		e.NativeCOT = true
		turn := codexclient.Turn{ID: "turn", Status: "interrupted", Items: []codexclient.Item{input("user", "synthetic request")}}
		apply(t, e, 300, false, turn)
		final, err := e.Store.Mirror(context.Background(), "host", "thread", mirrorKey("final", "turn", "0"))
		if err != nil || final.Retired || !strings.Contains(final.Card, "本轮已停止") {
			t.Fatalf("terminal history suppressed the required fallback: %+v %v", final, err)
		}
		progress, err := e.Store.Mirrors(context.Background(), "host", "thread")
		if err != nil {
			t.Fatal(err)
		}
		for _, mirror := range progress {
			if mirror.Kind == "progress" {
				t.Fatalf("first terminal observation created native progress: %+v", mirror)
			}
		}
	})
}
