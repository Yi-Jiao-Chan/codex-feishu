package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"codex-feishu/internal/bindings"
	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/discovery"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

// Runtime owns one connected native generation. Only Run consumes Events;
// per-thread actors receive coalesced wakeups, never secret-bearing raw events.
// The service must wait for Run to return before replacing the connection or
// recovering dispatching journals. Native writes belong to ActorTick and are
// serialized with that thread's reconciliation, not with other threads.
type RuntimeNative interface {
	discovery.Native
	bindings.Native
	Events() <-chan codexclient.Event
	Err() error
}

// VerificationAuthorize must keep recognized markers handled on failure and
// reject their automatic use; it owns the dedicated verification policy.
type Runtime struct {
	Store                      *store.Store
	Native                     RuntimeNative
	Groups                     bindings.Groups
	Messages                   messages.MessageGateway
	Mentions                   store.MentionOptions
	Membership                 messages.MembershipGateway
	HostID, AppID, OwnerID     string
	Members, AllowedRoots      []string
	DiscoveryEnabled           bool
	DirectoryRules             func(context.Context) ([]rules.DirectoryRule, error)
	ScanInterval, PollInterval time.Duration
	PinScanInterval            time.Duration
	FullScanInterval           time.Duration
	IdlePollInterval           time.Duration
	IdleHistoryInterval        time.Duration
	ActorIdleTimeout           time.Duration
	ActivationSweepInterval    time.Duration
	ActivationRetryInitial     time.Duration
	ActivationRetryMax         time.Duration
	initialThreads             []string
	MinProgressIntervalMS      int64
	SuppressAsyncQuestions     bool
	AsyncQuestionsSinceMS      int64
	ProgressTimeZone           string
	PrepareHuman               func(context.Context, string, []codexclient.AttachmentReference) ([]cards.AttachmentView, error)
	PreparePublic              func(context.Context, string, string) (string, error)
	ReadGoalTurns              func(context.Context, string, []string) (map[string]bool, error)
	SyncGroupName              func(context.Context, store.Binding, string) error
	Ready                      func() bool
	CanDeliver                 func(context.Context, store.Binding) error
	BeginDelivery              func(context.Context, store.Binding) (context.Context, func(), error)
	VerificationAuthorize      func(context.Context, codexclient.Thread, bool) (handled bool, err error)
	CreationAuthorize          func(context.Context, codexclient.Thread, bool) (handled bool, err error)
	CanAttach                  func(context.Context, string) error
	PrepareWorkspace           func(context.Context, string) error
	RememberWorkspace          func(context.Context, codexclient.Thread) error
	WorkspaceAuthorize         func(context.Context, codexclient.Thread) error
	ActorTick                  func(context.Context, string) error
	HistoryPreparing           func(context.Context, string, []codexclient.Turn) error
	HistoryStaged              func(context.Context, string, []codexclient.Turn) error
	HistoryHydrated            func(context.Context, string, []codexclient.Turn) error
	WaitingReminder            func(context.Context, string) (bool, error)
	AfterDelivery              func(context.Context, string, []codexclient.Turn) error
	EventObserved              func(codexclient.Event)
	EventObservedContext       func(context.Context, codexclient.Event) error
	// ActorRetrying and ActorReleased run under the lifecycle lock and must not
	// reenter Runtime.
	ActorRetrying  func(string)
	ActorReleased  func(string)
	RenderProgress func(string, string, string, cards.ProgressView) string
	NativeCOT      bool
	// Notice contains only a fixed diagnostic code, not RPC error text/bodies.
	Notice         func(string)
	Now            func() time.Time
	mu             sync.Mutex
	actors         map[string]*threadActor
	retries        map[string]actorRetry
	pending        map[string]bool
	activating     map[string]bool
	bindings       map[string]bool
	carryover      []string
	running        bool
	lifecycleReady bool
	checks         chan string
	activations    chan struct{}
	pinSupported   bool
	pinKnown       bool
	pinSectionID   string
}

