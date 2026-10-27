package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-feishu/internal/attachments"
	"codex-feishu/internal/bindings"
	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/creation"
	"codex-feishu/internal/interactions"
	"codex-feishu/internal/pairing"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
	messages "codex-feishu/internal/sync"
	"codex-feishu/internal/verification"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type ServiceNative interface {
	RuntimeNative
	submissions.Native
	Close()
}

type ServiceGateway interface {
	bindings.Groups
	messages.MessageGateway
	messages.MembershipGateway
	ReactionGateway
	Ready() bool
	BotInChat(context.Context, string) (bool, error)
	IsOneToOneGroup(context.Context, string, string) (bool, error)
	Listen(context.Context, func(context.Context, *im.P2MessageReceiveV1) error, func(context.Context, *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error)) error
}

// Service owns one Feishu receiver and serial native generations. Its caller
// holds both application and canonical database locks. Connect must discover
// the running App daemon; it must never launch another native server.
type Service struct {
	Config               config.Config
	Store                *store.Store
	Gateway              ServiceGateway
	BotID                string
	Connect              func(context.Context) (ServiceNative, error)
	PreparePublic        func(context.Context, string, string) (string, error)
	Notice               func(string)
	Now                  func() time.Time
	RetryInterval        time.Duration    // Tests may shorten; production starts at 1s.
	Pairing              *pairing.Manager // Ephemeral local management; never persisted as a task.
	Verification         *verification.Manager
	VerificationGroups   *verification.GroupManager
	VerificationThreads  *verification.NativeSessions
	VerificationBindings *verification.BindingManager
	mu                   sync.RWMutex
	interactions         *interactions.Controller
	creation             *creation.Manager
	current              *Runtime
	taskIcons            cards.TaskIcons
	actionsReady         map[string]bool
	nativeConnected      atomic.Bool
	running              atomic.Bool
	startedAtMS          atomic.Int64
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func (s *Service) notice(code string) {
	if s.Notice != nil {
		s.Notice(code)
	}
}
func (s *Service) Wake(thread string) {
	s.mu.RLock()
	runtime := s.current
	s.mu.RUnlock()
	if runtime != nil {
		runtime.Wake(thread)
	}
}
func (s *Service) NativeConnected() bool {
	if !s.nativeConnected.Load() {
		return false
	}
	s.mu.RLock()
	runtime := s.current
	s.mu.RUnlock()
	if runtime == nil {
		return s.nativeConnected.Load()
	}
	// Err is a terminal-event diagnostic, not a general readiness method on
	// RuntimeNative. The production client exposes the reader's Done signal.
	if native, ok := runtime.Native.(interface{ Done() <-chan struct{} }); ok {
		select {
		case <-native.Done():
			return false
		default:
		}
	}
	return s.nativeConnected.Load()
}

// A connected socket alone does not establish fresh per-thread control state.
// Ordinary inputs remain durable while disconnected; buttons need a completed
// history reconciliation in the current native generation.
func (s *Service) actionAvailable(thread string) bool {
	if !s.NativeConnected() {
		return false
	}
	s.mu.RLock()
	ready := s.actionsReady[thread]
	runtime := s.current
	s.mu.RUnlock()
	if !ready && runtime != nil {
		// The callback still fails closed, but its durable route is warmed so a
		// retry can use freshly reconciled native state.
		runtime.Wake(thread)
	}
	return ready
}

func (s *Service) clearActionsReady(thread string) {
	s.mu.Lock()
	delete(s.actionsReady, thread)
	s.mu.Unlock()
}

func (s *Service) verificationMarker(ctx context.Context, thread string) (store.VerificationBinding, bool, error) {
	b, err := s.Store.VerificationBindingForThread(ctx, s.Config.Host.ID, thread)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	if err != nil || b.AppID != s.Config.Feishu.AppID || b.OwnerID != s.Config.Access.OwnerOpenID || b.BotID != s.BotID {
		s.clearActionsReady(thread)
		return b, true, verification.ErrNotReady
	}
	return b, true, nil
}

func (s *Service) beginVerificationOperation(ctx context.Context, thread string) (context.Context, func(), bool, error) {
	_, handled, err := s.verificationMarker(ctx, thread)
	if !handled {
		// A removed verification route must not be repurposed as an ordinary
		// thread and inherit permission to its still-marked chat.
		b, bindingErr := s.Store.Binding(ctx, s.Config.Host.ID, thread)
		if bindingErr == nil {
			_, chatErr := s.Store.VerificationBindingForChat(ctx, s.Config.Host.ID, b.ChatID)
			if chatErr == nil || !errors.Is(chatErr, sql.ErrNoRows) {
				s.clearActionsReady(thread)
				return nil, nil, true, verification.ErrNotReady
			}
		} else if !errors.Is(bindingErr, sql.ErrNoRows) {
			return nil, nil, true, verification.ErrNotReady
		}
		return ctx, func() {}, false, err
	}
	if err == nil && s.verificationManagerMatches() {
		opctx, release, accepted, beginErr := s.VerificationBindings.BeginOperation(ctx, thread)
		if accepted && beginErr == nil && opctx != nil && release != nil && opctx.Err() == nil {
			return opctx, release, true, nil
		}
		if release != nil {
			release()
		}
	}
	s.clearActionsReady(thread)
	return nil, nil, true, verification.ErrNotReady
}

func (s *Service) verificationManagerMatches() bool {
	m := s.VerificationBindings
	if m == nil || m.Groups == nil {
		return false
	}
	g := m.Groups
	return g.Store == s.Store && g.HostID == s.Config.Host.ID && g.AppID == s.Config.Feishu.AppID && g.OwnerID == s.Config.Access.OwnerOpenID && g.BotID == s.BotID
}

func (s *Service) beginBound(ctx context.Context, b store.Binding, user string) (context.Context, func(), error) {
	if b.RepairRequired {
		return nil, nil, store.ErrTargetRepair
	}
	marker, handled, err := s.verificationMarker(ctx, b.ThreadID)
	if err != nil {
		return nil, nil, err
	}
	if handled && (user != marker.OwnerID || b.HostID != marker.HostID || b.ChatID != marker.ChatID || b.IntentID != marker.IntentID) {
		return nil, nil, store.ErrIneligible
	}
	opctx, release, _, err := s.beginVerificationOperation(ctx, b.ThreadID)
	return opctx, release, err
}

func (s *Service) beginThreadDelivery(ctx context.Context, thread string) (context.Context, func(), error) {
	if b, err := s.Store.Binding(ctx, s.Config.Host.ID, thread); err == nil {
		if err = s.verifyFollowTarget(ctx, b); err != nil {
			return nil, nil, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	opctx, release, _, err := s.beginVerificationOperation(ctx, thread)
	return opctx, release, err
}

func (s *Service) beginCardDelivery(ctx context.Context, b store.Binding) (context.Context, func(), error) {
	if b.HostID != s.Config.Host.ID {
		return nil, nil, verification.ErrNotReady
	}
	chatMarker, chatErr := s.Store.VerificationBindingForChat(ctx, s.Config.Host.ID, b.ChatID)
	if chatErr == nil && chatMarker.ThreadID != b.ThreadID || chatErr != nil && !errors.Is(chatErr, sql.ErrNoRows) {
		return nil, nil, verification.ErrNotReady
	}
	marker, handled, err := s.verificationMarker(ctx, b.ThreadID)
	if err != nil || handled && (b.HostID != marker.HostID || b.ChatID != marker.ChatID || b.IntentID != marker.IntentID) {
		return nil, nil, verification.ErrNotReady
	}
	return s.beginThreadDelivery(ctx, b.ThreadID)
}

func (s *Service) beginCommandDelivery(ctx context.Context, r store.CommandReply) (context.Context, func(), error) {
	if r.HostID != s.Config.Host.ID || r.AppID != s.Config.Feishu.AppID {
		return nil, nil, verification.ErrNotReady
	}
	// Commands recorded before a group was bound can have no ThreadID.
	// Look up the private chat marker even if the ordinary route was deleted.
	marker, err := s.Store.VerificationBindingForChat(ctx, r.HostID, r.ChatID)
	if err == nil {
		if r.ChatType != "group" || r.UserID != marker.OwnerID || r.ThreadID != "" && r.ThreadID != marker.ThreadID {
			return nil, nil, verification.ErrNotReady
		}
		return s.beginThreadDelivery(ctx, marker.ThreadID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, verification.ErrNotReady
	}
	if r.ThreadID != "" {
		_, handled, err := s.verificationMarker(ctx, r.ThreadID)
		if handled || err != nil {
			return nil, nil, verification.ErrNotReady
		}
	}
	return ctx, func() {}, ctx.Err()
}

func (s *Service) Run(parent context.Context) error {
	if s.Store == nil || s.Gateway == nil || s.Connect == nil || s.BotID == "" {
		return errors.New("service dependencies are incomplete")
	}
	if err := s.Config.Validate(); err != nil {
		return err
	}
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("service is already running")
	}
	s.startedAtMS.Store(s.now().UnixMilli())
	defer s.running.Store(false)
	s.loadTaskIcons()
	if _, err := s.Store.BeginBaseline(parent, s.Config.Host.ID, s.Config.Feishu.AppID, s.now().UnixMilli()); err != nil {
		return err
	}
	defaults, err := rules.DefaultPolicy(s.Config.Notifications.Timezone)
	if err != nil {
		return err
	}
	if s.Config.Notifications.QuietHours != "" {
		defaults.QuietStart, defaults.QuietEnd, _ = strings.Cut(s.Config.Notifications.QuietHours, "-")
	}
	if err := defaults.Validate(); err != nil {
		return err
	}
	for _, user := range s.Config.Notifications.TargetOpenIDs {
		if _, err := s.Store.InitNotificationPolicyAt(parent, s.Config.Feishu.AppID, user, defaults, s.now()); err != nil {
			return err
		}
	}
	if err := s.Store.RecoverInteractions(parent, s.Config.Host.ID); err != nil {
		return err
	}
	if err := s.Store.RecoverReactionTransitions(parent, s.Config.Host.ID, s.Config.Feishu.AppID); err != nil {
		return err
	}
	if err := s.Store.RecoverCommandDispatches(parent, s.Config.Host.ID); err != nil {
		return err
	}
	if s.Config.Creation.Enabled {
		if err := s.Store.RecoverCreations(parent, s.Config.Host.ID, s.Config.Feishu.AppID); err != nil {
			return err
		}
	}
	// Offline cards may be dispatched before any native generation connects.
	// Recover only pre-existing in-flight mirror writes, while no actor exists.
	if err := s.Store.RecoverMirrorDispatches(parent, s.Config.Host.ID); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait(); s.nativeConnected.Store(false) }()
	workers.Add(1)
	go func() {
		defer workers.Done()
		s.compactDeliveryPayloads(ctx)
	}()
	collectingAttachments, err := s.Store.HasCollectingAttachments(ctx, s.Config.Host.ID, s.Config.Feishu.AppID)
	if err != nil {
		return err
	}
	if s.Config.Attachments.Enabled || collectingAttachments {
		gateway, ok := s.Gateway.(attachments.ResourceGateway)
		if !ok {
			return errors.New("attachment resource gateway unavailable")
		}
		limits := s.Config.AttachmentLimits()
		cache, err := attachments.NewCache(limits.Directory, limits)
		if err != nil {
			return errors.New("private attachment cache unavailable")
		}
		if err := s.Store.RecoverAttachmentDownloads(ctx, s.Config.Host.ID, s.Config.Feishu.AppID); err != nil {
			return err
		}
		worker := &attachments.Worker{Store: s.Store, Cache: cache, Gateway: gateway, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, Wake: s.Wake, Notice: s.Notice, Begin: s.beginThreadDelivery}
		workers.Add(1)
		go func() { defer workers.Done(); worker.Run(ctx) }()
	}
	commands := &Commands{Store: s.Store, Config: s.Config, Wake: s.Wake, Now: s.Now, BeginDelivery: s.beginCommandDelivery}
	receiver := &Receiver{Store: s.Store, Config: s.Config, BotID: s.BotID, Wake: s.Wake, Command: commands.Handle, Notice: s.Notice, Verification: s.Verification, VerificationGroups: s.VerificationGroups}
	receiver.IsOneToOneGroup = s.Gateway.IsOneToOneGroup
	receiver.TaskAction = s.claimTask
	receiver.InteractionAction = s.claimInteraction
	receiver.ModelSettingsAction = commands.ModelSettingsAction
	receiver.ActionAllowed = s.actionAvailable
	receiver.BeginBound = s.beginBound
	if s.Config.Creation.Enabled {
		gateway, ok := s.Gateway.(creation.Gateway)
		if !ok {
			return errors.New("creation gateway capabilities unavailable")
		}
		m := &creation.Manager{Store: s.Store, Config: s.Config, Gateway: gateway, Now: s.Now, Notice: s.Notice}
		m.Native = func() creation.Native {
			s.mu.RLock()
			defer s.mu.RUnlock()
			if s.current == nil {
				return nil
			}
			n, _ := s.current.Native.(creation.Native)
			return n
		}
		m.Activate = func(thread string) {
			s.mu.RLock()
			r := s.current
			s.mu.RUnlock()
			if r != nil {
				r.Activate(thread)
			}
		}
		s.creation = m
		receiver.Create, receiver.CreationAction = m.Receive, m.Action
		workers.Add(1)
		go func() { defer workers.Done(); m.Run(ctx) }()
	}
	errorsCh := make(chan error, 2)
	workers.Add(1)
	go func() {
		defer workers.Done()
		err := s.Gateway.Listen(ctx, func(ctx context.Context, event *im.P2MessageReceiveV1) error {
			if s.Pairing != nil && s.Pairing.Handle(event) {
				return nil
			}
			return receiver.Message(ctx, event)
		}, receiver.Action)
		if ctx.Err() == nil {
			// Keep SDK error payloads out of logs or CLI output.
			if err == nil {
				err = errors.New("Feishu receiver stopped")
			} else {
				err = errors.New("Feishu receiver unavailable")
			}
			errorsCh <- err
		}
	}()
	workers.Add(1)
	go func() { defer workers.Done(); s.nativeLoop(ctx) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.commandLoop(ctx, commands) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.receivedLoop(ctx) }()
	select {
	case <-parent.Done():
		return parent.Err()
	case err := <-errorsCh:
		return err
	}
}

func (s *Service) nativeLoop(ctx context.Context) {
	interval := s.RetryInterval
	if interval <= 0 {
		interval = time.Second
	}
	offlineStaged := false
	var carryover []string
	for ctx.Err() == nil {
		native, err := s.Connect(ctx)
		if err != nil {
			s.notice("codex.connection_pending")
			if !offlineStaged {
				s.disconnected(ctx)
				offlineStaged = true
			}
		} else {
			offlineStaged = false
			runtime := s.generation(native)
			runtime.initialThreads = append([]string(nil), carryover...)
			s.mu.Lock()
			s.current = runtime
			s.mu.Unlock()
			s.nativeConnected.Store(true)
			s.notice("codex.connected")
			err = runtime.Run(ctx) // Run cancels and joins its actors before returning.
			carryover = runtime.carryoverThreads()
			s.nativeConnected.Store(false)
			s.mu.Lock()
			s.current = nil
			s.interactions = nil
			s.actionsReady = nil
			s.mu.Unlock()
			native.Close()
			if ctx.Err() == nil {
				s.disconnected(ctx)
				offlineStaged = true
				s.notice("codex.reconcile_required")
			}
		}
		if !s.waitForRetry(ctx, interval) {
			return
		}
		if interval < 30*time.Second {
			interval *= 2
			if interval > 30*time.Second {
				interval = 30 * time.Second
			}
		}
	}
}

// Only the native-generation owner dispatches offline mirrors. Runtime has
// already joined every actor; the next generation cannot recover this journal
// while an offline HTTP request is in flight. Feedback fills backoff time but
// cannot move the next native attempt past that backoff deadline.
func (s *Service) waitForRetry(ctx context.Context, interval time.Duration) bool {
	deadline := time.Now().Add(interval)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-tick.C:
			if remaining := time.Until(deadline); remaining > 0 {
				feedbackCtx, cancel := context.WithTimeout(ctx, remaining)
				s.disconnected(feedbackCtx)
				cancel()
			}
		}
	}
}

