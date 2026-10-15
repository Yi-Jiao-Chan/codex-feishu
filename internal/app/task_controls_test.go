package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
	messagesync "codex-feishu/internal/sync"
)

// The fixture is an independent App actor. Local card claims never call it;
// only the serialized submission controller performs native writes.
type taskNative struct {
	turns          []codexclient.Turn
	queue          []codexclient.Queued
	queueErr       error
	interruptErr   error
	interruptApply bool
	reads, starts  int
	interrupts     []string
}

func (f *taskNative) Queue(_ context.Context, thread string) ([]codexclient.Queued, error) {
	f.reads++
	if thread != "thread" {
		return nil, errors.New("foreign task thread")
	}
	return append([]codexclient.Queued(nil), f.queue...), f.queueErr
}
func (f *taskNative) Turns(_ context.Context, thread string) ([]codexclient.Turn, error) {
	f.reads++
	if thread != "thread" {
		return nil, errors.New("foreign task thread")
	}
	turns := append([]codexclient.Turn(nil), f.turns...)
	for i := range turns {
		turns[i].Items = append([]codexclient.Item(nil), turns[i].Items...)
	}
	return turns, nil
}
func (*taskNative) Add(context.Context, string, string, string) (codexclient.Queued, error) {
	return codexclient.Queued{}, errors.New("unexpected task add")
}
func (*taskNative) Delete(context.Context, string, string) (bool, error) {
	return false, errors.New("unexpected task delete")
}
func (*taskNative) Steer(context.Context, string, string, string, string) error {
	return errors.New("unexpected task steer")
}
func (f *taskNative) Interrupt(_ context.Context, thread, target string) error {
	if thread != "thread" {
		return errors.New("foreign task thread")
	}
	f.interrupts = append(f.interrupts, target)
	for i := range f.turns {
		if f.turns[i].ID == target && f.turns[i].Status == "inProgress" {
			if f.interruptApply {
				f.turns[i].Status = "interrupted"
			}
			return f.interruptErr
		}
	}
	return &codexclient.RPCError{Code: -32600}
}
func (f *taskNative) StartQueue(_ context.Context, thread string) error {
	if thread != "thread" {
		return errors.New("foreign task thread")
	}
	f.starts++
	for _, turn := range f.turns {
		if turn.Status == "inProgress" {
			return &codexclient.RPCError{Code: -32600}
		}
	}
	if len(f.queue) > 0 {
		q := f.queue[0]
		f.queue = f.queue[1:]
		f.turns = append(f.turns, codexclient.Turn{ID: "next-turn", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{{ID: "next-user", Type: "userMessage", ClientID: q.ClientUserMessageID}}})
	}
	return nil
}

type taskFixture struct {
	s        *store.Store
	path     string
	controls *TaskControls
	native   *taskNative
	engine   messagesync.Engine
}

func newTaskFixture(t *testing.T, bound bool, status string) *taskFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "task.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &taskFixture{s: s, path: path}
	t.Cleanup(func() { f.s.Close() })
	if _, err = s.BeginBaseline(ctx, "host", "app", 100); err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveNew(ctx, "host", "thread", "/project", 101); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkEligible(ctx, "host", "thread", "original-user", "/project", "/project"); err != nil {
		t.Fatal(err)
	}
	body := json.RawMessage(`{"type":"text","text":"fixture request"}`)
	u := codexclient.Item{Type: "userMessage", ID: "original-user", Content: []json.RawMessage{body}}
	if bound {
		i, err := s.PrepareGroup(ctx, "host", "thread", "owner", "task", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ClaimGroupDispatch(ctx, i.ID); err != nil {
			t.Fatal(err)
		}
		if err = s.ConfirmGroup(ctx, i.ID, "chat"); err != nil {
			t.Fatal(err)
		}
		if err = s.BindConfirmedGroup(ctx, i.ID); err != nil {
			t.Fatal(err)
		}
		r, _, err := s.Claim(ctx, store.Receipt{AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: "actual-initiator", Body: "fixture request"})
		if err != nil {
			t.Fatal(err)
		}
		r.State = "consumed"
		if err = s.SaveReceipt(ctx, r); err != nil {
			t.Fatal(err)
		}
		u.ClientID = &r.ClientID
	}
	phase := "commentary"
	f.native = &taskNative{interruptApply: true, turns: []codexclient.Turn{{ID: "original-turn", Status: status, ItemsView: "full", Items: []codexclient.Item{u, {ID: "public", Type: "agentMessage", Text: "working", Phase: &phase}}}}}
	f.controls = &TaskControls{Store: s, HostID: "host", AppID: "app", Queue: f.native.Queue}
	f.engine = messagesync.Engine{Store: s, HostID: "host", ProgressRender: f.controls.Render}
	return f
}

