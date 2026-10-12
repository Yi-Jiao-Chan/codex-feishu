package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/control"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

type managedBindingNative struct{ *verificationServiceNative }

func (n *managedBindingNative) Add(ctx context.Context, thread, body, client string) (codexclient.Queued, error) {
	if _, err := n.Metadata(ctx, thread); err != nil || !strings.HasPrefix(thread, "probe-") {
		return codexclient.Queued{}, errors.New("wrong fixture thread")
	}
	n.adds.Add(1)
	input, _ := json.Marshal(codexclient.TextInput(body))
	q := codexclient.Queued{ID: "fixture-queued-" + client, Input: input, ClientUserMessageID: &client}
	n.qmu.Lock()
	n.queue = append(n.queue, q)
	n.qmu.Unlock()
	return q, nil
}

func (n *managedBindingNative) AttachLocalVerification(ctx context.Context, id string, spec codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
	thread, err := n.Metadata(ctx, id)
	if err == nil && (thread.ID != id || thread.CWD != spec.CWD) {
		return codexclient.Thread{}, errors.New("fixture identity mismatch")
	}
	return thread, err
}

type managedBindingGateway struct {
	*serviceGateway
	owner   string
	creates atomic.Int32
	reads   atomic.Int32
}

func (g *managedBindingGateway) CreateGroup(context.Context, string, string, []string, string) (string, error) {
	g.creates.Add(1)
	return "verification-chat", nil
}
func (g *managedBindingGateway) MemberIDs(context.Context, string) ([]string, error) {
	g.reads.Add(1)
	return []string{g.owner, "ou_bot"}, nil
}

