package submissions

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

// fakeNative is an independent App actor as well as the RPC fixture: its hooks
// may consume queue entries, finish/start a turn, or lose a reply during an RPC.
// It deliberately does not deduplicate clientUserMessageId.
type fakeNative struct {
	startReplyErr                                             error
	mu                                                        sync.Mutex
	queue                                                     []codexclient.Queued
	turns                                                     []codexclient.Turn
	paused                                                    bool
	addCalls, deleteCalls, steerCalls, interruptCalls, starts int
	seq                                                       int
	addReplyErr, steerReplyErr, deleteReplyErr, stopReplyErr  error
	addReject, deleteBeforeErr                                error
	beforeDelete, afterDelete, beforeSteer, beforeStart       func(*fakeNative, string)
	expected                                                  []string
	clients                                                   []string
}

func (f *fakeNative) active() string {
	for _, t := range f.turns {
		if t.Status == "inProgress" {
			return t.ID
		}
	}
	return ""
}

func (f *fakeNative) end() {
	for i := range f.turns {
		if f.turns[i].Status == "inProgress" {
			f.turns[i].Status = "completed"
		}
	}
}

func (f *fakeNative) start(client string) string {
	f.seq++
	id := fmt.Sprintf("t%d", f.seq)
	item := codexclient.Item{ID: fmt.Sprintf("i%d", f.seq), Type: "userMessage"}
	if client != "" {
		item.ClientID = &client
	}
	f.turns = append(f.turns, codexclient.Turn{ID: id, Status: "inProgress", ItemsView: "full", Items: []codexclient.Item{item}})
	return id
}

func (f *fakeNative) consume(id string) {
	for i, q := range f.queue {
		if q.ID == id {
			f.queue = append(f.queue[:i], f.queue[i+1:]...)
			f.end()
			client := ""
			if q.ClientUserMessageID != nil {
				client = *q.ClientUserMessageID
			}
			f.start(client)
			return
		}
	}
}

func checkThread(id string) error {
	if id != "thread" {
		return errors.New("fixture received foreign thread")
	}
	return nil
}

func (f *fakeNative) Queue(_ context.Context, id string) ([]codexclient.Queued, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := checkThread(id); err != nil {
		return nil, err
	}
	return append([]codexclient.Queued(nil), f.queue...), nil
}

func (f *fakeNative) Turns(_ context.Context, id string) ([]codexclient.Turn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := checkThread(id); err != nil {
		return nil, err
	}
	result := append([]codexclient.Turn(nil), f.turns...)
	for i := range result {
		result[i].Items = append([]codexclient.Item(nil), result[i].Items...)
	}
	return result, nil
}

func (f *fakeNative) Add(_ context.Context, id, _ string, client string) (codexclient.Queued, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls++
	f.clients = append(f.clients, client)
	if err := checkThread(id); err != nil {
		return codexclient.Queued{}, err
	}
	if f.addReject != nil {
		return codexclient.Queued{}, f.addReject
	}
	q := codexclient.Queued{ID: fmt.Sprintf("q%d", f.addCalls), ClientUserMessageID: &client}
	if f.active() == "" && !f.paused {
		f.start(client)
	} else {
		f.queue = append(f.queue, q)
	}
	if f.addReplyErr != nil {
		err := f.addReplyErr
		f.addReplyErr = nil
		return codexclient.Queued{}, err
	}
	return q, nil
}

