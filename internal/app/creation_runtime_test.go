package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
)

type creationRuntimeNative struct {
	*runtimeNative
	attaches, adds, failedMetadata atomic.Int32
	failMetadata                   atomic.Bool
	holdAttach                     <-chan struct{}
}

func (n *creationRuntimeNative) Metadata(ctx context.Context, id string) (codexclient.Thread, error) {
	if n.failMetadata.Load() {
		n.failedMetadata.Add(1)
		return codexclient.Thread{}, errors.New("fixture metadata temporarily unavailable")
	}
	return n.runtimeNative.Metadata(ctx, id)
}
func (n *creationRuntimeNative) Attach(ctx context.Context, id string) (codexclient.Thread, error) {
	n.attaches.Add(1)
	if n.holdAttach != nil {
		select {
		case <-n.holdAttach:
		case <-ctx.Done():
			return codexclient.Thread{}, ctx.Err()
		}
	}
	return n.runtimeNative.Attach(ctx, id)
}
func (n *creationRuntimeNative) Add(_ context.Context, _, _ string, client string) (codexclient.Queued, error) {
	n.adds.Add(1)
	return codexclient.Queued{ID: "initial-queue", ClientUserMessageID: &client}, nil
}
func (n *creationRuntimeNative) Queue(context.Context, string) ([]codexclient.Queued, error) {
	return nil, nil
}
func (n *creationRuntimeNative) Delete(context.Context, string, string) (bool, error) {
	return false, errors.New("unexpected delete")
}
func (n *creationRuntimeNative) Steer(context.Context, string, string, string, string) error {
	return errors.New("unexpected steer")
}
func (n *creationRuntimeNative) Interrupt(context.Context, string, string) error {
	return errors.New("unexpected interrupt")
}
func (n *creationRuntimeNative) StartQueue(context.Context, string) error {
	return errors.New("unexpected queue start")
}

func creationRuntimeService(r *Runtime) *Service {
	var cfg config.Config
	cfg.Host.ID, cfg.Feishu.AppID = r.HostID, r.AppID
	cfg.Access.OwnerOpenID, cfg.Access.OperatorOpenIDs = r.OwnerID, []string{"creator"}
	cfg.Creation.Enabled, cfg.Creation.AllowedRoots = true, r.AllowedRoots
	s := &Service{Store: r.Store, Config: cfg}
	r.CanAttach, r.CreationAuthorize = s.creationAttachGate, s.authorizeCreation
	r.DiscoveryEnabled = false
	return s
}

// Native creation is confirmed here, but the platform route and first input
// can be materialized later, including after this Runtime has entered Run.
func prepareRuntimeCreation(t *testing.T, r *Runtime, n *creationRuntimeNative, thread string) store.CreationDraft {
	return prepareRuntimeCreationWorkspace(t, r, n, thread, store.CreationIntent{Directory: r.AllowedRoots[0]})
}