type threadActor struct {
	thread    string
	wake      chan struct{}
	replay    bool
	boundOnly bool
}

type actorExit uint8

const (
	actorStopped actorExit = iota
	actorDormant
	actorRetryLater
)

type actorRetry struct {
	due       time.Time
	delay     time.Duration
	authorize bool
	boundOnly bool
}

func (r *Runtime) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
func (r *Runtime) notice(code string) {
	if r.Notice != nil {
		r.Notice(code)
	}
}
func (r *Runtime) ready() bool { return r.Ready == nil || r.Ready() }

// Wake may be called by a Feishu receiver after its durable receipt commit.
// A dormant route is activated on demand; an unknown thread still has to pass
// discovery before it can acquire a route.
func (r *Runtime) Wake(thread string) {
	if thread == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running || !r.lifecycleReady {
		return
	}
	if a := r.actors[thread]; a != nil {
		select {
		case a.wake <- struct{}{}:
		default:
		}
	} else if r.bindings[thread] {
		delete(r.retries, thread) // Explicit activity bypasses background backoff.
		r.queueActivationLocked(thread, false)
	} else if r.DiscoveryEnabled {
		select {
		case r.checks <- thread:
		default:
		}
	}
}

// Activate queues an explicit existing route for this native generation. Only
// Run may add actors, so activation cannot race the worker join on shutdown.
func (r *Runtime) Activate(thread string) bool {
	if thread == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return false
	}
	r.queueActivationLocked(thread, true)
	return true
}

// ActorStarted reports local actor presence, not current permission or delivery.
func (r *Runtime) ActorStarted(thread string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running && r.actors[thread] != nil
}

func (r *Runtime) bindingLifecycle() (active, retrying map[string]bool, managed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	active, retrying = map[string]bool{}, map[string]bool{}
	if !r.running || !r.lifecycleReady {
		return active, retrying, false
	}
	for id := range r.actors {
		active[id] = true
	}
	markPending := func(id string) {
		if !active[id] {
			retrying[id] = true
		}
	}
	for id := range r.retries {
		markPending(id)
	}
	for id := range r.pending {
		markPending(id)
	}
	for id := range r.activating {
		markPending(id)
	}
	return active, retrying, true
}

// Authorize checks actual current native metadata, including registered Git
// worktrees. Directory mute affects only a future automatic group, not an
// existing binding. Fork following remains deliberately deferred.
func (r *Runtime) Authorize(ctx context.Context, t codexclient.Thread, automatic bool) error {
	_, err := r.authorize(ctx, t, automatic)
	return err
}

func (r *Runtime) authorize(ctx context.Context, t codexclient.Thread, automatic bool) (bool, error) {
	if r.WorkspaceAuthorize != nil {
		if err := r.WorkspaceAuthorize(ctx, t); err != nil {
			return false, err
		}
	}
	if r.CreationAuthorize != nil {
		if handled, err := r.CreationAuthorize(ctx, t, automatic); handled || err != nil {
			return handled, err
		}
	}
	if r.VerificationAuthorize != nil {
		if handled, err := r.VerificationAuthorize(ctx, t, automatic); handled || err != nil {
			return handled, err
		}
	}
	if !t.InteractiveMain() || t.ForkedFromID != nil {
		return false, store.ErrIneligible
	}
	scope, err := rules.ResolveScope(ctx, t.CWD, r.AllowedRoots)
	if err != nil {
		return false, err
	}
	if !scope.Allowed {
		return false, store.ErrIneligible
	}
	if automatic && r.DirectoryRules != nil {
		dirs, err := r.DirectoryRules(ctx)
		if err != nil {
			return false, err
		}
		if _, muted := rules.Muted(scope, dirs); muted {
			return false, store.ErrIneligible
		}
	}
	return false, nil
}