// This runs only in the native-generation loop, after Runtime joined all
// actors and before the next generation starts journal recovery. Offline
// patches therefore cannot race recovery or another mirror writer.
func (s *Service) disconnected(ctx context.Context) {
	if err := s.Store.RecoverInteractions(ctx, s.Config.Host.ID); err != nil {
		s.notice("interactions.recovery_pending")
	}
	// Optional Feishu feedback must not postpone the next native reconnect
	// indefinitely when many chats or a slow gateway are present.
	ctx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	bindings, err := s.Store.Bindings(ctx, s.Config.Host.ID)
	if err != nil {
		s.notice("offline.storage_pending")
		return
	}
	location, _ := time.LoadLocation(s.Config.Notifications.Timezone)
	if location == nil {
		location = time.UTC
	}
	feedback := &Feedback{Store: s.Store, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID}
	for _, b := range bindings {
		if ctx.Err() != nil {
			return
		}
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := feedback.StageReceived(callCtx, b.ThreadID)
		if err == nil {
			offline := &interactions.Controller{Store: s.Store, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID}
			err = offline.StageCards(callCtx, b.ThreadID, nil)
		}

		if err != nil {
			s.notice("offline.receipt_card_pending")
			cancel()
			continue
		}
		mark, err := s.Store.Watermark(callCtx, s.Config.Host.ID, b.ThreadID)
		status := "Codex 连接暂不可用"
		if err == nil {
			status = "Codex 连接已断开 · 状态截至 " + time.UnixMilli(mark.ObservedAtMS).In(location).Format("01-02 15:04:05")
		}
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			err = s.Store.StageDisconnectedCards(callCtx, store.SubmissionScope{HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, ThreadID: b.ThreadID}, status)
		}
		if err != nil {
			s.notice("offline.card_pending")
			cancel()
			continue
		}
		if s.Gateway.Ready() && s.canDeliver(callCtx, b) == nil {
			outbound := messages.Outbound{Store: s.Store, Gateway: s.Gateway, Membership: s.Gateway, BeginWrite: s.beginCardDelivery, Now: s.now, MinProgressIntervalMS: int64(s.Config.Sync.CardUpdateMinIntervalMS), Mentions: &store.MentionOptions{Enabled: true, Targets: s.Config.Notifications.TargetOpenIDs, MentionOn: s.Config.Notifications.MentionOn}}
			if reader, ok := s.Gateway.(messages.MessageReadbackGateway); ok {
				outbound.Readback = reader
			}
			for i := 0; i < 16 && callCtx.Err() == nil; i++ {
				result, err := outbound.DispatchOne(callCtx, s.Config.Host.ID, b.ThreadID, s.now().UnixMilli())
				if err != nil {
					s.notice("offline.delivery_pending")
					break
				}
				if result.AttemptID == "" || result.State != "confirmed" {
					break
				}
			}
		}
		cancel()
	}
}

