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
	"sync"
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
	owner           string
	creates         atomic.Int32
	reads           atomic.Int32
	finalStarted    chan struct{}
	finalRelease    chan struct{}
	finalOnce       sync.Once
	reactionParents []string
}

func (g *managedBindingGateway) CreateGroupWithDescription(ctx context.Context, name, owner string, members []string, uuid, _ string) (string, error) {
	return g.CreateGroup(ctx, name, owner, members, uuid)
}

func (g *managedBindingGateway) CreateGroup(context.Context, string, string, []string, string) (string, error) {
	g.creates.Add(1)
	return "verification-chat", nil
}
func (g *managedBindingGateway) MemberIDs(context.Context, string) ([]string, error) {
	g.reads.Add(1)
	return []string{g.owner, "ou_bot"}, nil
}

func (g *managedBindingGateway) ReplyCard(ctx context.Context, parent, card, uuid string) (string, error) {
	if g.finalStarted != nil && strings.Contains(card, "fixture-final-ready") {
		g.finalOnce.Do(func() { close(g.finalStarted) })
		select {
		case <-g.finalRelease:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return g.runtimeGateway.ReplyCard(ctx, parent, card, uuid)
}

func (g *managedBindingGateway) AddReaction(_ context.Context, message, emoji string) (string, error) {
	g.muFeedback.Lock()
	defer g.muFeedback.Unlock()
	g.reactions = append(g.reactions, emoji)
	g.reactionParents = append(g.reactionParents, message)
	return "reaction-" + emoji, nil
}

func TestManagedBindingActivatesOnExistingServiceAndStateIsReadOnly(t *testing.T) {
	db, cfg, _ := serviceFixture(t)
	g := &managedBindingGateway{serviceGateway: &serviceGateway{runtimeGateway: &runtimeGateway{}, listening: make(chan struct{})}, owner: cfg.Access.OwnerOpenID, finalStarted: make(chan struct{}), finalRelease: make(chan struct{})}
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
	if health, err := s.Health(life); err != nil || !health.VerificationBindingSupported || health.BindingHydrationScope == nil || health.BindingHydrationScope.OrdinaryBindings != 1 || health.BindingHydrationScope.VerificationBindings != 1 || health.Persistent.Bindings != 2 {
		t.Fatal("binding capability missing", err)
	}
	t.Run("orphaned private route is not ordinary hydration", func(t *testing.T) {
		// A new instance has no original local-model session. Both the marker
		// and actual route persist, while their authorization remains frozen.
		restarted := &Service{Store: db, Config: cfg, Gateway: g, BotID: "ou_bot", actionsReady: map[string]bool{"thread": true, result.ThreadID: true}}
		restarted.running.Store(true)
		restarted.nativeConnected.Store(true)
		health, err := restarted.Health(life)
		if err != nil || health.BindingHydrationScope == nil || health.BindingHydrationScope.OrdinaryBindings != 1 || health.BindingHydrationScope.VerificationBindings != 1 || health.HydratedBindings != 1 || health.Persistent.Bindings != 2 {
			t.Fatal("orphaned fixture altered ordinary readiness or erased persistent facts", health, err)
		}
		if _, _, _, err := restarted.beginVerificationOperation(life, result.ThreadID); !errors.Is(err, verification.ErrNotReady) {
			t.Fatal("health classification authorized an orphaned test route", err)
		}
	})
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
	t.Run("progress and final retain original reply and completion feedback", func(t *testing.T) {
		thread, parent := result.ThreadID, "fixture-owner-followup"
		phase := "commentary"
		n.mu.Lock()
		user := n.turns[thread][0].Items[0]
		n.turns[thread][0].Items = []codexclient.Item{user, {ID: "fixture-progress-one", Type: "agentMessage", Phase: &phase, Text: "fixture progress one"}}
		n.mu.Unlock()
		s.Wake(thread)
		var progress store.Mirror
		runtimeWait(t, func() bool {
			mirrors, err := db.Mirrors(life, cfg.Host.ID, thread)
			if err != nil {
				return false
			}
			for _, m := range mirrors {
				g.mu.Lock()
				wireHasProgress := false
				for _, write := range g.writes {
					wireHasProgress = wireHasProgress || strings.Contains(write.card, "fixture progress one")
				}
				g.mu.Unlock()
				if m.Kind == "progress" && m.TurnID == "fixture-followup-turn" && m.RemoteID != "" && m.Card == "" && m.CardHash != "" && m.DeliveredRevision == m.Revision && wireHasProgress {
					progress = m
					return true
				}
			}
			return false
		})
		n.mu.Lock()
		n.turns[thread][0].Items = []codexclient.Item{user, {ID: "fixture-progress-one", Type: "agentMessage", Phase: &phase, Text: "fixture progress one"}, {ID: "fixture-progress-two", Type: "agentMessage", Phase: &phase, Text: "fixture progress two"}, {ID: "fixture-private-reasoning", Type: "reasoning", Text: "PRIVATE_REASONING"}, {ID: "fixture-command", Type: "commandExecution", Status: "completed", Text: "PRIVATE_TOOL_OUTPUT"}}
		n.mu.Unlock()
		s.Wake(thread)
		runtimeWait(t, func() bool {
			mirrors, err := db.Mirrors(life, cfg.Host.ID, thread)
			if err != nil {
				return false
			}
			for _, m := range mirrors {
				if m.Kind == "progress" && m.TurnID == "fixture-followup-turn" && m.RemoteID == progress.RemoteID && m.Card == "" && m.CardHash != "" && m.DeliveredRevision == m.Revision {
					g.mu.Lock()
					defer g.mu.Unlock()
					for _, write := range g.writes {
						if write.op == "update" && write.target == progress.RemoteID && strings.Contains(write.card, "fixture progress two") {
							return true
						}
					}
				}
			}
			return false
		})
		finalPhase := "final_answer"
		now := time.Now().Unix()
		n.mu.Lock()
		n.turns[thread][0].Status, n.turns[thread][0].CompletedAt = "completed", &now
		n.turns[thread][0].Items = append(append([]codexclient.Item(nil), n.turns[thread][0].Items...), codexclient.Item{ID: "fixture-final", Type: "agentMessage", Phase: &finalPhase, Text: "fixture-final-ready"})
		n.mu.Unlock()
		s.Wake(thread)
		select {
		case <-g.finalStarted:
		case <-time.After(8 * time.Second):
			t.Fatal("final reply was not dispatched")
		}
		g.muFeedback.Lock()
		premature := false
		for _, emoji := range g.reactions {
			premature = premature || emoji == "CheckMark"
		}
		g.muFeedback.Unlock()
		if premature {
			t.Fatal("completion reaction preceded final delivery acknowledgement")
		}
		beforeDelivery, err := control.VerifyBinding(life, endpoint, identity, id, false)
		if err != nil || beforeDelivery.DeliveryEvidenceStatus != "pass" || beforeDelivery.DeliveryEvidence == nil || beforeDelivery.DeliveryEvidence.FinalCards != 1 || beforeDelivery.DeliveryEvidence.FinalConfirmed != 0 || beforeDelivery.DeliveryEvidence.FinalTurnsDelivered != 0 || beforeDelivery.DeliveryEvidence.UnresolvedAttempts < 1 {
			t.Fatal("unacknowledged final was reported as delivered", beforeDelivery.DeliveryEvidence, err)
		}
		close(g.finalRelease)
		runtimeWait(t, func() bool {
			g.muFeedback.Lock()
			defer g.muFeedback.Unlock()
			for _, emoji := range g.reactions {
				if emoji == "CheckMark" {
					return true
				}
			}
			return false
		})
		mirrors, err := db.Mirrors(life, cfg.Host.ID, thread)
		if err != nil {
			t.Fatal(err)
		}
		progressCount, finalCount := 0, 0
		var finalID string
		for _, m := range mirrors {
			if m.Kind == "human" || strings.Contains(m.Card, "fixture owner follow-up") || strings.Contains(m.Card, "PRIVATE_REASONING") || strings.Contains(m.Card, "PRIVATE_TOOL_OUTPUT") {
				t.Fatal("human echo or private content mirrored", m.Kind)
			}
			if m.Kind == "progress" && m.TurnID == "fixture-followup-turn" {
				progressCount++
				if m.RemoteID != progress.RemoteID || !m.Terminal || m.ParentMessageID != parent || m.DeliveredRevision != m.Revision || m.Card != "" || m.CardHash == "" {
					t.Fatal("progress lost original card, final state or reply relation", m.Key)
				}
			}
			if m.Kind == "final" {
				finalCount++
				finalID = m.RemoteID
				if m.RemoteID == "" || m.RemoteID == progress.RemoteID || m.ParentMessageID != parent || m.DeliveredRevision != m.Revision || m.Card != "" || m.CardHash == "" {
					t.Fatal("final was not a separate confirmed reply", m.Key)
				}
			}
		}
		if progressCount != 1 || finalCount != 1 {
			t.Fatal("unexpected per-turn card counts", progressCount, finalCount)
		}
		g.mu.Lock()
		writes := append([]runtimeWrite(nil), g.writes...)
		g.mu.Unlock()
		progressUpdates, finalReplies := 0, 0
		for _, write := range writes {
			if write.op == "send" || write.op == "reply" && write.parent != parent {
				t.Fatal("message was not a reply to original user input", write.op, write.parent)
			}
			if write.op == "update" && write.target == progress.RemoteID {
				progressUpdates++
			}
			if write.op == "reply" && strings.Contains(write.card, "fixture-final-ready") {
				finalReplies++
			}
		}
		if progressUpdates < 2 || finalReplies != 1 || finalID == "" || n.adds.Load() != 1 {
			t.Fatal("progress was reposted, final duplicated or input resent", progressUpdates, finalReplies)
		}
		g.muFeedback.Lock()
		reactionParents := append([]string(nil), g.reactionParents...)
		g.muFeedback.Unlock()
		for _, target := range reactionParents {
			if target != parent {
				t.Fatal("reaction did not target original user message", target)
			}
		}
		afterDelivery, err := control.VerifyBinding(life, endpoint, identity, id, false)
		if err != nil || afterDelivery.DeliveryEvidenceStatus != "pass" || afterDelivery.DeliveryEvidence == nil || afterDelivery.DeliveryEvidence.ProgressCards != 1 || afterDelivery.DeliveryEvidence.ProgressConfirmed != 1 || afterDelivery.DeliveryEvidence.FinalCards != 1 || afterDelivery.DeliveryEvidence.FinalConfirmed != 1 || afterDelivery.DeliveryEvidence.FinalTurns != 1 || afterDelivery.DeliveryEvidence.FinalTurnsDelivered != 1 || afterDelivery.DeliveryEvidence.ReferencedCards != 2 || afterDelivery.DeliveryEvidence.UnresolvedAttempts != 0 {
			t.Fatal("confirmed scoped final/reply evidence missing", afterDelivery.DeliveryEvidence, err)
		}
	})
}

func TestRuntimeActivationReportsQueueAdmissionAndLocalPresence(t *testing.T) {
	r := &Runtime{}
	if r.Activate("thread") || r.ActorStarted("thread") {
		t.Fatal("stopped runtime admitted activation")
	}
	r.running, r.activations = true, make(chan struct{}, 1)
	r.pending = map[string]bool{}
	r.actors = map[string]*threadActor{}
	if !r.Activate("thread") || !r.Activate("other") || r.ActorStarted("thread") || len(r.pending) != 2 {
		t.Fatal("coalesced queue admission masqueraded as actor start or dropped work")
	}
	r.actors["thread"] = &threadActor{thread: "thread"}
	if !r.ActorStarted("thread") {
		t.Fatal("local actor presence not reported")
	}
}