// Run requires the caller's exclusive deployment lock. It drains births while
// the baseline snapshot awaits RPCs, activates only routes with work, and joins
// all workers on cancellation/disconnect.
func (r *Runtime) Run(parent context.Context) error {
	if r.Store == nil || r.Native == nil || r.Messages == nil || r.HostID == "" || r.AppID == "" || r.DiscoveryEnabled && (r.Groups == nil || r.OwnerID == "") {
		return errors.New("runtime dependencies are incomplete")
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("native generation is already running")
	}
	r.actors = map[string]*threadActor{}
	r.retries = map[string]actorRetry{}
	r.pending = map[string]bool{}
	r.activating = map[string]bool{}
	r.bindings = map[string]bool{}
	r.carryover = nil
	r.checks = make(chan string, 256)
	r.activations = make(chan struct{}, 1)
	r.running = true
	r.lifecycleReady = false
	r.pinSupported, r.pinKnown, r.pinSectionID = false, false, ""
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	defer func() {
		r.captureCarryover()
		cancel()
		workers.Wait()
		r.mu.Lock()
		r.running = false
		r.lifecycleReady = false
		r.actors = nil
		r.retries = nil
		r.pending = nil
		r.activating = nil
		r.bindings = nil
		r.checks = nil
		r.activations = nil
		r.mu.Unlock()
	}()
	e := &discovery.Engine{Store: r.Store, Native: r.Native, HostID: r.HostID, AppID: r.AppID, AllowedRoots: r.AllowedRoots, DirectoryRules: r.DirectoryRules, BoundObserved: r.observeBoundActivity, Now: r.Now}
	if _, err := e.Begin(ctx); err != nil {
		return err
	}
	if err := r.Store.RecoverGroupDispatches(ctx, r.HostID); err != nil {
		return err
	}
	if err := r.Store.RecoverMirrorDispatches(ctx, r.HostID); err != nil {
		return err
	}
	if r.WaitingReminder != nil {
		if err := r.Store.RecoverRequestReminders(ctx, r.HostID); err != nil {
			return err
		}
	}
	bindings, err := r.Store.Bindings(ctx, r.HostID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	for _, binding := range bindings {
		r.bindings[binding.ThreadID] = true
	}
	r.mu.Unlock()
	pinNative, previousPins, pinsSupported := r.preparePinning(ctx)
	startActor := func(id string, replay, boundOnly, force bool) bool {
		r.mu.Lock()
		if a := r.actors[id]; a != nil {
			if force {
				select {
				case a.wake <- struct{}{}:
				default:
				}
			}
			r.mu.Unlock()
			return false
		}
		if retry, ok := r.retries[id]; ok && !force && retry.due.After(r.now()) {
			r.mu.Unlock()
			return false
		}
		if force {
			delete(r.retries, id)
		}
		if boundOnly {
			r.bindings[id] = true
		}
		a := &threadActor{thread: id, wake: make(chan struct{}, 1), replay: replay, boundOnly: boundOnly}
		r.actors[id] = a
		r.mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				reason := r.runActor(ctx, a)
				r.mu.Lock()
				if ctx.Err() == nil && reason != actorStopped {
					select {
					case <-a.wake:
						delete(r.retries, id)
						r.mu.Unlock()
						continue
					default:
					}
				}
				if r.actors[id] == a {
					delete(r.actors, id)
					if reason == actorRetryLater && r.ActorRetrying != nil {
						r.ActorRetrying(id)
					}
					if r.ActorReleased != nil {
						r.ActorReleased(id)
					}
				}
				if ctx.Err() == nil && reason == actorRetryLater {
					r.scheduleRetryLocked(id, false, a.boundOnly)
				}
				r.mu.Unlock()
				return
			}
		}()
		return true
	}
	recovery, err := r.Store.BindingActivationThreads(ctx, r.HostID, r.AppID)
	if err != nil {
		return err
	}
	initialThreads := make(map[string]bool, len(recovery)+len(r.initialThreads))
	for _, id := range recovery {
		initialThreads[id] = true
	}
	for _, id := range r.initialThreads {
		if id != "" {
			initialThreads[id] = true
		}
	}
	for id := range initialThreads {
		startActor(id, true, true, true)
	}
	r.mu.Lock()
	r.lifecycleReady = true
	r.mu.Unlock()
	if pinsSupported {
		workers.Add(1)
		go func() { defer workers.Done(); r.runPins(ctx, pinNative, previousPins) }()
	}
	// Persisted eligible candidates may have accumulated content while offline.
	initial, err := r.Store.PendingObservations(ctx, r.HostID)
	if err != nil {
		return err
	}
	old := map[string]bool{}
	for _, o := range initial {
		old[o.ThreadID] = true
	}
	scanned := make(chan struct{}, 1)
	if r.DiscoveryEnabled {
		workers.Add(1)
		go func() { defer workers.Done(); r.runDiscovery(ctx, e, scanned) }()
	} else {
		workers.Add(1)
		go func() { defer workers.Done(); r.runBoundObservation(ctx, e) }()
	}
	activate := func(id string, force bool) {
		activationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := r.Store.Binding(activationCtx, r.HostID, id)
		if errors.Is(err, sql.ErrNoRows) {
			cancel()
			if r.DiscoveryEnabled {
				select {
				case r.checks <- id:
				default:
				}
			}
			r.notice("thread.activation_pending")
			return
		}
		if err == nil {
			var meta codexclient.Thread
			meta, err = r.Native.Metadata(activationCtx, id)
			if err == nil && meta.ID == id {
				err = r.activationAuthorize(activationCtx, meta)
			} else if err == nil {
				err = store.ErrConflict
			}
		}
		cancel()
		if err != nil {
			r.mu.Lock()
			if r.ActorRetrying != nil {
				r.ActorRetrying(id)
			}
			r.scheduleRetryLocked(id, true, true)
			r.mu.Unlock()
			r.notice("thread.activation_pending")
		} else if ctx.Err() == nil {
			r.mu.Lock()
			r.bindings[id] = true
			r.mu.Unlock()
			startActor(id, true, true, force)
		}
	}
	// One scheduler owns durable-work sweeps and exponential activation retry;
	// dormant bindings do not retain a goroutine or timer of their own.
	activationTimer := time.NewTicker(r.activationTimerInterval())
	defer activationTimer.Stop()
	nextSweep := r.now().Add(r.activationSweepInterval())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-activationTimer.C:
			now := r.now()
			for _, request := range r.dueRetries(now) {
				if request.authorize {
					activate(request.thread, false)
				} else {
					startActor(request.thread, true, request.boundOnly, false)
				}
			}
			if !now.Before(nextSweep) {
				ids, err := r.Store.BindingActivationThreads(ctx, r.HostID, r.AppID)
				if err != nil {
					return err
				}
				for _, id := range ids {
					startActor(id, true, true, false)
				}
				nextSweep = now.Add(r.activationSweepInterval())
			}
		case <-r.activations:
			for _, request := range r.takeActivations(32) {
				if request.authorize {
					activate(request.thread, true)
				} else {
					startActor(request.thread, true, true, true)
				}
				r.finishActivation(request.thread)
			}
		case <-scanned:
			pending, err := r.Store.PendingObservations(ctx, r.HostID)
			if err != nil {
				return err
			}
			for _, o := range pending {
				if o.State == "eligible" {
					startActor(o.ThreadID, old[o.ThreadID], false, false)
				}
			}
		case event, open := <-r.Native.Events():
			if !open {
				if parent.Err() != nil {
					return parent.Err()
				}
				return errors.New("native event generation ended; reconnect and reconcile required")
			}
			if r.EventObserved != nil {
				r.EventObserved(event)
			}
			if r.EventObservedContext != nil {
				if err := r.EventObservedContext(ctx, event); err != nil {
					// Stop this generation rather than lose a resolved request or
					// give a failed first observation a later event timestamp.
					return errors.New("native request journal requires reconciliation")
				}
			}
			if event.Method == "thread/started" && r.DiscoveryEnabled {
				var params struct {
					Thread codexclient.Thread `json:"thread"`
				}
				if json.Unmarshal(event.Params, &params) != nil || params.Thread.ID == "" {
					r.notice("native.birth_invalid")
					continue
				}
				if err := e.ObserveBirth(ctx, params.Thread); err != nil {
					return err
				}
				r.Wake(params.Thread.ID)
			} else {
				// Server requests are never answered by this subscription. The
				// App retains approval/question ownership and native policies.
				var params struct {
					ThreadID string `json:"threadId"`
				}
				if json.Unmarshal(event.Params, &params) == nil {
					r.Wake(params.ThreadID)
				}
			}
		}
	}
}