func (f *fakeNative) Delete(_ context.Context, id, qid string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	if err := checkThread(id); err != nil {
		return false, err
	}
	if f.beforeDelete != nil {
		f.beforeDelete(f, qid)
	}
	if f.deleteBeforeErr != nil {
		return false, f.deleteBeforeErr
	}
	for i, q := range f.queue {
		if q.ID == qid {
			f.queue = append(f.queue[:i], f.queue[i+1:]...)
			if f.afterDelete != nil {
				f.afterDelete(f, qid)
			}
			if f.deleteReplyErr != nil {
				return false, f.deleteReplyErr
			}
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeNative) Steer(_ context.Context, id, expected, _ string, client string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steerCalls++
	f.expected = append(f.expected, expected)
	f.clients = append(f.clients, client)
	if err := checkThread(id); err != nil {
		return err
	}
	if f.beforeSteer != nil {
		f.beforeSteer(f, client)
	}
	if f.active() != expected || expected == "" {
		return &codexclient.RPCError{Code: -32600, Message: "turn changed"}
	}
	for i := range f.turns {
		if f.turns[i].ID == expected {
			f.turns[i].Items = append(f.turns[i].Items, codexclient.Item{ID: "steered-" + client, Type: "userMessage", ClientID: &client})
		}
	}
	return f.steerReplyErr
}

func (f *fakeNative) Interrupt(_ context.Context, id, turn string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interruptCalls++
	f.expected = append(f.expected, turn)
	if err := checkThread(id); err != nil {
		return err
	}
	for i := range f.turns {
		if f.turns[i].ID == turn && f.turns[i].Status == "inProgress" {
			f.turns[i].Status = "interrupted"
			f.paused = true
			return f.stopReplyErr
		}
	}
	return &codexclient.RPCError{Code: -32600}
}

func (f *fakeNative) StartQueue(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if err := checkThread(id); err != nil {
		return err
	}
	if f.beforeStart != nil {
		f.beforeStart(f, "")
	}
	if f.active() != "" {
		return &codexclient.RPCError{Code: -32600, Message: "busy"}
	}
	f.paused = false
	if len(f.queue) != 0 {
		f.consume(f.queue[0].ID)
	}
	return f.startReplyErr
}

func fixture(t *testing.T, busy bool) (*Controller, *fakeNative, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inputs.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// Idempotent while the composition root is wiring this exported migration.
	if _, err = s.DB.Exec(store.SubmissionSchema); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = s.BeginBaseline(ctx, "host", "app", 100); err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveNew(ctx, "host", "thread", "/project", 101); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkEligible(ctx, "host", "thread", "seed", "/project", "/project"); err != nil {
		t.Fatal(err)
	}
	g, err := s.PrepareGroup(ctx, "host", "thread", "owner", "group", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimGroupDispatch(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmGroup(ctx, g.ID, "chat"); err != nil {
		t.Fatal(err)
	}
	if err = s.BindConfirmedGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	f := &fakeNative{}
	if busy {
		f.start("")
	}
	return &Controller{Native: f, Store: s, HostID: "host", AppID: "app", ThreadID: "thread"}, f, path
}

func claim(t *testing.T, c *Controller, message string) store.Receipt {
	t.Helper()
	r, fresh, err := c.Store.Claim(context.Background(), store.Receipt{AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: message, Body: "fixture " + message})
	if err != nil || !fresh {
		t.Fatalf("claim: fresh=%v err=%v", fresh, err)
	}
	return r
}

func view(t *testing.T, c *Controller, id string) store.Submission {
	t.Helper()
	list, err := c.Store.Submissions(context.Background(), c.scope())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range list {
		if n.ID == id {
			return n
		}
	}
	t.Fatal("receipt view missing")
	return store.Submission{}
}

func tick(t *testing.T, c *Controller) {
	t.Helper()
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func action(t *testing.T, c *Controller, id, kind string) store.SubmissionAction {
	t.Helper()
	n := view(t, c, id)
	a, err := c.ClaimAction(context.Background(), id, kind, n.QueueID, n.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func reopen(t *testing.T, c *Controller, path string) *Controller {
	t.Helper()
	if err := c.Store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &Controller{Native: c.Native, Store: s, HostID: c.HostID, AppID: c.AppID, ThreadID: c.ThreadID}
}

func TestQ1OrdinaryInputAlwaysUsesNativeAdd(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprint(busy), func(t *testing.T) {
			c, f, _ := fixture(t, busy)
			r := claim(t, c, "input")
			tick(t, c)
			n := view(t, c, r.ID)
			want := "consumed"
			if busy {
				want = "queued"
			}
			if f.addCalls != 1 || f.starts != 0 || f.steerCalls != 0 || n.State != want || busy && n.QueueOrdinal != 1 {
				t.Fatalf("native calls/state: add=%d start=%d steer=%d state=%s ordinal=%d", f.addCalls, f.starts, f.steerCalls, n.State, n.QueueOrdinal)
			}
		})
	}
}

func TestQ2RealOrdinalIncludesAppEntriesAndTracksConsumption(t *testing.T) {
	c, f, _ := fixture(t, true)
	f.queue = append(f.queue, codexclient.Queued{ID: "app-queue"})
	a, b, d := claim(t, c, "A"), claim(t, c, "B"), claim(t, c, "C")
	tick(t, c)
	for i, r := range []store.Receipt{a, b, d} {
		if n := view(t, c, r.ID); n.QueueOrdinal != i+2 {
			t.Fatalf("ordinal %d", n.QueueOrdinal)
		}
	}
	f.consume("app-queue")
	f.consume(view(t, c, a.ID).QueueID)
	tick(t, c)
	if view(t, c, a.ID).State != "consumed" || view(t, c, b.ID).QueueOrdinal != 1 || view(t, c, d.ID).QueueOrdinal != 2 || f.addCalls != 3 {
		t.Fatal("native order or consumption changed")
	}
}

func TestQ3SteerDeletesOnlyExactItemAndPreservesOtherOrder(t *testing.T) {
	c, f, _ := fixture(t, true)
	a, b, d := claim(t, c, "A"), claim(t, c, "B"), claim(t, c, "C")
	tick(t, c)
	old := view(t, c, b.ID)
	action(t, c, b.ID, "steer")
	tick(t, c)
	n := view(t, c, b.ID)
	if n.State != "steered" || n.TurnID != "t1" || n.ItemID == "" || f.deleteCalls != 1 || f.steerCalls != 1 || len(f.queue) != 2 {
		t.Fatalf("steer result: state=%s turn=%s item=%s delete=%d steer=%d queue=%d", n.State, n.TurnID, n.ItemID, f.deleteCalls, f.steerCalls, len(f.queue))
	}
	if f.queue[0].ID != view(t, c, a.ID).QueueID || f.queue[1].ID != view(t, c, d.ID).QueueID || n.QueueID != old.QueueID || f.clients[len(f.clients)-1] != b.ClientID {
		t.Fatal("input or queue identities changed")
	}
	links, err := c.Store.InputLinks(context.Background(), "host", "thread")
	if err != nil || len(links) != 1 || links[0].ReceiptID != b.ID {
		t.Fatal("original receipt history link missing", err)
	}
}

func TestQ4AppConsumptionBeforeDeleteNeverSteers(t *testing.T) {
	c, f, _ := fixture(t, true)
	r := claim(t, c, "race")
	tick(t, c)
	f.beforeDelete = func(f *fakeNative, qid string) { f.consume(qid) }
	action(t, c, r.ID, "steer")
	tick(t, c)
	if f.steerCalls != 0 || f.addCalls != 1 || view(t, c, r.ID).State != "consumed" {
		t.Fatal("deleted=false reinjected consumed input")
	}
}

func TestQ5TurnChangesAfterDeleteRestoresWithoutRetargeting(t *testing.T) {
	for _, startNew := range []bool{false, true} {
		t.Run(fmt.Sprint(startNew), func(t *testing.T) {
			c, f, _ := fixture(t, true)
			r := claim(t, c, "restore")
			tick(t, c)
			oldQ := view(t, c, r.ID).QueueID
			f.afterDelete = func(f *fakeNative, _ string) {
				f.end()
				if startNew {
					f.start("")
				}
			}
			op := action(t, c, r.ID, "steer")
			tick(t, c)
			n := view(t, c, r.ID)
			got, err := c.Store.SubmissionAction(context.Background(), c.scope(), op.ID)
			if err != nil || got.Result != "restored" || got.ExpectedTurnID != "t1" || f.steerCalls != 1 || f.expected[0] != "t1" || f.addCalls != 2 || n.QueueID == oldQ {
				t.Fatalf("restoration result: %s state=%s adds=%d steer=%d err=%v", got.Result, n.State, f.addCalls, f.steerCalls, err)
			}
			if f.clients[0] != f.clients[len(f.clients)-1] || f.starts != 0 {
				t.Fatal("restore changed client ID or explicitly started a turn")
			}
		})
	}
}

func TestQ6LostSteerReplyConfirmsOnlyOriginalTargetTurn(t *testing.T) {
	c, f, path := fixture(t, true)
	r := claim(t, c, "lost-steer")
	tick(t, c)
	f.steerReplyErr = errors.New("sensitive fixture native payload")
	op := action(t, c, r.ID, "steer")
	if err := c.Tick(context.Background()); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("lost reply not classified safely")
	}
	c = reopen(t, c, path)
	tick(t, c)
	got, _ := c.Store.SubmissionAction(context.Background(), c.scope(), op.ID)
	if got.State != "confirmed" || got.Result != "steered" || view(t, c, r.ID).State != "steered" || f.steerCalls != 1 || f.addCalls != 1 {
		t.Fatal("lost steer reply redispatched or not reconciled")
	}
}

func TestQ6SameClientInAnotherTurnDoesNotConfirmUnknownSteer(t *testing.T) {
	c, f, _ := fixture(t, true)
	r := claim(t, c, "wrong-turn")
	tick(t, c)
	f.steerReplyErr = errors.New("lost")
	action(t, c, r.ID, "steer")
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("expected unknown")
	}
	item := f.turns[0].Items[len(f.turns[0].Items)-1]
	f.turns[0].Items = f.turns[0].Items[:len(f.turns[0].Items)-1]
	f.end()
	f.start("")
	f.turns[1].Items = append(f.turns[1].Items, item)
	for i := 0; i < 3; i++ {
		tick(t, c)
	}
	if view(t, c, r.ID).State != "unknown" || f.steerCalls != 1 || f.addCalls != 1 {
		t.Fatal("unrelated turn falsely confirmed steer")
	}
}

func TestQ7LostAddReplyReconcilesQueueAndFullHistoryAfterReopen(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprint(busy), func(t *testing.T) {
			c, f, path := fixture(t, busy)
			r := claim(t, c, "lost-add")
			f.addReplyErr = errors.New("lost")
			if err := c.Tick(context.Background()); err == nil {
				t.Fatal("expected unknown add")
			}
			if !busy {
				f.end()
				for i := 0; i < 180; i++ {
					f.start("")
					f.end()
				}
			}
			c = reopen(t, c, path)
			tick(t, c)
			n := view(t, c, r.ID)
			if n.State == "unknown" || n.ClientID != r.ClientID || f.addCalls != 1 {
				t.Fatal("unknown add was duplicated or not correlated")
			}
		})
	}
}

func TestQ8LostDeleteReplyFreezesWithPresentOrAbsentQueue(t *testing.T) {
	for _, performed := range []bool{false, true} {
		t.Run(fmt.Sprint(performed), func(t *testing.T) {
			c, f, path := fixture(t, true)
			r := claim(t, c, "lost-delete")
			tick(t, c)
			if performed {
				f.deleteReplyErr = errors.New("lost")
			} else {
				f.deleteBeforeErr = errors.New("lost")
			}
			action(t, c, r.ID, "steer")
			if err := c.Tick(context.Background()); err == nil {
				t.Fatal("expected unknown delete")
			}
			c = reopen(t, c, path)
			for i := 0; i < 3; i++ {
				tick(t, c)
			}
			if view(t, c, r.ID).State != "unknown" || f.steerCalls != 0 || f.deleteCalls != 1 || f.addCalls != 1 {
				t.Fatal("unknown delete was retried, steered or declared removed")
			}
		})
	}
}

func TestQ9ReplayAndConcurrentConflictingCallbacksHaveOneWinner(t *testing.T) {
	c, f, _ := fixture(t, true)
	r := claim(t, c, "replay")
	tick(t, c)
	n := view(t, c, r.ID)
	callbackController := &Controller{Store: c.Store, HostID: "host", AppID: "app", ThreadID: "thread"}
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := callbackController.ClaimAction(context.Background(), r.ID, "steer", n.QueueID, n.Revision)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- a.ID
		}()
	}
	wg.Wait()
	close(ids)
	winner := ""
	for id := range ids {
		if winner != "" && id != winner {
			t.Fatal("replay created a second action")
		}
		winner = id
	}
	if _, err := callbackController.ClaimAction(context.Background(), r.ID, "remove", n.QueueID, n.Revision); !errors.Is(err, store.ErrSubmissionStale) {
		t.Fatalf("old conflicting action: %v", err)
	}
	tick(t, c)
	a, err := callbackController.ClaimAction(context.Background(), r.ID, "steer", n.QueueID, n.Revision)
	if err != nil || a.ID != winner || a.Result != "steered" || f.deleteCalls != 1 || f.steerCalls != 1 {
		t.Fatal("completed callback replay changed result", err)
	}
}