func (s *Service) generation(native ServiceNative) *Runtime {
	tracker := &RequestTracker{Store: s.Store, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID,
		BeginWrite: s.beginThreadDelivery,
		Options:    store.MentionOptions{Enabled: true, Targets: s.Config.Notifications.TargetOpenIDs, MentionOn: s.Config.Notifications.MentionOn},
		Gateway:    s.Gateway, Membership: s.Gateway, Now: s.Now, GenerationStartedAtMS: s.now().UnixMilli()}
	var interactive *interactions.Controller
	if s.Config.Interactions.Enabled {
		if n, ok := native.(interactions.Native); ok {
			interactive = &interactions.Controller{Store: s.Store, Native: n, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, StartedAtMS: s.now().UnixMilli(), EnabledTypes: s.Config.Interactions.EnabledTypes, Options: tracker.Options, Now: s.Now}
		}
	}
	if interactive != nil {
		if source, ok := native.(interface {
			Metadata(context.Context, string) (codexclient.Thread, error)
		}); ok {
			results := codexclient.NewQuestionResultReader(source.Metadata)
			interactive.ReadQuestionResult = results.Read
		}
		interactive.BeginWrite = func(ctx context.Context, thread string) (context.Context, func(), error) {
			return s.beginThreadDelivery(ctx, thread)
		}
	}
	s.mu.Lock()
	s.interactions = interactive
	s.mu.Unlock()
	feedback := &Feedback{Store: s.Store, Gateway: s.Gateway, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, Waiting: tracker.Waiting, Now: s.now, BeginWrite: s.beginThreadDelivery}
	settingsNative, _ := native.(sessionSettingsNative)
	settings := SessionSettings{Store: s.Store, Native: settingsNative, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID,
		Operators: s.Config.Access.OperatorOpenIDs, BeginWrite: s.beginThreadDelivery, Now: s.now}
	controllers := map[string]*submissions.Controller{}
	tasks := map[string]*TaskControls{}
	var retryEvidence func(context.Context, string, []string) (map[string]string, error)
	_, retrySupported := native.(submissions.RetryNative)
	versionedRetry, retryVersionKnown := native.(interface{ AppServerVersion() string })
	retrySupported = retrySupported && retryVersionKnown && versionedRetry.AppServerVersion() == codexclient.ManualRetrySchemaVersion
	var actorStateMu sync.Mutex
	s.mu.Lock()
	s.actionsReady = map[string]bool{}
	s.mu.Unlock()
	readQueue := native.Queue
	if metadata, ok := native.(codexclient.QueueStateReader); ok {
		readQueue = metadata.QueueState
	}
	task := func(thread string) *TaskControls {
		actorStateMu.Lock()
		defer actorStateMu.Unlock()
		t := tasks[thread]
		if t == nil {
			t = &TaskControls{Store: s.Store, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, Waiting: tracker.Waiting, InFeishu: interactive != nil, Queue: readQueue, Icons: s.taskIcons, RetryEnabled: retrySupported && retryEvidence != nil}
			tasks[thread] = t
		}
		return t
	}
	r := &Runtime{
		Store: s.Store, Native: native, Groups: s.Gateway, Messages: s.Gateway, Membership: s.Gateway,
		HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, OwnerID: s.Config.Access.OwnerOpenID,
		Members: s.Config.Groups.AdditionalMemberOpenIDs, AllowedRoots: s.Config.Discovery.AllowedRoots,
		DiscoveryEnabled: s.Config.Discovery.Enabled, ScanInterval: time.Duration(s.Config.Discovery.ScanIntervalSeconds) * time.Second,
		PollInterval:          time.Duration(s.Config.Sync.CardUpdateMinIntervalMS) * time.Millisecond,
		MinProgressIntervalMS: int64(s.Config.Sync.CardUpdateMinIntervalMS),
		ProgressTimeZone:      s.Config.Notifications.Timezone,
		PreparePublic:         s.PreparePublic,
		Mentions:              store.MentionOptions{Enabled: true, Targets: s.Config.Notifications.TargetOpenIDs, MentionOn: s.Config.Notifications.MentionOn},
		Ready:                 s.Gateway.Ready, Notice: s.Notice, Now: s.Now, EventObservedContext: tracker.ObserveContext,
		HistoryHydrated: tracker.Hydrated, WaitingReminder: tracker.DispatchOne,
		DirectoryRules: func(ctx context.Context) ([]rules.DirectoryRule, error) {
			return s.Store.DirectoryRules(ctx, s.Config.Host.ID, s.Config.Feishu.AppID)
		},
	}
	if interactive != nil {
		r.EventObservedContext = func(ctx context.Context, event codexclient.Event) error {
			if err := tracker.ObserveContext(ctx, event); err != nil {
				return err
			}
			return interactive.Observe(ctx, event)
		}
		r.WaitingReminder = nil
		r.SuppressAsyncQuestions = true
		r.AsyncQuestionsSinceMS = interactive.StartedAtMS
		r.HistoryHydrated = func(ctx context.Context, thread string, turns []codexclient.Turn) error {
			if err := tracker.Hydrated(ctx, thread, turns); err != nil {
				return err
			}
			opctx, release, _, err := s.beginVerificationOperation(ctx, thread)
			if err != nil {
				return err
			}
			defer release()
			return interactive.StageCards(opctx, thread, turns)
		}
	}
	r.CanDeliver = s.canDeliver
	r.BeginDelivery = s.beginCardDelivery
	r.CreationAuthorize = s.authorizeCreation
	r.CanAttach = s.creationAttachGate
	if workspace, ok := native.(workspaceRestoreNative); ok {
		r.PrepareWorkspace = func(ctx context.Context, thread string) error {
			return s.prepareWorkspace(ctx, workspace, thread)
		}
		r.RememberWorkspace = func(ctx context.Context, thread codexclient.Thread) error {
			return s.rememberWorkspace(ctx, workspace, thread)
		}
		r.WorkspaceAuthorize = s.authorizeRestoredWorkspace
	}
	if source, ok := native.(interface {
		Metadata(context.Context, string) (codexclient.Thread, error)
		CodexHome() string
	}); ok {
		origins := codexclient.NewUnanchoredTurnReader(source.Metadata, source.CodexHome())
		r.ReadUnanchoredTurns = origins.Read
		retryEvidence = origins.RetryTurns
	}
	if gateway, ok := s.Gateway.(bindings.GroupNameGateway); ok {
		names := &bindings.GroupNames{Store: s.Store, Gateway: gateway, AppID: s.Config.Feishu.AppID,
			OwnerID: s.Config.Access.OwnerOpenID, Now: s.Now, BeginWrite: s.beginCardDelivery}
		r.SyncGroupName = names.Reconcile
	}
	media := &messages.HumanMedia{Store: s.Store, AppID: s.Config.Feishu.AppID,
		BeginWrite: func(ctx context.Context, thread string) (context.Context, func(), error) {
			binding, err := s.Store.Binding(ctx, s.Config.Host.ID, thread)
			if err != nil {
				return ctx, func() {}, err
			}
			return s.beginCardDelivery(ctx, binding)
		}}
	if uploader, ok := s.Gateway.(messages.ImageUploader); ok {
		media.Uploader = uploader
	}
	r.PrepareHuman = media.Resolve
	r.VerificationAuthorize = func(ctx context.Context, t codexclient.Thread, automatic bool) (bool, error) {
		_, handled, err := s.verificationMarker(ctx, t.ID)
		if !handled {
			return false, err
		}
		if err == nil && s.verificationManagerMatches() {
			accepted, authorizeErr := s.VerificationBindings.Authorize(ctx, t, automatic)
			if accepted && authorizeErr == nil {
				return true, nil
			}
		}
		s.clearActionsReady(t.ID)
		return true, verification.ErrNotReady
	}
	r.ActorTick = func(ctx context.Context, thread string) ([]codexclient.Turn, error) {
		actorStateMu.Lock()
		c := controllers[thread]
		if c == nil {
			c = &submissions.Controller{Store: s.Store, Native: native, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, ThreadID: thread}
			c.RetryEvidence = retryEvidence
			c.BeginWrite = func(ctx context.Context) (context.Context, func(), error) {
				return s.beginSubmissionWrite(ctx, native, thread)
			}
			controllers[thread] = c
		}
		actorStateMu.Unlock()
		if interactive != nil {
			if err := interactive.Tick(ctx, thread); err != nil {
				return nil, err
			}
		}
		settingsErr := settings.Tick(ctx, thread)
		// An unresolved settings write must not disable stop, queue recovery,
		// or ordinary messages. Only further settings mutations are blocked.
		turns, inputErr := c.TickWithHistory(ctx)
		return turns, errors.Join(settingsErr, inputErr)
	}
	r.ActorReleased = func(thread string) {
		actorStateMu.Lock()
		delete(controllers, thread)
		delete(tasks, thread)
		actorStateMu.Unlock()
	}
	r.ActorRetrying = s.clearActionsReady
	r.HistoryPreparing = func(ctx context.Context, thread string, turns []codexclient.Turn) error {
		opctx, release, _, err := s.beginVerificationOperation(ctx, thread)
		if err != nil {
			return err
		}
		defer release()
		ctx = opctx
		if err := tracker.ReconcileContext(ctx, thread, turns); err != nil {
			return err
		}
		if interactive != nil {
			if err := interactive.Prepare(ctx, thread, turns); err != nil {
				return err
			}
		}
		return task(thread).Prepare(ctx, thread, turns)
	}
	r.RenderProgress = func(thread, turn, status string, progress cards.ProgressView) string {
		return task(thread).Render(thread, turn, status, progress)
	}
	r.RenderFinal = func(thread, turn, status, card string) string {
		return task(thread).RenderFinal(thread, turn, status, card)
	}
	_, r.NativeCOT = s.Gateway.(messages.COTGateway)
	r.HistoryStaged = func(ctx context.Context, thread string, turns []codexclient.Turn) error {
		opctx, release, _, err := s.beginVerificationOperation(ctx, thread)
		if err != nil {
			return err
		}
		defer release()
		ctx = opctx
		if _, err := s.Store.Binding(ctx, s.Config.Host.ID, thread); err != nil {
			s.clearActionsReady(thread)
			return nil
		}
		// The first projection may run before the initiating Feishu input's
		// exact history link or App human mirror is committed. Stage again
		// after Apply, now that the reply relationship is durable.
		if interactive != nil {
			if err := interactive.StageCards(ctx, thread, turns); err != nil {
				s.clearActionsReady(thread)
				return err
			}
		} else {
			offline := &interactions.Controller{Store: s.Store, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID}
			if err := offline.StageCards(ctx, thread, turns); err != nil {
				return err
			}
		}
		if err := feedback.Stage(ctx, thread); err != nil {
			s.clearActionsReady(thread)
			return err
		}
		if err := ctx.Err(); err != nil {
			s.clearActionsReady(thread)
			return err
		}
		s.mu.Lock()
		s.actionsReady[thread] = true
		s.mu.Unlock()
		return nil
	}
	r.AfterDelivery = func(ctx context.Context, thread string, turns []codexclient.Turn) error {
		if err := feedback.Sync(ctx, thread, turns); err != nil {
			return err
		}
		if workspace, ok := native.(workspaceRestoreNative); ok {
			return s.publishRestoredWorkspace(ctx, workspace, thread, turns)
		}
		return nil
	}
	return r
}