func (f *taskFixture) prepare(t *testing.T, at int64) store.Mirror {
	t.Helper()
	ctx := context.Background()
	if err := f.controls.Prepare(ctx, "thread", f.native.turns); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Apply(ctx, "thread", f.native.turns, false, at); err != nil {
		t.Fatal(err)
	}
	list, err := f.s.Mirrors(ctx, "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.Kind == "progress" && m.TurnID == "original-turn" {
			return m
		}
	}
	t.Fatal("original task progress not staged")
	return store.Mirror{}
}

func (f *taskFixture) deliver(t *testing.T, now int64) store.Mirror {
	t.Helper()
	ctx := context.Background()
	for n := 0; n < 20; n++ {
		ready, err := f.s.ReadyMirrors(ctx, "host", "thread")
		if err != nil {
			t.Fatal(err)
		}
		claimedAny := false
		for _, m := range ready {
			a, claimed, err := f.s.ClaimMirror(ctx, "host", "thread", m.Key, m.Revision, now)
			if err != nil {
				t.Fatal(err)
			}
			if !claimed {
				continue
			}
			claimedAny = true
			remote := m.RemoteID
			if remote == "" {
				remote = "actual-" + m.Kind + "-" + m.TurnID
			}
			if err = f.s.FinishMirrorAttempt(ctx, a.ID, "confirmed", remote); err != nil {
				t.Fatal(err)
			}
		}
		if !claimedAny {
			break
		}
	}
	list, err := f.s.Mirrors(ctx, "host", "thread")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.Kind == "progress" && m.TurnID == "original-turn" {
			if m.RemoteID == "" || m.DeliveredRevision != m.Revision {
				t.Fatal("original process card was not delivered")
			}
			return m
		}
	}
	t.Fatal("process card missing")
	return store.Mirror{}
}

