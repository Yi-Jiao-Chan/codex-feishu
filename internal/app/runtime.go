package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"codex-feishu/internal/bindings"
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
	MinProgressIntervalMS      int64
	Ready                      func() bool
	CanDeliver                 func(context.Context, store.Binding) error
	ActorTick                  func(context.Context, string) error
	HistoryPreparing           func(context.Context, string, []codexclient.Turn) error
	HistoryStaged              func(context.Context, string, []codexclient.Turn) error
	HistoryHydrated            func(context.Context, string, []codexclient.Turn) error
	WaitingReminder            func(context.Context, string) (bool, error)
	AfterDelivery              func(context.Context, string, []codexclient.Turn) error
	EventObserved              func(codexclient.Event)
	EventObservedContext       func(context.Context, codexclient.Event) error
	RenderProgress             func(string, string, string, string) string
	// Notice contains only a fixed diagnostic code, not RPC error text/bodies.
	Notice  func(string)
	Now     func() time.Time
	mu      sync.Mutex
	actors  map[string]*threadActor
	running bool
	checks  chan string
}

type threadActor struct {
	thread string
	wake   chan struct{}
	replay bool
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
// Unknown threads only enter discovery checks; they never acquire a route.
func (r *Runtime) Wake(thread string) {
	if thread == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return
	}
	if a := r.actors[thread]; a != nil {
		select {
		case a.wake <- struct{}{}:
		default:
		}
	} else if r.DiscoveryEnabled {
		select {
		case r.checks <- thread:
		default:
		}
	}
}

// Authorize checks actual current native metadata, including registered Git
// worktrees. Directory mute affects only a future automatic group, not an
// existing binding. Fork following remains deliberately deferred.
func (r *Runtime) Authorize(ctx context.Context, t codexclient.Thread, automatic bool) error {
	if !t.InteractiveMain() || t.ForkedFromID != nil {
		return store.ErrIneligible
	}
	scope, err := rules.ResolveScope(ctx, t.CWD, r.AllowedRoots)
	if err != nil {
		return err
	}
	if !scope.Allowed {
		return store.ErrIneligible
	}
	if automatic && r.DirectoryRules != nil {
		dirs, err := r.DirectoryRules(ctx)
		if err != nil {
			return err
		}
		if _, muted := rules.Muted(scope, dirs); muted {
			return store.ErrIneligible
		}
	}
	return nil
}

// Run requires the caller's exclusive deployment lock. It drains births while
// the baseline snapshot awaits RPCs, starts existing routes without rediscovery,
// and joins all workers on cancellation/disconnect.
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
	r.checks = make(chan string, 256)
	r.running = true
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
		r.mu.Lock()
		r.running = false
		r.actors = nil
		r.mu.Unlock()
	}()
	e := &discovery.Engine{Store: r.Store, Native: r.Native, HostID: r.HostID, AppID: r.AppID, AllowedRoots: r.AllowedRoots, DirectoryRules: r.DirectoryRules, Now: r.Now}
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
	bounds, err := r.Store.Bindings(ctx, r.HostID)
	if err != nil {
		return err
	}
	startActor := func(id string, replay bool) {
		r.mu.Lock()
		if r.actors[id] != nil {
			r.mu.Unlock()
			r.Wake(id)
			return
		}
		a := &threadActor{thread: id, wake: make(chan struct{}, 1), replay: replay}
		r.actors[id] = a
		r.mu.Unlock()
		workers.Add(1)
		go func() { defer workers.Done(); r.runActor(ctx, a) }()
	}
	for _, b := range bounds {
		startActor(b.ThreadID, true)
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
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-scanned:
			pending, err := r.Store.PendingObservations(ctx, r.HostID)
			if err != nil {
				return err
			}
			for _, o := range pending {
				if o.State == "eligible" {
					startActor(o.ThreadID, old[o.ThreadID])
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
	fullScan := func() {
		if err := e.Snapshot(ctx); err != nil {
			r.notice("discovery.baseline_pending")
		} else {
			if err := e.Scan(ctx); err != nil {
				r.notice("discovery.scan_pending")
			}
			notify()
		}
	}
	fullScan()
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
			fullScan()
		}
	}
}