func (r *Runtime) runDiscovery(ctx context.Context, e *discovery.Engine, done chan<- struct{}) {
	interval := r.ScanInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	notify := func() {
		select {
		case done <- struct{}{}:
		default:
		}
	}
	scan := func() {
		if err := e.Snapshot(ctx); err != nil {
			r.notice("discovery.baseline_pending")
		} else {
			if err := r.scanDiscovery(ctx, e); err != nil {
				r.notice("discovery.scan_pending")
			}
			notify()
		}
	}
	scan()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-r.checks:
			fence, complete, err := r.Store.BaselineFence(ctx, r.HostID)
			if err == nil && complete {
				if err := e.Check(ctx, id, fence); err != nil {
					r.notice("discovery.candidate_pending")
				}
				notify()
			}
		case <-timer.C:
			scan()
		}
	}
}

func (r *Runtime) runBoundObservation(ctx context.Context, e *discovery.Engine) {
	interval := r.ScanInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	if err := e.ScanBindingsSince(ctx, 0); err != nil {
		r.notice("binding.scan_pending")
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := e.ScanBindingsSince(ctx, r.now().Add(-recentActivityWindow).Unix()); err != nil {
				r.notice("binding.scan_pending")
			}
		}
	}
}

func (r *Runtime) runActor(ctx context.Context, a *threadActor) actorExit {
	interval := r.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	idlePoll := r.IdlePollInterval
	if idlePoll <= 0 {
		idlePoll = 15 * time.Second
	}
	if idlePoll < interval {
		idlePoll = interval
	}
	idleHistory := r.IdleHistoryInterval
	if idleHistory <= 0 {
		idleHistory = 5 * time.Minute
	}
	cold, tickInterval := false, interval
	setCold := func(value bool) {
		cold = value
		next := interval
		if cold {
			next = idlePoll
		}
		if next != tickInterval {
			timer.Reset(next)
			tickInterval = next
		}
	}
	engine := &messages.Engine{Store: r.Store, Native: r.Native, HostID: r.HostID, TimeZone: r.ProgressTimeZone}
	engine.PrepareHuman = r.PrepareHuman
	engine.PreparePublic = r.PreparePublic
	engine.ReadGoalTurns = r.ReadGoalTurns
	engine.ProgressRender = r.RenderProgress
	engine.NativeCOT = r.NativeCOT
	engine.SuppressAsyncQuestions = r.SuppressAsyncQuestions
	engine.AsyncQuestionsSinceMS = r.AsyncQuestionsSinceMS
	if r.Mentions.Enabled {
		engine.MentionTargets = r.Mentions.Targets
	}
	outbound := messages.Outbound{Store: r.Store, Gateway: r.Messages, Mentions: &r.Mentions, Membership: r.Membership, MinProgressIntervalMS: r.MinProgressIntervalMS, Now: r.now, BeginWrite: r.BeginDelivery}
	if reader, ok := r.Messages.(messages.MessageReadbackGateway); ok {
		outbound.Readback = reader
	}
	deliveryPending := false
	deliver := func(b store.Binding, turns []codexclient.Turn, historyOK bool) {
		deliveryPending = false
		if r.ready() && (r.CanDeliver == nil || r.CanDeliver(ctx, b) == nil) {
			// Bounded batch prevents one long answer from monopolizing
			// shutdown; every segment has its own pre-RPC journal.
			for i := 0; i < 16 && ctx.Err() == nil; i++ {
				result, err := outbound.DispatchOne(ctx, r.HostID, a.thread, r.now().UnixMilli())
				if err != nil {
					deliveryPending = true
					r.notice("delivery.commit_pending")
					break
				}
				if result.AttemptID == "" {
					break
				}
				if result.State == "deferred" {
					deliveryPending = true
					r.notice("delivery.rate_limited")
					continue // Independent final or queue cards may still deliver.
				}
				if result.State != "confirmed" {
					deliveryPending = true
					r.notice("delivery.verification_required")
					break
				}
			}
			if r.AfterDelivery != nil {
				if err := r.AfterDelivery(ctx, a.thread, turns); err != nil {
					deliveryPending = true
					r.notice("feedback.reconcile_pending")
				}
			}
			if r.WaitingReminder != nil && historyOK {
				if _, err := r.WaitingReminder(ctx, a.thread); err != nil {
					deliveryPending = true
					r.notice("request.reminder_pending")
				}
			}
		} else {
			deliveryPending = true
		}
	}
	members := []string{r.OwnerID}
	seenMembers := map[string]bool{r.OwnerID: true}
	for _, id := range r.Members {
		if id != "" && !seenMembers[id] {
			members = append(members, id)
			seenMembers[id] = true
		}
	}
	manager := &bindings.Manager{Store: r.Store, Native: r.Native, Groups: r.Groups, HostID: r.HostID, OwnerID: r.OwnerID, Members: members, Now: r.now, Authorize: func(ctx context.Context, t codexclient.Thread) error { return r.Authorize(ctx, t, true) }}
	groupDone := make(chan error, 1)
	var groupWG sync.WaitGroup
	defer groupWG.Wait()
	groupRunning, groupBlocked, attached, hydrated := false, false, false, false
	var groupRetryAtMS int64
	dirty := true
	lastRead := time.Time{}
	dormantAt := time.Time{}
	deliveryAuthorized := false
	var latestTurns []codexclient.Turn
	latestHistoryOK := false
	title := "Codex"
	nameStateKnown := false
	syncedTitle := ""
	var nextNameSyncAtMS int64
	syncName := func(b store.Binding) {
		if !nameStateKnown || b.FollowMode == "topic" || r.SyncGroupName == nil || syncedTitle == title || r.now().UnixMilli() < nextNameSyncAtMS || !r.ready() ||
			(r.CanDeliver != nil && r.CanDeliver(ctx, b) != nil) {
			return
		}
		nameCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		err := r.SyncGroupName(nameCtx, b, title)
		cancel()
		if err == nil {
			syncedTitle, nextNameSyncAtMS = title, 0
			return
		}
		syncedTitle = "" // A pending rename may have changed the previous name.
		nextNameSyncAtMS = r.now().Add(15 * time.Second).UnixMilli()
		var deferred *bindings.GroupNameDeferredError
		if errors.As(err, &deferred) && deferred.RetryAtMS > nextNameSyncAtMS {
			nextNameSyncAtMS = deferred.RetryAtMS
		}
		r.notice("group.name_sync_pending")
	}
actorLoop:
	for {
		if ctx.Err() != nil {
			return actorStopped
		}
		if cold && !dormantAt.IsZero() && !r.now().Before(dormantAt) {
			return actorDormant
		}
		if groupRetryAtMS > 0 && r.now().UnixMilli() >= groupRetryAtMS {
			groupRetryAtMS = 0
			dirty = true
		}
		historyInterval := 15 * time.Second
		if cold {
			historyInterval = idleHistory
		}
		readHistory := dirty || r.now().Sub(lastRead) >= historyInterval
		if readHistory {
			setCold(false)
			dirty = false
			lastRead = r.now()
			if r.PrepareWorkspace != nil {
				prepareCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				err := r.PrepareWorkspace(prepareCtx, a.thread)
				cancel()
				if err != nil {
					deliveryAuthorized = false
					r.notice("worktree.restore_pending")
					return actorRetryLater
				}
			}
			if !attached {
				attachCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				var t codexclient.Thread
				var err error
				if r.CanAttach != nil {
					err = r.CanAttach(attachCtx, a.thread)
				}
				if err == nil {
					t, err = r.Native.Attach(attachCtx, a.thread)
				}
				if err == nil && t.ID == a.thread {
					err = r.Authorize(attachCtx, t, false)
				} else if err == nil {
					err = store.ErrConflict
				}
				if err != nil {
					r.notice("thread.attach_pending")
				} else {
					attached = true
				}
				cancel()
				if err != nil {
					return actorRetryLater
				}
			}
			if attached {
				tickPending := false
				deliveryAuthorized = false
				readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				meta, err := r.Native.Metadata(readCtx, a.thread)
				verificationHandled := false
				if err == nil && meta.ID == a.thread {
					verificationHandled, err = r.authorize(readCtx, meta, false)
				} else if err == nil {
					err = store.ErrConflict
				}
				if err != nil {
					cancel()
					r.notice("thread.authorization_pending")
					return actorRetryLater
				}
				deliveryAuthorized = true
				if r.RememberWorkspace != nil {
					if err := r.RememberWorkspace(readCtx, meta); err != nil {
						r.notice("worktree.identity_pending")
					}
				}
				// Name is the App's user-facing title. Refresh it from every
				// authorized metadata read, including name-update wakeups.
				title, nameStateKnown = r.groupTitle(meta, latestTurns)
				if r.ActorTick != nil {
					if _, err := r.Store.Binding(readCtx, r.HostID, a.thread); err == nil {
						if err := r.ActorTick(readCtx, a.thread); err != nil {
							tickPending = true
							r.notice("input.reconcile_pending")
						}
					}
				}
				// Read and persist first, regardless of group network latency. A
				// failed or unknown creation never discards the first/final items.
				turns, err := r.Native.Turns(readCtx, a.thread)
				if verificationHandled && codexclient.BeforeFirstInput(err) {
					// Native verification drafts can be authorized before their
					// first input; never invent a user item to unlock hydration.
					turns, err = nil, nil
				}
				if err == nil {
					title, nameStateKnown = r.groupTitle(meta, turns)
					if r.HistoryPreparing != nil {
						err = r.HistoryPreparing(readCtx, a.thread, turns)
					}
					if err == nil {
						err = engine.Apply(readCtx, a.thread, turns, a.replay, r.now().UnixMilli())
					}
					if err == nil {
						// Freeze first-observation reminder decisions even before
						// a slow group creation returns. Disabled configuration
						// also gets a marker, preventing later replay reminders.
						err = r.Store.ScheduleMentions(readCtx, r.HostID, a.thread, r.Mentions)
					}
					if err == nil && r.HistoryStaged != nil {
						err = r.HistoryStaged(readCtx, a.thread, turns)
					}
					if err == nil && r.HistoryHydrated != nil {
						err = r.HistoryHydrated(readCtx, a.thread, turns)
					}
				}
				cancel()
				if err != nil {
					r.notice("history.reconcile_pending")
				} else {
					hydrated = true
					a.replay = false
					r.actorHealthy(a.thread)
				}
				latestTurns, latestHistoryOK = turns, err == nil
				b, bindingErr := r.Store.Binding(ctx, r.HostID, a.thread)
				if bindingErr == nil {
					r.mu.Lock()
					if r.bindings != nil {
						r.bindings[a.thread] = true
					}
					r.mu.Unlock()
					deliver(b, turns, err == nil)
					syncName(b)
					mayIdle := err == nil && !tickPending && !deliveryPending && !groupRunning && groupRetryAtMS == 0 &&
						settledThread(meta, turns) && (!nameStateKnown || b.FollowMode == "topic" || r.SyncGroupName == nil || syncedTitle == title)
					if mayIdle {
						// An unknown native write can return nil while awaiting
						// readback. It still needs the normal controller cadence.
						blocked, pendingErr := r.Store.SubmissionBlocked(ctx, store.SubmissionScope{HostID: r.HostID, AppID: r.AppID, ThreadID: a.thread})
						mayIdle = pendingErr == nil && !blocked
						if pendingErr != nil {
							r.notice("input.reconcile_pending")
						}
					}
					if mayIdle {
						if dormantAt.IsZero() {
							dormantAt = r.now().Add(r.actorIdleTimeout())
						}
					} else {
						dormantAt = time.Time{}
					}
					setCold(mayIdle)
				} else if !errors.Is(bindingErr, sql.ErrNoRows) {
					r.notice("binding.read_pending")
					return actorRetryLater
				} else if hydrated && !a.boundOnly && !verificationHandled && r.DiscoveryEnabled && r.ready() && !groupRunning && !groupBlocked && r.now().UnixMilli() >= groupRetryAtMS {
					groupRunning = true
					groupWG.Add(1)
					// Capture the chosen title: later history/name changes must
					// not race or change the parameters of an in-flight intent.
					go func(taskTitle string) {
						defer groupWG.Done()
						groupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
						defer cancel()
						_, err := manager.Ensure(groupCtx, a.thread, taskTitle)
						select {
						case groupDone <- err:
						case <-ctx.Done():
						}
					}(title)
				}
				if err != nil {
					return actorRetryLater
				}
			}
		}
		if !readHistory && deliveryAuthorized && r.ready() {
			// Pending cards have their own local deadline. Retrying delivery
			// must not re-read native history on every actor tick.
			due, err := r.Store.HasDueMirrors(ctx, r.HostID, a.thread, r.now().UnixMilli(), r.MinProgressIntervalMS)
			if err != nil {
				r.notice("delivery.selection_pending")
			} else if due || cold {
				// Cold ticks still reconcile unknown readbacks, deferred
				// reactions and reminders. They need no native history read;
				// a nil/empty dispatch does not prove durable work is complete.
				if b, err := r.Store.Binding(ctx, r.HostID, a.thread); err == nil {
					deliver(b, latestTurns, latestHistoryOK)
					if deliveryPending {
						setCold(false)
					}
				} else if !errors.Is(err, sql.ErrNoRows) {
					r.notice("binding.read_pending")
				}
			}
		}
		for {
			select {
			case <-ctx.Done():
				return actorStopped
			case <-timer.C:
				continue actorLoop
			case <-a.wake:
				// Coalesce token and status events until the next actor tick.
				dirty = true
				dormantAt = time.Time{}
				if cold {
					// A new native/Feishu event bypasses the cold timer and
					// reauthorizes metadata before any thread work resumes.
					setCold(false)
					continue actorLoop
				}
			case err := <-groupDone:
				groupRunning = false
				groupBlocked = errors.Is(err, bindings.ErrGroupUnknown) || errors.Is(err, bindings.ErrGroupRejected)
				// Error completion must not immediately rehydrate/spawn another
				// creator. A durable limit supplies its own due wakeup; unrelated
				// transient failures use the existing periodic/history retry.
				dirty = dirty || err == nil // Preserve a real event already waiting to hydrate.
				var deferred *bindings.GroupDeferredError
				if errors.As(err, &deferred) {
					groupRetryAtMS = deferred.RetryAtMS
					r.notice("group.rate_wait")
				} else if err != nil {
					r.notice("group.verification_required")
				}
				continue actorLoop
			}
		}
	}
}

func shortGroupText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > limit {
		text = string(runes[:limit-1]) + "…"
	}
	return text
}