func TestQ10StopAddAndExplicitContinuePreserveQueue(t *testing.T) {
	c, f, _ := fixture(t, true)
	a := claim(t, c, "A")
	tick(t, c)
	for i := 0; i < 10; i++ {
		if _, err := c.Stop(context.Background(), "stop-event"); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, c)
	b := claim(t, c, "B")
	tick(t, c)
	if f.interruptCalls != 1 || f.starts != 0 || f.active() != "" || len(f.queue) != 2 || view(t, c, b.ID).State != "queued" {
		t.Fatal("stop or ordinary input implicitly resumed queue")
	}
	if _, err := c.Continue(context.Background(), "continue-event"); err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	if f.starts != 1 || f.steerCalls != 0 || view(t, c, a.ID).State != "consumed" || len(f.queue) != 1 || f.queue[0].ClientUserMessageID == nil || *f.queue[0].ClientUserMessageID != b.ClientID {
		t.Fatal("explicit continue lost queue order or misused steer")
	}
}

func TestQ10AppStartRaceReturnsBusyWithoutSteer(t *testing.T) {
	c, f, _ := fixture(t, false)
	f.paused = true
	r := claim(t, c, "queued")
	tick(t, c)
	f.beforeStart = func(f *fakeNative, _ string) { f.consume(f.queue[0].ID) }
	op, err := c.Continue(context.Background(), "race-start")
	if err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	got, _ := c.Store.SubmissionAction(context.Background(), c.scope(), op.ID)
	if f.starts != 1 || f.steerCalls != 0 || got.State != "rejected" || view(t, c, r.ID).State != "consumed" {
		t.Fatal("busy start was converted into steer or lost actual consumption")
	}
}