func (s *Service) canDeliver(ctx context.Context, b store.Binding) error {
	current, err := s.Store.Binding(ctx, b.HostID, b.ThreadID)
	if err != nil {
		return err
	}
	if current.RepairRequired {
		return store.ErrTargetRepair
	}
	if err := s.verifyFollowTarget(ctx, current); err != nil {
		return err
	}
	marker, handled, err := s.verificationMarker(ctx, b.ThreadID)
	if err != nil {
		return err
	}
	if handled {
		if marker.HostID != b.HostID || marker.ChatID != b.ChatID || marker.IntentID != b.IntentID {
			return verification.ErrNotReady
		}
		_, release, _, err := s.beginVerificationOperation(ctx, b.ThreadID)
		if release != nil {
			release()
		}
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	present, err := s.Gateway.BotInChat(checkCtx, b.ChatID)
	if err != nil || !present {
		return errors.New("bound chat delivery unavailable")
	}
	return nil
}

func (s *Service) commandLoop(ctx context.Context, commands *Commands) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if !s.Gateway.Ready() {
				continue
			}
			for i := 0; i < 16 && ctx.Err() == nil; i++ {
				callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				wrote, err := commands.DispatchOne(callCtx, s.Gateway)
				cancel()
				if err != nil {
					s.notice("command.delivery_pending")
					break
				}
				if !wrote {
					break
				}
			}
		}
	}
}