func TestManagedBindingActivatesOnExistingServiceAndStateIsReadOnly(t *testing.T) {
	db, cfg, _ := serviceFixture(t)
	g := &managedBindingGateway{serviceGateway: &serviceGateway{runtimeGateway: &runtimeGateway{}, listening: make(chan struct{})}, owner: cfg.Access.OwnerOpenID}
	n := &managedBindingNative{verificationServiceNative: &verificationServiceNative{serviceNative: &serviceNative{runtimeNative: &runtimeNative{threads: map[string]codexclient.Thread{}, turns: map[string][]codexclient.Turn{}, events: make(chan codexclient.Event, 64)}}, done: make(chan struct{})}}
	var connections atomic.Int32
	s := &Service{Config: cfg, Store: db, Gateway: g, BotID: "ou_bot", Connect: func(context.Context) (ServiceNative, error) { connections.Add(1); return n, nil }}
	life, stop := context.WithCancel(context.Background())
	defer stop()
	root := filepath.Join(t.TempDir(), "binding-resources")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	manager := &verification.NativeManager{Store: db, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: cfg.Access.OwnerOpenID, Commit: "fixture-build", BinarySHA256: strings.Repeat("c", 64), Current: s.VerificationCreator}
	sessions, err := verification.NewNativeSessions(life, manager, root)
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	groups := &verification.GroupManager{Store: db, Gateway: g, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: cfg.Access.OwnerOpenID, BotID: "ou_bot", Commit: manager.Commit, BinarySHA256: manager.BinarySHA256, Ready: g.Ready}
	bindings := &verification.BindingManager{Sessions: sessions, Groups: groups, Activate: s.ActivateVerificationBinding, ActorStarted: s.VerificationActorStarted}
	s.VerificationGroups, s.VerificationThreads, s.VerificationBindings = groups, sessions, bindings
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- s.Run(life) }()
	defer func() { stop(); <-serviceDone }()
	select {
	case <-g.listening:
	case err := <-serviceDone:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("service receiver did not start")
	}
	deadline := time.Now().Add(3 * time.Second)
	for s.VerificationCreator() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.VerificationCreator() == nil {
		t.Fatal("existing connection unavailable")
	}
	endpointRoot, err := os.MkdirTemp("", "cfbind-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(endpointRoot)
	endpoint, identity := filepath.Join(endpointRoot, "control.sock"), strings.Repeat("a", 64)
	server, err := control.Listen(life, endpoint, identity, s.Health)
	if err != nil {
		t.Fatal(err)
	}
	server.VerificationGroups, server.VerificationThreads, server.VerificationBindings = groups, sessions, bindings
	controlDone := make(chan error, 1)
	go func() { controlDone <- server.Serve(life) }()
	defer func() { stop(); server.Close(); <-controlDone }()
	id := store.ID()
	if _, err := control.VerifyBinding(life, endpoint, identity, id, true); !errors.Is(err, verification.ErrNotFound) && !errors.Is(err, control.ErrUnavailable) {
		t.Fatal("binding-only created missing components", err)
	}
	if g.creates.Load() != 0 || n.starts.Load() != 0 {
		t.Fatal("binding created group/thread")
	}
	if _, err := control.VerifyGroup(life, endpoint, identity, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := control.VerifyThreadCreation(life, endpoint, identity, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := control.VerifyBinding(life, endpoint, identity, id, true); !errors.Is(err, control.ErrUnavailable) {
		t.Fatal("missing owner observation was accepted", err)
	}
	message := receiverMessage(t, "verification-chat", cfg.Access.OwnerOpenID, "owner-proof", "group", true)
	body, _ := json.Marshal(map[string]string{"text": "@_user_1 /verify-group " + id})
	message.Event.Message.Content = strPointer(string(body))
	if err := g.message(life, message); err != nil {
		t.Fatal(err)
	}
	result, err := control.VerifyBinding(life, endpoint, identity, id, true)
	if err != nil || result.BindingStatus != "pass" || !result.ActivationRequested || !result.CreatorConnected || !result.ResourcesReady {
		t.Fatal(result, err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for !result.ActorStarted && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		result, err = control.VerifyBinding(life, endpoint, identity, id, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err = control.VerifyBinding(life, endpoint, identity, id, false)
	if err != nil || !result.ActorStarted || result.ActivationRequested {
		t.Fatal("actor was not started or query claimed activation", result)
	}
	for _, begin := range []bool{true, false} {
		result, err := control.VerifyBinding(life, endpoint, identity, id, begin)
		if err != nil || result.ActivationRequested != begin || result.BindingStatus != "pass" || !result.ActorStarted {
			t.Fatal(result, err)
		}
	}
	if connections.Load() != 1 || g.listens.Load() != 1 || n.starts.Load() != 1 || g.creates.Load() != 1 || n.adds.Load() != 0 {
		t.Fatal("binding duplicated service, targets, or input")
	}
	var receipts int
	if err := db.DB.QueryRow(`SELECT count(*) FROM m0_receipts`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("group observation became input", receipts, err)
	}
	// The result only proves an actor exists locally, not actual App UI.
	if health, err := s.Health(life); err != nil || !health.VerificationBindingSupported {
		t.Fatal("binding capability missing", err)
	}
	t.Run("release original local model via existing service", func(t *testing.T) {
		birth, err := db.VerificationNative(life, id, cfg.Host.ID, cfg.Feishu.AppID, cfg.Access.OwnerOpenID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := control.VerifyTurnRelease(life, endpoint, identity, id); !errors.Is(err, control.ErrUnavailable) {
			t.Fatal("release opened an unstarted fixture", err)
		}
		modelContext, cancel := context.WithTimeout(life, 3*time.Second)
		defer cancel()
		responseDone := make(chan error, 1)
		go func() {
			request, err := http.NewRequestWithContext(modelContext, http.MethodPost, birth.ModelURL+"/responses", strings.NewReader(`{"model":"fixture-model","stream":true}`))
			if err != nil {
				responseDone <- err
				return
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				responseDone <- err
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err == nil && !strings.Contains(string(body), `"type":"response.completed"`) {
				err = errors.New("local fixture response did not complete")
			}
			responseDone <- err
		}()
		// Calls plus the flushed Started gate are not exposed in health. The
		// idempotent release method returns not-ready until that gate is reached.
		deadline := time.Now().Add(2 * time.Second)
		var released verification.BindingResult
		for time.Now().Before(deadline) {
			released, err = control.VerifyTurnRelease(life, endpoint, identity, id)
			if err == nil {
				break
			}
			if !errors.Is(err, control.ErrUnavailable) {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
		}
		if err != nil || !released.ModelReleased || released.ActivationRequested {
			t.Fatal(released, err)
		}
		select {
		case err := <-responseDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-modelContext.Done():
			t.Fatal("managed release did not open original model response")
		}
		if _, err := control.VerifyTurnRelease(life, endpoint, identity, id); err != nil {
			t.Fatal("repeat release was not idempotent", err)
		}
		state, err := control.VerifyBinding(life, endpoint, identity, id, false)
		if err != nil || !state.ModelReleased || state.ActivationRequested || state.ExpiresAtMS != released.ExpiresAtMS || connections.Load() != 1 || g.listens.Load() != 1 || n.starts.Load() != 1 || g.creates.Load() != 1 || n.adds.Load() != 0 {
			t.Fatal("release/query recreated component, submitted input or changed window", state, err)
		}
		if health, err := s.Health(life); err != nil || !health.VerificationTurnReleaseSupported {
			t.Fatal("release capability missing", health, err)
		}
	})
	t.Run("owner input evidence from receiver and actor", func(t *testing.T) {
		thread := result.ThreadID
		n.mu.Lock()
		n.turns[thread] = []codexclient.Turn{{ID: "fixture-followup-turn", Status: "inProgress", ItemsView: "full"}}
		n.mu.Unlock()
		message := receiverMessage(t, "verification-chat", cfg.Access.OwnerOpenID, "fixture-owner-followup", "group", true)
		body, _ := json.Marshal(map[string]string{"text": "@_user_1 fixture owner follow-up"})
		message.Event.Message.Content = strPointer(string(body))
		for range 2 {
			if err := g.message(life, message); err != nil {
				t.Fatal(err)
			}
		}
		var queued verification.BindingResult
		runtimeWait(t, func() bool {
			var err error
			queued, err = control.VerifyBinding(life, endpoint, identity, id, false)
			return err == nil && queued.InputEvidenceStatus == "pass" && queued.InputEvidence != nil && queued.InputEvidence.StagedInputs == 1 && queued.InputEvidence.AddConfirmed == 1 && queued.InputEvidence.QueuedSnapshot == 1
		})
		if n.adds.Load() != 1 || queued.InputEvidence.HistoryLinked != 0 || queued.InputEvidence.SteerConfirmed != 0 || queued.InputEvidence.SteerHistoryLinked != 0 {
			t.Fatal("duplicate input or queued evidence promoted to history/steer", queued)
		}
		receipts, err := db.ReceiptsForDeployment(life, cfg.Host.ID, thread)
		if err != nil || len(receipts) != 1 {
			t.Fatal(receipts, err)
		}
		client := receipts[0].ClientID
		content, _ := json.Marshal(map[string]string{"type": "text", "text": "fixture owner follow-up"})
		n.qmu.Lock()
		n.queue = nil
		n.qmu.Unlock()
		n.mu.Lock()
		n.turns[thread][0].Items = []codexclient.Item{{ID: "fixture-native-user-item", Type: "userMessage", ClientID: &client, Content: []json.RawMessage{content}}}
		n.mu.Unlock()
		s.Wake(thread)
		runtimeWait(t, func() bool {
			state, err := control.VerifyBinding(life, endpoint, identity, id, false)
			return err == nil && state.InputEvidence != nil && state.InputEvidence.HistoryLinked == 1 && state.InputEvidence.QueuedSnapshot == 0
		})
		if connections.Load() != 1 || g.listens.Load() != 1 || n.starts.Load() != 1 || g.creates.Load() != 1 || n.adds.Load() != 1 {
			t.Fatal("input observation recreated service/components or resent input")
		}
	})
}

func TestRuntimeActivationReportsQueueAdmissionAndLocalPresence(t *testing.T) {
	r := &Runtime{}
	if r.Activate("thread") || r.ActorStarted("thread") {
		t.Fatal("stopped runtime admitted activation")
	}
	r.running, r.activations = true, make(chan string, 1)
	r.actors = map[string]*threadActor{}
	if !r.Activate("thread") || r.Activate("other") || r.ActorStarted("thread") {
		t.Fatal("queue admission masqueraded as actor start")
	}
	r.actors["thread"] = &threadActor{thread: "thread"}
	if !r.ActorStarted("thread") {
		t.Fatal("local actor presence not reported")
	}
}