func TestJournalFailureBeforeRPCAndSuccessBeforeLocalSave(t *testing.T) {
	for _, afterRPC := range []bool{false, true} {
		t.Run(fmt.Sprint(afterRPC), func(t *testing.T) {
			c, f, path := fixture(t, true)
			r := claim(t, c, "disk-fault")
			sql := `CREATE TRIGGER dispatch_fault BEFORE INSERT ON submission_attempts BEGIN SELECT RAISE(ABORT,'journal unavailable'); END`
			if afterRPC {
				sql = `CREATE TRIGGER dispatch_fault BEFORE UPDATE ON submission_attempts WHEN NEW.state='confirmed' BEGIN SELECT RAISE(ABORT,'journal unavailable'); END`
			}
			if _, err := c.Store.DB.Exec(sql); err != nil {
				t.Fatal(err)
			}
			if err := c.Tick(context.Background()); err == nil {
				t.Fatal("expected journal failure")
			}
			want := 0
			if afterRPC {
				want = 1
			}
			if f.addCalls != want {
				t.Fatal("RPC escaped its journal")
			}
			c = reopen(t, c, path)
			if _, err := c.Store.DB.Exec(`DROP TRIGGER dispatch_fault`); err != nil {
				t.Fatal(err)
			}
			tick(t, c)
			if f.addCalls != 1 || view(t, c, r.ID).State != "queued" {
				t.Fatal("safe prepared input lost or lost-success input duplicated")
			}
		})
	}
}