func (s *Service) receivedLoop(ctx context.Context) {
	feedback := &Feedback{Store: s.Store, Gateway: s.Gateway, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, Now: s.now, BeginWrite: s.beginThreadDelivery}
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if !s.Gateway.Ready() {
				continue
			}
			bindings, err := s.Store.ReceivedFeedbackBindingsAt(ctx, s.Config.Host.ID, s.Config.Feishu.AppID, s.now().UnixMilli())
			if err != nil {
				s.notice("feedback.storage_pending")
				continue
			}
			for _, b := range bindings {
				callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if s.canDeliver(callCtx, b) == nil {
					if err := feedback.Received(callCtx, b.ThreadID); err != nil {
						s.notice("feedback.received_pending")
					}
				}
				cancel()
			}
		}
	}
}

func (s *Service) claimTask(ctx context.Context, chat, message, kind, token string, revision int64) (string, error) {
	// Claim is local DB work. It must not wait for a thread actor holding an
	// RPC or reuse that actor's render cache. A fresh object reads durable state.
	b, err := s.Store.BindingByChat(ctx, s.Config.Host.ID, chat)
	if errors.Is(err, sql.ErrNoRows) {
		b, err = s.Store.BindingByMessage(ctx, s.Config.Host.ID, chat, message)
	}
	if err != nil || !s.actionAvailable(b.ThreadID) {
		return "", store.ErrSubmissionStale
	}
	opctx, release, _, err := s.beginVerificationOperation(ctx, b.ThreadID)
	if err != nil {
		return "", err
	}
	defer release()
	ctx = opctx
	t := TaskControls{Store: s.Store, HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID}
	thread, err := t.Claim(ctx, chat, message, kind, token, revision)
	if err == nil {
		s.Wake(thread)
	}
	return thread, err
}

func (s *Service) claimInteraction(ctx context.Context, scope store.SubmissionScope, user string, operators []string, chat, message, token string, revision int64, decision string, form map[string]any) (string, error) {
	if !s.actionAvailable(scope.ThreadID) {
		return "", store.ErrSubmissionStale
	}
	s.mu.RLock()
	controller := s.interactions
	s.mu.RUnlock()
	if controller == nil {
		return "", store.ErrSubmissionStale
	}
	return controller.Claim(ctx, scope, user, operators, chat, message, token, revision, decision, form)
}