func prepareRuntimeCreationWorkspace(t *testing.T, r *Runtime, n *creationRuntimeNative, thread string, plan store.CreationIntent) store.CreationDraft {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Store.BeginBaseline(ctx, r.HostID, r.AppID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.CompleteBaseline(ctx, r.HostID, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	d, fresh, err := r.Store.ReceiveCreationDraft(ctx, store.CreationDraft{HostID: r.HostID, AppID: r.AppID, CreatorID: "creator", SourceMessageID: "source-" + thread, SourceChatID: "entry-chat", SourceChatType: "p2p", Body: "initial task", Directory: plan.Directory, FollowMode: "group", WorkspaceMode: plan.WorkspaceMode, BaseBranch: plan.BaseBranch, CreatedAtMS: now, ExpiresAtMS: now + time.Hour.Milliseconds()})
	if err != nil || !fresh {
		t.Fatal("creation draft fixture failed", err)
	}
	plan.OwnerID, plan.Name, plan.Members = r.OwnerID, "created task", []string{r.OwnerID, "creator"}
	i, claimed, err := r.Store.ClaimCreationIntent(ctx, d, plan, now)
	if err != nil || !claimed {
		t.Fatal("creation intent fixture failed", err)
	}
	params, err := codexclient.CreationRequestJSON(codexclient.CreationSpec{IntentID: i.ID, CWD: i.ExecutionDirectory(), ProjectID: i.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct{ name, params, id string }{{"group", `{"uuid":"fixture-group"}`, "created-chat"}, {"root", `{"chatId":"created-chat"}`, "created-root"}, {"native", params, thread}}
	if i.WorkspaceMode == "worktree" {
		marker, err := json.Marshal(struct{ DraftID, Source, Repository, Common, Branch, Commit, Destination, CWD string }{i.DraftID, i.Directory, i.GitRoot, i.GitCommonDir, i.BaseBranch, i.BaseCommit, i.WorktreePath, i.ExecutionDirectory()})
		if err != nil || os.WriteFile(filepath.Join(filepath.Dir(i.WorktreePath), ".codex-feishu-"+i.DraftID+".json"), marker, 0600) != nil {
			t.Fatal("worktree marker fixture failed", err)
		}
		steps = append([]struct{ name, params, id string }{{"worktree", string(marker), i.WorktreePath}}, steps...)
	}
	for _, step := range steps {
		a, err := r.Store.PrepareCreationAttempt(ctx, d.ID, step.name, step.params, now)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := r.Store.DispatchCreationAttempt(ctx, a); err != nil || !ok {
			t.Fatal("creation step fixture dispatch failed", err)
		}
		if err := r.Store.FinishCreationAttempt(ctx, a, "confirmed", step.id, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	source, _ := codexclient.CreationThreadSource(i.ID)
	marker, _ := json.Marshal(source)
	n.mu.Lock()
	n.threads[thread] = codexclient.Thread{ID: thread, CWD: i.ExecutionDirectory(), Source: json.RawMessage(`"vscode"`), ThreadSource: marker}
	n.mu.Unlock()
	return d
}

func acceptRuntimeCreation(t *testing.T, r *Runtime, n *creationRuntimeNative, d store.CreationDraft, receipt store.Receipt) {
	t.Helper()
	c := &submissions.Controller{Store: r.Store, Native: n, HostID: r.HostID, AppID: r.AppID, ThreadID: receipt.ThreadID}
	if err := c.SubmitInitial(context.Background(), receipt.ID); err != nil {
		t.Fatal("initial input fixture was not accepted", err)
	}
	i, err := r.Store.CreationIntent(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Supply a real-shaped, correlated first turn and final reply to exercise
	// ordinary history staging/delivery after takeover, not just actor presence.
	n.set(receipt.ThreadID, i.ExecutionDirectory(), "completed")
	n.mu.Lock()
	n.turns[receipt.ThreadID][0].Items[0].ClientID = &receipt.ClientID
	source, _ := codexclient.CreationThreadSource(i.ID)
	marker, _ := json.Marshal(source)
	meta := n.threads[receipt.ThreadID]
	meta.ThreadSource = marker
	n.threads[receipt.ThreadID] = meta
	n.mu.Unlock()
	if err := r.Store.CompleteCreation(context.Background(), d.ID); err != nil {
		t.Fatal("initial input confirmation fixture failed", err)
	}
}

func runCreationRuntime(t *testing.T, r *Runtime, n *creationRuntimeNative) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	r.EventObserved = func(e codexclient.Event) {
		if e.Method == "fixture/creation-ready" {
			close(ready)
		}
	}
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("creation runtime stopped unexpectedly: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("creation runtime did not join")
		}
	})
	n.events <- codexclient.Event{Method: "fixture/creation-ready", Params: json.RawMessage(`{}`)}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not enter event loop")
	}
}

func creationRuntimeDelivered(g *runtimeGateway) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, write := range g.writes {
		if write.op == "reply" && write.parent == "created-root" {
			return true
		}
	}
	return false
}

func TestCreationRuntimePendingBindingCannotAttachBeforeInitialAcceptance(t *testing.T) {
	db, base, g, r := runtimeFixture(t)
	n := &creationRuntimeNative{runtimeNative: base}
	r.Native = n
	creationRuntimeService(r)
	d := prepareRuntimeCreation(t, r, n, "created-thread")
	_, receipt, err := db.MaterializeCreation(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	i, err := db.CreationIntent(context.Background(), d.ID)
	if err != nil || i.State != "working" {
		t.Fatal("pending binding fixture is not working", err)
	}
	gateSeen := make(chan struct{}, 1)
	r.Notice = func(code string) {
		if code == "thread.attach_pending" {
			select {
			case gateSeen <- struct{}{}:
			default:
			}
		}
	}
	runCreationRuntime(t, r, n)
	select {
	case <-gateSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("pending creation did not reach attach gate")
	}
	if n.attaches.Load() != 0 || creationRuntimeDelivered(g) {
		t.Fatal("pending materialized binding attached or delivered before first input")
	}
	acceptRuntimeCreation(t, r, n, d, receipt)
	if !r.Activate(receipt.ThreadID) {
		t.Fatal("confirmed creation activation was not admitted")
	}
	runtimeWait(t, func() bool { return n.attaches.Load() > 0 && creationRuntimeDelivered(g) })
	g.mu.Lock()
	groups := g.groups
	g.mu.Unlock()
	if groups != 0 || n.adds.Load() != 1 {
		t.Fatal("handoff duplicated a group or initial input")
	}
}

func TestCreationRuntimeLateConfirmedIntentRecoversWithoutReconnect(t *testing.T) {
	for _, mode := range []string{"no-activate", "first-activate-metadata-fails"} {
		t.Run(mode, func(t *testing.T) {
			db, base, g, r := runtimeFixture(t)
			n := &creationRuntimeNative{runtimeNative: base}
			var releaseAttach chan struct{}
			if mode == "first-activate-metadata-fails" {
				releaseAttach = make(chan struct{})
				n.holdAttach = releaseAttach
			}
			r.Native = n
			creationRuntimeService(r)
			d := prepareRuntimeCreation(t, r, n, "created-thread")
			activationFailed := make(chan struct{}, 1)
			r.Notice = func(code string) {
				if code == "thread.activation_pending" {
					select {
					case activationFailed <- struct{}{}:
					default:
					}
				}
			}
			runCreationRuntime(t, r, n)
			if r.ActorStarted("created-thread") {
				t.Fatal("fixture already had an actor before late materialization")
			}
			_, receipt, err := db.MaterializeCreation(context.Background(), d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if releaseAttach != nil {
				n.failMetadata.Store(true)
			}
			acceptRuntimeCreation(t, r, n, d, receipt)
			if releaseAttach != nil {
				if !r.Activate(receipt.ThreadID) {
					t.Fatal("first activation was not queued")
				}
				select {
				case <-activationFailed:
				case <-time.After(5 * time.Second):
					t.Fatal("initial activation did not exercise metadata failure")
				}
				n.failMetadata.Store(false)
				close(releaseAttach)
			}
			// No second Activate, wake event, reconnect, or Runtime restart. Its
			// periodic durable-confirmation scan alone must create the actor.
			runtimeWait(t, func() bool { return r.ActorStarted(receipt.ThreadID) && creationRuntimeDelivered(g) })
			g.mu.Lock()
			groups := g.groups
			g.mu.Unlock()
			if groups != 0 || n.attaches.Load() == 0 || n.adds.Load() != 1 {
				t.Fatal("late takeover duplicated creation or never attached")
			}
			if releaseAttach != nil && n.failedMetadata.Load() == 0 {
				t.Fatal("failed activation fixture did not fail native metadata")
			}
		})
	}
}