func TestRemovePendingOfflineAndRemoveQueued(t *testing.T) {
	for _, submitted := range []bool{false, true} {
		t.Run(fmt.Sprint(submitted), func(t *testing.T) {
			c, f, _ := fixture(t, true)
			r := claim(t, c, "remove")
			if submitted {
				tick(t, c)
			} else if err := c.Store.StageSubmissions(context.Background(), c.scope()); err != nil {
				t.Fatal(err)
			}
			callback := &Controller{Store: c.Store, HostID: c.HostID, AppID: c.AppID, ThreadID: c.ThreadID}
			action(t, callback, r.ID, "remove")
			tick(t, c)
			if view(t, c, r.ID).State != "removed" || f.steerCalls != 0 || !submitted && f.addCalls != 0 || submitted && f.deleteCalls != 1 {
				t.Fatal("removal was not durable")
			}
		})
	}
}

func TestForeignReceiptsAndIncompleteHistoryNeverReachNativeWrites(t *testing.T) {
	c, f, _ := fixture(t, true)
	for i, r := range []store.Receipt{{AppID: "other", ThreadID: "thread", ChatID: "chat"}, {AppID: "app", ThreadID: "other", ChatID: "chat"}, {AppID: "app", ThreadID: "thread", ChatID: "other"}} {
		r.MessageID = fmt.Sprint(i)
		if _, _, err := c.Store.Claim(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	tick(t, c)
	if f.addCalls != 0 {
		t.Fatal("foreign actor receipt dispatched")
	}
	r := claim(t, c, "incomplete")
	f.turns[0].ItemsView = "summary"
	if err := c.Tick(context.Background()); err == nil || f.addCalls != 0 || view(t, c, r.ID).State != "received" {
		t.Fatal("partial history permitted native mutation")
	}
	foreign := &Controller{Store: c.Store, Native: f, HostID: "host", AppID: "other", ThreadID: "thread"}
	if err := foreign.Tick(context.Background()); err == nil {
		t.Fatal("wrong deployment app accepted")
	}
}

func TestQ8SavedDeleteAndRejectedSteerStagesResumeAfterReopen(t *testing.T) {
	for _, stage := range []string{"steer", "restore"} {
		t.Run(stage, func(t *testing.T) {
			c, f, path := fixture(t, true)
			r := claim(t, c, "saved-stage")
			tick(t, c)
			n := view(t, c, r.ID)
			op := action(t, c, r.ID, "steer")
			ctx := context.Background()
			if err := c.Store.PrepareSubmissionAction(ctx, c.scope(), op.ID, "delete", "t1", n.QueueID, n.ClientID); err != nil {
				t.Fatal(err)
			}
			d, claimed, err := c.Store.ClaimSubmissionAttempt(ctx, c.scope(), r.ID, op.ID, "delete")
			if err != nil || !claimed {
				t.Fatal(err)
			}
			deleted, err := f.Delete(ctx, "thread", n.QueueID)
			if err != nil || !deleted {
				t.Fatal(err)
			}
			if err = c.Store.FinishSubmissionAttempt(ctx, c.scope(), d.ID, "confirmed", "", 1); err != nil {
				t.Fatal(err)
			}
			f.end()
			f.start("") // App independently changes to another active turn.
			if stage == "restore" {
				a, claimed, err := c.Store.ClaimSubmissionAttempt(ctx, c.scope(), r.ID, op.ID, "steer")
				if err != nil || !claimed {
					t.Fatal(err)
				}
				if err = f.Steer(ctx, "thread", a.ExpectedTurnID, r.Body, r.ClientID); err == nil {
					t.Fatal("expected native rejection")
				}
				if err = c.Store.FinishSubmissionAttempt(ctx, c.scope(), a.ID, "rejected", "", -1); err != nil {
					t.Fatal(err)
				}
			}
			c = reopen(t, c, path)
			tick(t, c)
			got, _ := c.Store.SubmissionAction(ctx, c.scope(), op.ID)
			if got.Result != "restored" || got.ExpectedTurnID != "t1" || f.deleteCalls != 1 || f.steerCalls != 1 || f.expected[0] != "t1" || f.addCalls != 2 {
				t.Fatal("durable stage resumed incorrectly")
			}
		})
	}
}

func TestLostDeleteThenAppConsumptionReportsConsumedWithoutSteer(t *testing.T) {
	c, f, _ := fixture(t, true)
	r := claim(t, c, "lost-delete-consumed")
	tick(t, c)
	n := view(t, c, r.ID)
	f.deleteBeforeErr = errors.New("lost")
	op := action(t, c, r.ID, "steer")
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("expected unknown delete")
	}
	f.consume(n.QueueID)
	tick(t, c)
	got, _ := c.Store.SubmissionAction(context.Background(), c.scope(), op.ID)
	if got.State != "rejected" || got.Result != "consumed" || view(t, c, r.ID).State != "consumed" || f.steerCalls != 0 || f.addCalls != 1 {
		t.Fatal("unknown delete consumption was reported as bridge deletion/steer")
	}
}

func TestLostRestoreReplyNeverAddsAgain(t *testing.T) {
	c, f, path := fixture(t, true)
	r := claim(t, c, "lost-restore")
	tick(t, c)
	f.afterDelete = func(f *fakeNative, _ string) {
		f.end()
		f.start("")
		f.addReplyErr = errors.New("lost restore")
	}
	op := action(t, c, r.ID, "steer")
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("expected unknown restore")
	}
	c = reopen(t, c, path)
	tick(t, c)
	got, _ := c.Store.SubmissionAction(context.Background(), c.scope(), op.ID)
	if got.Result != "restored" || f.addCalls != 2 || f.steerCalls != 1 || view(t, c, r.ID).State != "queued" {
		t.Fatal("restore response loss duplicated input")
	}
}