func (r *Runtime) runActor(ctx context.Context, a *threadActor) {
	interval := r.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	engine := &messages.Engine{Store: r.Store, Native: r.Native, HostID: r.HostID}
	engine.ProgressRender = r.RenderProgress
	outbound := messages.Outbound{Store: r.Store, Gateway: r.Messages, Mentions: &r.Mentions, Membership: r.Membership, MinProgressIntervalMS: r.MinProgressIntervalMS}
	members := []string{r.OwnerID}
	seenMembers := map[string]bool{r.OwnerID: true}
	for _, id := range r.Members {
		if id != "" && !seenMembers[id] {
			members = append(members, id)
			seenMembers[id] = true
		}
	}
	manager := &bindings.Manager{Store: r.Store, Native: r.Native, Groups: r.Groups, HostID: r.HostID, OwnerID: r.OwnerID, Members: members, Authorize: func(ctx context.Context, t codexclient.Thread) error { return r.Authorize(ctx, t, true) }}
	groupDone := make(chan error, 1)
	var groupWG sync.WaitGroup
	defer groupWG.Wait()
	groupRunning, groupBlocked, attached, hydrated := false, false, false, false
	dirty := true
	lastRead := time.Time{}
	title := "Codex"
actorLoop:
	for {
		if ctx.Err() != nil {
			return
		}
		if dirty || r.now().Sub(lastRead) >= 15*time.Second {
			dirty = false
			lastRead = r.now()
			if !attached {
				attachCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				t, err := r.Native.Attach(attachCtx, a.thread)
				if err == nil && t.ID == a.thread {
					err = r.Authorize(attachCtx, t, false)
				} else if err == nil {
					err = store.ErrConflict
				}
				if err != nil {
					r.notice("thread.attach_pending")
				} else {
					attached = true
					if t.Name != nil && *t.Name != "" {
						runes := []rune(*t.Name)
						if len(runes) > 40 {
							runes = runes[:40]
						}
						title = string(runes)
					}
				}
				cancel()
			}
			if attached {
				readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				meta, err := r.Native.Metadata(readCtx, a.thread)
				if err == nil && meta.ID == a.thread {
					err = r.Authorize(readCtx, meta, false)
				} else if err == nil {
					err = store.ErrConflict
				}
				if err != nil {
					cancel()
					r.notice("thread.authorization_pending")
					continue actorLoop
				}
				if r.ActorTick != nil {
					if _, err := r.Store.Binding(readCtx, r.HostID, a.thread); err == nil {
						if err := r.ActorTick(readCtx, a.thread); err != nil {
							r.notice("input.reconcile_pending")
						}
					}
				}
				// Read and persist first, regardless of group network latency. A
				// failed or unknown creation never discards the first/final items.
				turns, err := r.Native.Turns(readCtx, a.thread)
				if err == nil {
					if title == "Codex" {
						for _, turn := range turns {
							for _, item := range turn.Items {
								if item.Type == "userMessage" && strings.TrimSpace(item.PublicText()) != "" {
									title = shortGroupText(item.PublicText(), 40)
									break
								}
							}
							if title != "Codex" {
								break
							}
						}
					}
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
				}
				b, bindingErr := r.Store.Binding(ctx, r.HostID, a.thread)
				if bindingErr == nil {
					if r.ready() && (r.CanDeliver == nil || r.CanDeliver(ctx, b) == nil) {
						// Bounded batch prevents one long answer from monopolizing
						// shutdown; every segment has its own pre-RPC journal.
						for i := 0; i < 16 && ctx.Err() == nil; i++ {
							result, err := outbound.DispatchOne(ctx, r.HostID, a.thread, r.now().UnixMilli())
							if err != nil {
								r.notice("delivery.commit_pending")
								break
							}
							if result.AttemptID == "" {
								break
							}
							if result.State != "confirmed" {
								r.notice("delivery.verification_required")
								break
							}
						}
						if r.AfterDelivery != nil {
							if err := r.AfterDelivery(ctx, a.thread, turns); err != nil {
								r.notice("feedback.reconcile_pending")
							}
						}
						if r.WaitingReminder != nil && err == nil {
							if _, err := r.WaitingReminder(ctx, a.thread); err != nil {
								r.notice("request.reminder_pending")
							}
						}
					}
				} else if !errors.Is(bindingErr, sql.ErrNoRows) {
					r.notice("binding.read_pending")
				} else if hydrated && r.DiscoveryEnabled && r.ready() && !groupRunning && !groupBlocked {
					groupRunning = true
					groupWG.Add(1)
					// Capture the chosen title: later history/name changes must
					// not race or change the parameters of an in-flight intent.
					go func(taskTitle string) {
						defer groupWG.Done()
						groupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
						defer cancel()
						o, err := r.Store.Observation(groupCtx, r.HostID, a.thread)
						if err == nil {
							_, err = manager.Ensure(groupCtx, a.thread, shortGroupText(filepath.Base(o.ProjectRoot)+" · "+taskTitle, 60))
						}
						select {
						case groupDone <- err:
						case <-ctx.Done():
						}
					}(title)
				}
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				continue actorLoop
			case <-a.wake:
				// Coalesce token and status events until the next actor tick.
				dirty = true
			case err := <-groupDone:
				groupRunning = false
				groupBlocked = errors.Is(err, bindings.ErrGroupUnknown) || errors.Is(err, bindings.ErrGroupRejected)
				dirty = true
				if err != nil {
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