func (f *taskFixture) view(t *testing.T) store.TaskControl {
	t.Helper()
	list, err := f.s.TaskControls(context.Background(), store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range list {
		if v.TurnID == "original-turn" {
			return v
		}
	}
	t.Fatal("original task view missing")
	return store.TaskControl{}
}

func (f *taskFixture) tick(t *testing.T) {
	t.Helper()
	c := &submissions.Controller{Store: f.s, Native: f.native, HostID: "host", AppID: "app", ThreadID: "thread"}
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTaskStopCallbackIsLocalAndNeverRetargetsAppReplacement(t *testing.T) {
	f := newTaskFixture(t, true, "inProgress")
	f.native.queue = []codexclient.Queued{{ID: "app-pending"}}
	f.prepare(t, 200)
	shown := f.deliver(t, 1000)
	v := f.view(t)
	reads := f.native.reads
	callback := &TaskControls{Store: f.s, HostID: "host", AppID: "app"}
	thread, err := callback.Claim(context.Background(), "chat", shown.RemoteID, "stop", v.Token, v.Revision)
	if err != nil || thread != "thread" || f.native.reads != reads || len(f.native.interrupts) != 0 {
		t.Fatal("card callback performed native I/O", err)
	}
	f.native.turns[0].Status = "completed"
	f.native.turns = append(f.native.turns, codexclient.Turn{ID: "replacement", Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{{ID: "replacement-user", Type: "userMessage"}}})
	f.tick(t)
	f.prepare(t, 201)
	claims, err := f.s.TaskControlClaims(context.Background(), store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if err != nil || len(claims) != 1 || claims[0].State != "rejected" || claims[0].TurnID != "original-turn" || claims[0].Result != "ended" || len(f.native.interrupts) != 0 || f.native.turns[1].Status != "inProgress" || len(f.native.queue) != 1 {
		t.Fatal("stale task card interrupted the replacement turn or changed queue", err)
	}
	if _, err = callback.Claim(context.Background(), "chat", shown.RemoteID, "stop", v.Token, v.Revision); err != nil {
		t.Fatal("original terminal callback result could not be replayed", err)
	}
	f.tick(t)
	if len(f.native.interrupts) != 0 {
		t.Fatal("terminal task replay retried native stop")
	}
}

func TestTaskStoppedWithPendingAppQueueReusesProgressForContinue(t *testing.T) {
	f := newTaskFixture(t, true, "inProgress")
	f.native.queue = []codexclient.Queued{{ID: "app-pending"}, {ID: "app-pending-2"}}
	f.prepare(t, 200)
	shown := f.deliver(t, 1000)
	v := f.view(t)
	ctx := context.Background()
	callback := &TaskControls{Store: f.s, HostID: "host", AppID: "app"}
	if _, err := callback.Claim(ctx, "chat", shown.RemoteID, "stop", v.Token, v.Revision); err != nil {
		t.Fatal(err)
	}
	pending := f.prepare(t, 201)
	if !strings.Contains(pending.Card, "正在停止") || strings.Contains(pending.Card, `"tag":"button"`) {
		t.Fatal("accepted local stop claim presented a confirmed result or another button")
	}
	f.tick(t)
	stopped := f.prepare(t, 202)
	continued := f.view(t)
	if stopped.Key != shown.Key || stopped.ParentMessageID != "actual-initiator" || continued.Kind != "continue" || continued.QueueCount != 2 || continued.State != "available" || !strings.Contains(stopped.Card, "2 条消息待执行") || len(f.native.interrupts) != 1 || f.native.interrupts[0] != "original-turn" || len(f.native.queue) != 2 {
		t.Fatal("stop failed to preserve App queue or the original progress card")
	}
	f.deliver(t, 5000)
	if _, err := callback.Claim(ctx, "chat", shown.RemoteID, "continue", continued.Token, continued.Revision); err != nil {
		t.Fatal(err)
	}
	if f.native.starts != 0 {
		t.Fatal("Continue callback started queue inside receiver")
	}
	f.tick(t)
	if f.native.starts != 1 || len(f.native.queue) != 1 || f.native.turns[1].Status != "inProgress" {
		t.Fatal("explicit Continue did not start the actual pending App queue")
	}
	f.prepare(t, 203)
	if _, err := callback.Claim(ctx, "chat", shown.RemoteID, "continue", continued.Token, continued.Revision); err != nil {
		t.Fatal("Continue replay was not durable", err)
	}
	f.tick(t)
	if f.native.starts != 1 {
		t.Fatal("Continue replay started native queue again")
	}
}

func TestTaskControlClaimsRecoverLocalDispatchGapAndKeepUnknownHonest(t *testing.T) {
	f := newTaskFixture(t, true, "inProgress")
	f.native.queue = []codexclient.Queued{{ID: "app-pending"}}
	f.prepare(t, 200)
	shown := f.deliver(t, 1000)
	v := f.view(t)
	ctx := context.Background()
	// Crash after the card claim commits, before its native action is linked.
	claim, err := f.s.ClaimTaskControl(ctx, "host", "app", "chat", shown.RemoteID, "stop", v.Token, v.Revision)
	if err != nil {
		t.Fatal(err)
	}
	f.s.Close()
	f.s, err = store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.controls.Store, f.engine.Store = f.s, f.s
	f.prepare(t, 201)
	claims, err := f.s.TaskControlClaims(ctx, store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if err != nil || len(claims) != 1 || claims[0].ExternalID != claim.ExternalID || claims[0].NativeActionID == "" || claims[0].State != "pending" {
		t.Fatal("local crash gap did not recover the stable original action", err)
	}
	f.native.interruptApply, f.native.interruptErr = false, errors.New("fixture native lost reply with private payload")
	c := &submissions.Controller{Store: f.s, Native: f.native, HostID: "host", AppID: "app", ThreadID: "thread"}
	if err = c.Tick(ctx); err == nil || strings.Contains(err.Error(), "private payload") {
		t.Fatal("lost native reply did not return its safe verification error", err)
	}
	unknown := f.prepare(t, 202)
	if !strings.Contains(unknown.Card, "正在核验") || strings.Contains(unknown.Card, `"tag":"button"`) || strings.Contains(unknown.Card, "private payload") || f.view(t).State != "unknown" {
		t.Fatal("ambiguous native stop presented false success or a retry button")
	}
	f.tick(t)
	if len(f.native.interrupts) != 1 || len(f.native.queue) != 1 {
		t.Fatal("unknown native stop was retried or changed queue")
	}
}

func TestTaskContinueWithoutProcessPreparesUnboundAndReadsActualQueue(t *testing.T) {
	f := newTaskFixture(t, false, "interrupted")
	f.native.queue = []codexclient.Queued{{ID: "app-native-entry"}}
	m := f.prepare(t, 200)
	if m.ParentKey == "" || m.ParentMessageID != "" || m.TurnID != "original-turn" || !strings.Contains(m.Card, `"action":"continue"`) || f.view(t).QueueCount != 1 {
		t.Fatal("eligible unbound stopped turn did not stage its original-parent control")
	}
	reads, card := f.native.reads, f.controls.Render("thread", "original-turn", "interrupted", cards.ProgressView{Body: "working", ElapsedSeconds: -1})
	for i := 0; i < 5; i++ {
		if got := f.controls.Render("thread", "original-turn", "interrupted", cards.ProgressView{Body: "working", ElapsedSeconds: -1}); got != card {
			t.Fatal("pure render changed without preparation")
		}
	}
	if f.native.reads != reads {
		t.Fatal("render performed native reads")
	}
	v := f.view(t)
	if _, err := f.controls.Claim(context.Background(), "chat", "fabricated-card", "continue", v.Token, v.Revision); !errors.Is(err, store.ErrSubmissionStale) || f.native.starts != 0 {
		t.Fatal("unbound stopped task accepted callback", err)
	}
	for _, queue := range [][]codexclient.Queued{nil, {{ID: "duplicate"}, {ID: "duplicate"}}, {{}}} {
		f.native.queue = queue
		f.prepare(t, 201)
		if strings.Contains(f.controls.Render("thread", "original-turn", "interrupted", cards.ProgressView{ElapsedSeconds: -1}), `"tag":"button"`) {
			t.Fatal("empty or incomplete App queue exposed Continue")
		}
	}
	f.native.queue = []codexclient.Queued{{ID: "known"}}
	f.native.queueErr = errors.New("private queue reply")
	failed := f.prepare(t, 202)
	if f.view(t).QueueCount != -1 || !strings.Contains(failed.Card, "队列状态待核验") || strings.Contains(failed.Card, `"tag":"button"`) || strings.Contains(failed.Card, "private queue reply") {
		t.Fatal("failed queue read invented an App queue count or action")
	}
}

func TestTaskWaitingOnlyReturnsToAppAndConcurrentCallbackReusesAction(t *testing.T) {
	f := newTaskFixture(t, true, "inProgress")
	f.controls.Waiting = func(thread, turn string) bool { return thread == "thread" && turn == "original-turn" }
	m := f.prepare(t, 200)
	if !strings.Contains(m.Card, "请回 Codex App 处理审批或问题") || strings.Contains(m.Card, "approve") || strings.Contains(m.Card, "answer") {
		t.Fatal("task waiting card did not return to App without answering")
	}
	shown := f.deliver(t, 1000)
	v := f.view(t)
	reads := f.native.reads
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			callback := &TaskControls{Store: f.s, HostID: "host", AppID: "app"}
			_, err := callback.Claim(context.Background(), "chat", shown.RemoteID, "stop", v.Token, v.Revision)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("concurrent callback lost its original claim", err)
		}
	}
	if f.native.reads != reads || len(f.native.interrupts) != 0 {
		t.Fatal("local callback invoked native requests")
	}
	actions, err := f.s.SubmissionActions(context.Background(), store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"})
	if err != nil || len(actions) != 1 || actions[0].ExpectedTurnID != "original-turn" {
		t.Fatal("concurrent task callbacks created different native actions", err)
	}
	f.tick(t)
	if len(f.native.interrupts) != 1 {
		t.Fatal("concurrent task replay dispatched native stop more than once")
	}
}

func TestTaskTypedProgressPreservesRecordsWhileMismatchedStatusSuppressesControls(t *testing.T) {
	f := newTaskFixture(t, true, "inProgress")
	f.prepare(t, 200)
	reads := f.native.reads
	progress := cards.ProgressView{Records: []cards.ProgressRecord{{Text: "first public action"}, {Text: "second public action"}}, ElapsedSeconds: 3}
	card := f.controls.Render("thread", "original-turn", "completed", progress)
	if !strings.Contains(card, "正在核验任务状态") || strings.Contains(card, `"tag":"button"`) || !strings.Contains(card, "first public action") || !strings.Contains(card, "second public action") || f.native.reads != reads {
		t.Fatal("typed process history bypassed stale-state controls or performed RPC")
	}
}

func TestTaskInvalidHistoryReturnsSafeErrorWithoutQueueRead(t *testing.T) {
	f := newTaskFixture(t, true, "interrupted")
	for i, turns := range [][]codexclient.Turn{
		{{ID: "turn", Status: "private payload"}},
		{{ID: "turn", Status: "interrupted", ItemsView: "summary"}},
		{{ID: "turn", Status: "interrupted"}, {ID: "turn", Status: "interrupted"}},
		{{ID: "turn", Status: "inProgress"}, {ID: "later", Status: "inProgress"}},
	} {
		before := f.native.reads
		err := f.controls.Prepare(context.Background(), "thread", turns)
		if err == nil || strings.Contains(err.Error(), "private payload") || f.native.reads != before {
			t.Fatal(fmt.Sprintf("invalid full history %d accepted or exposed", i), err)
		}
	}
}