func TestPendingLocalRemoveIsAllowedWhileOtherInputIsUnknown(t *testing.T) {
	c, f, _ := fixture(t, true)
	claim(t, c, "unknown")
	f.addReplyErr = errors.New("lost")
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("expected unknown")
	}
	r := claim(t, c, "pending")
	if err := c.Store.StageSubmissions(context.Background(), c.scope()); err != nil {
		t.Fatal(err)
	}
	action(t, c, r.ID, "remove")
	if view(t, c, r.ID).State != "removed" || f.deleteCalls != 0 || f.addCalls != 1 {
		t.Fatal("safe local cancellation was blocked or reached native queue")
	}
}

func TestQ10LostStopReplyUsesCapturedActualTurn(t *testing.T) {
	c, f, path := fixture(t, true)
	r := claim(t, c, "queued-before-stop")
	tick(t, c)
	f.stopReplyErr = errors.New("lost stop")
	op, err := c.Stop(context.Background(), "stop-lost")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Tick(context.Background()); err == nil {
		t.Fatal("expected unknown stop")
	}
	f.start("") // A new App turn must never become the interrupt target.
	c = reopen(t, c, path)
	tick(t, c)
	got, _ := c.Store.SubmissionAction(context.Background(), c.scope(), op.ID)
	if got.Result != "stopped" || got.ExpectedTurnID != "t1" || f.interruptCalls != 1 || f.expected[0] != "t1" || view(t, c, r.ID).State != "queued" || f.active() != "t2" {
		t.Fatal("stop recovery retargeted or changed the preserved queue")
	}
}

func TestQ9ConcurrentRemoveAndSteerHaveOneDurableWinner(t *testing.T) {
	c, f, _ := fixture(t, true)
	r := claim(t, c, "conflicting-buttons")
	tick(t, c)
	n := view(t, c, r.ID)
	var wg sync.WaitGroup
	winners := make(chan store.SubmissionAction, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kind := "remove"
			if i%2 == 0 {
				kind = "steer"
			}
			a, err := c.ClaimAction(context.Background(), r.ID, kind, n.QueueID, n.Revision)
			if errors.Is(err, store.ErrSubmissionStale) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			winners <- a
		}(i)
	}
	wg.Wait()
	close(winners)
	var winner store.SubmissionAction
	for a := range winners {
		if winner.ID != "" && winner.ID != a.ID {
			t.Fatal("conflicting callbacks created multiple active actions")
		}
		winner = a
	}
	tick(t, c)
	wantSteer := 0
	if winner.Kind == "steer" {
		wantSteer = 1
	}
	if winner.ID == "" || f.deleteCalls != 1 || f.steerCalls != wantSteer {
		t.Fatal("concurrent conflict did not have exactly one remote winner")
	}
}

func TestDeleteSuccessLocalSaveFailureNeverProceedsToSteer(t *testing.T) {
	c, f, path := fixture(t, true)
	r := claim(t, c, "delete-save-failure")
	tick(t, c)
	action(t, c, r.ID, "steer")
	if _, err := c.Store.DB.Exec(`CREATE TRIGGER delete_save_fault BEFORE UPDATE ON submission_attempts WHEN OLD.stage='delete' AND NEW.state='confirmed' BEGIN SELECT RAISE(ABORT,'journal unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.Tick(context.Background()); err == nil || f.steerCalls != 0 || f.deleteCalls != 1 {
		t.Fatal("steer proceeded before delete success was durable")
	}
	c = reopen(t, c, path)
	if _, err := c.Store.DB.Exec(`DROP TRIGGER delete_save_fault`); err != nil {
		t.Fatal(err)
	}
	tick(t, c)
	if view(t, c, r.ID).State != "unknown" || f.steerCalls != 0 || f.deleteCalls != 1 {
		t.Fatal("vanished queue was treated as proof of bridge delete")
	}
}

func TestUnknownQueueStartDoesNotInferSuccessFromAppTurn(t *testing.T) {
	c, f, path := fixture(t, false)
	f.paused = true
	claim(t, c, "unknown-start")
	tick(t, c)
	f.startReplyErr = errors.New("lost start")
	op, err := c.Continue(context.Background(), "unknown-continue-event")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Tick(context.Background()); err == nil {
		t.Fatal("expected unknown start")
	}
	c = reopen(t, c, path)
	tick(t, c)
	got, _ := c.Store.SubmissionAction(context.Background(), c.scope(), op.ID)
	if got.State != "unknown" || f.starts != 1 || f.steerCalls != 0 {
		t.Fatal("unidentified queue start was retried or falsely confirmed")
	}
}

func TestUnknownAddRequiresExactClientIDAndDefiniteRejectionIsTerminal(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(fmt.Sprint(rejected), func(t *testing.T) {
			c, f, _ := fixture(t, true)
			r := claim(t, c, "unaccepted")
			f.addReject = errors.New("unknown before acknowledgement")
			want := "unknown"
			if rejected {
				f.addReject = &codexclient.RPCError{Code: -32600}
				want = "failed"
			}
			err := c.Tick(context.Background())
			if !rejected && err == nil || rejected && err != nil {
				t.Fatal("response class was lost")
			}
			nearClient := r.ClientID + "-other"
			f.queue = append(f.queue, codexclient.Queued{ID: "other-q", ClientUserMessageID: &nearClient})
			for i := 0; i < 3; i++ {
				tick(t, c)
			}
			if view(t, c, r.ID).State != want || f.addCalls != 1 {
				t.Fatal("absence or approximate correlation triggered retry/confirmation")
			}
		})
	}
}

func TestLocallyCancelledReceiptNeverDispatchesAfterReconnect(t *testing.T) {
	c, f, path := fixture(t, true)
	r := claim(t, c, "cancel-before-reconnect")
	ctx := context.Background()
	if e := c.Store.StageSubmissions(ctx, c.scope()); e != nil {
		t.Fatal(e)
	}
	if e := c.Store.StageQueueCardWithOptions(ctx, c.scope(), r.ID, store.QueueCardOptions{LocalPending: true, Offline: true}); e != nil {
		t.Fatal(e)
	}
	m, e := c.Store.Mirror(ctx, c.HostID, c.ThreadID, store.QueueCardKey(r.ID))
	if e != nil {
		t.Fatal(e)
	}
	attempt, ok, e := c.Store.ClaimMirror(ctx, c.HostID, c.ThreadID, m.Key, m.Revision, 5000)
	if e != nil || !ok {
		t.Fatal(e)
	}
	if e = c.Store.FinishMirrorAttempt(ctx, attempt.ID, "confirmed", "actual-card"); e != nil {
		t.Fatal(e)
	}
	n := view(t, c, r.ID)
	if _, e = c.Store.CancelPendingSubmission(ctx, c.scope(), "operator", []string{"operator"}, n.ChatID, "actual-card", n.ActionToken, n.Revision); e != nil {
		t.Fatal(e)
	}
	c = reopen(t, c, path)
	tick(t, c)
	tick(t, c)
	if f.addCalls != 0 || f.deleteCalls != 0 || f.steerCalls != 0 || f.starts != 0 || view(t, c, r.ID).State != "removed" {
		t.Fatal("cancelled receipt reached native execution")
	}
}
