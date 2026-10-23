package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-feishu/compat"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/creation"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

type workspaceRestoreNative interface {
	Metadata(context.Context, string) (codexclient.Thread, error)
	CodexHome() string
	AppServerVersion() string
}

func (s *Service) workspaceScope(thread string) store.SubmissionScope {
	return store.SubmissionScope{HostID: s.Config.Host.ID, AppID: s.Config.Feishu.AppID, ThreadID: thread}
}

// A receipt can arrive, or the desktop can remove a checkout, after the actor's
// workspace preparation. Recheck immediately before claiming a native write so
// a locally missing cwd does not turn a definitely unsent input into unknown.
func (s *Service) beginSubmissionWrite(ctx context.Context, native ServiceNative, thread string) (context.Context, func(), error) {
	opctx, release, err := s.beginThreadDelivery(ctx, thread)
	if err != nil {
		return opctx, release, err
	}
	workspace, ok := native.(workspaceRestoreNative)
	if !ok {
		return opctx, release, nil
	}
	meta, err := workspace.Metadata(opctx, thread)
	if err != nil || meta.ID != thread {
		return opctx, release, store.ErrSubmissionStale
	}
	if info, err := os.Stat(meta.CWD); err != nil || !info.IsDir() {
		s.clearActionsReady(thread)
		s.Wake(thread)
		return opctx, release, store.ErrSubmissionStale
	}
	return opctx, release, nil
}

// Validate the existing route and native identity without requiring the
// missing checkout. This grants no permission to start a replacement thread.
func (s *Service) workspaceRestoreOrigin(ctx context.Context, t codexclient.Thread) (*store.CreationIntent, error) {
	if _, err := s.Store.SubmissionBinding(ctx, s.workspaceScope(t.ID)); err != nil {
		return nil, err
	}
	b, err := s.Store.Binding(ctx, s.Config.Host.ID, t.ID)
	if err != nil || b.RepairRequired || t.Ephemeral || t.ParentThreadID != nil || t.ForkedFromID != nil {
		return nil, store.ErrIneligible
	}
	if _, handled, err := s.verificationMarker(ctx, t.ID); handled || err != nil {
		return nil, store.ErrIneligible
	}
	i, err := s.Store.BridgeCreationThread(ctx, s.Config.Host.ID, s.Config.Feishu.AppID, t.ID)
	if errors.Is(err, sql.ErrNoRows) {
		if !t.InteractiveMain() || t.BridgeCreated() {
			return nil, store.ErrIneligible
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var source string
	want, sourceErr := codexclient.CreationThreadSource(i.ID)
	if i.State != "confirmed" || i.WorkspaceMode != "worktree" || t.CWD != i.ExecutionDirectory() ||
		!s.Config.Operator(i.CreatorID) || i.OwnerID != s.Config.Access.OwnerOpenID ||
		b.IntentID != i.ID || b.RootMessageID != i.RootMessageID || sourceErr != nil ||
		json.Unmarshal(t.ThreadSource, &source) != nil || source != want ||
		i.ProjectID != "" && (t.ProjectID == nil || *t.ProjectID != i.ProjectID) {
		return nil, store.ErrIneligible
	}
	return &i, nil
}

func (s *Service) workspaceRestorePolicy(ctx context.Context, t codexclient.Thread, identity store.ThreadWorktreeIdentity) (*store.CreationIntent, error) {
	i, err := s.workspaceRestoreOrigin(ctx, t)
	if err != nil || identity.HostID != s.Config.Host.ID || identity.AppID != s.Config.Feishu.AppID || identity.ThreadID != t.ID || identity.RawCWD != t.CWD {
		return nil, store.ErrIneligible
	}
	roots := s.Config.Discovery.AllowedRoots
	if i != nil {
		if identity.Root != i.WorktreePath || identity.SourceRoot != i.GitRoot || identity.CommonDir != i.GitCommonDir {
			return nil, store.ErrConflict
		}
		// Creation originally authorizes the entire repository as well as its
		// selected directory; do not widen that rule during a restore.
		directories := creation.DirectoryService{Roots: s.Config.Creation.AllowedRoots}
		if choice, err := directories.Validate(ctx, i.GitRoot, ""); err != nil || choice.Path != i.GitRoot {
			return nil, store.ErrIneligible
		}
		if choice, err := directories.Validate(ctx, i.Directory, ""); err != nil || choice.Path != i.Directory {
			return nil, store.ErrIneligible
		}
		roots = s.Config.Creation.AllowedRoots
	}
	return i, creation.AuthorizeWorktreeRestoreSource(ctx, identity, roots)
}

func (s *Service) restoreWorkspaceIdentity(ctx context.Context, n workspaceRestoreNative, t codexclient.Thread) (store.ThreadWorktreeIdentity, *store.CreationIntent, error) {
	scope := s.workspaceScope(t.ID)
	identity, err := s.Store.ThreadWorktreeIdentity(ctx, scope)
	if err == nil {
		i, err := s.workspaceRestorePolicy(ctx, t, identity)
		return identity, i, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return identity, nil, err
	}
	i, err := s.workspaceRestoreOrigin(ctx, t)
	if err != nil {
		return identity, nil, err
	}
	if i != nil {
		rel, err := filepath.Rel(i.GitRoot, i.Directory)
		if err != nil {
			return identity, nil, err
		}
		identity = store.ThreadWorktreeIdentity{HostID: scope.HostID, AppID: scope.AppID, ThreadID: scope.ThreadID,
			RawCWD: t.CWD, Root: i.WorktreePath, SourceRoot: i.GitRoot, CommonDir: i.GitCommonDir, RelativeCWD: rel}
	} else {
		observed, err := s.Store.Observation(ctx, scope.HostID, t.ID)
		if err != nil || observed.CWD != t.CWD || observed.ProjectRoot == "" || observed.FirstUserItemID == "" {
			return identity, nil, creation.ErrWorktreeIdentity
		}
		identity, err = creation.LocateWorktreeSnapshotIdentity(ctx, scope, t.CWD, observed.ProjectRoot, n.CodexHome())
		if err != nil {
			return identity, nil, err
		}
	}
	i, err = s.workspaceRestorePolicy(ctx, t, identity)
	if err == nil {
		err = s.Store.RegisterThreadWorktreeIdentity(ctx, identity)
	}
	return identity, i, err
}

// Called only after normal runtime authorization. Local directories and
// unmanaged Git checkouts retain their existing behavior.
func (s *Service) rememberWorkspace(ctx context.Context, n workspaceRestoreNative, t codexclient.Thread) error {
	if !compat.VerifiedWorktreeCodexVersion(n.AppServerVersion()) {
		return nil
	}
	if _, err := s.Store.Binding(ctx, s.Config.Host.ID, t.ID); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if identity, err := s.Store.ThreadWorktreeIdentity(ctx, s.workspaceScope(t.ID)); err == nil {
		if identity.RawCWD != t.CWD {
			return creation.ErrWorktreeIdentity
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	i, err := s.workspaceRestoreOrigin(ctx, t)
	if err != nil {
		return nil // Verification sessions and local creation have no restore lane.
	}
	source := ""
	if i != nil {
		source = i.GitRoot
	} else {
		scope, err := rules.ResolveScope(ctx, t.CWD, s.Config.Discovery.AllowedRoots)
		if err != nil || !scope.Allowed || !scope.Worktree {
			return err
		}
		source = scope.ProjectRoot
	}
	identity, err := creation.InspectWorktreeRestoreIdentity(ctx, s.workspaceScope(t.ID), t.CWD, source, n.CodexHome())
	if errors.Is(err, creation.ErrWorktreeScope) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = s.workspaceRestorePolicy(ctx, t, identity); err != nil {
		return err
	}
	return s.Store.RegisterThreadWorktreeIdentity(ctx, identity)
}

// Actor serialization and generation joining guarantee that dispatching here
// belongs to an interrupted earlier call, never another live restore worker.
func (s *Service) prepareWorkspace(ctx context.Context, n workspaceRestoreNative, thread string) error {
	scope := s.workspaceScope(thread)
	if _, err := s.Store.Binding(ctx, scope.HostID, thread); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	grant, err := s.Store.NextWorktreeRestoreGrant(ctx, scope)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrSubmissionBusy) {
		return nil // Existing native unknowns must enter their original reconciler.
	}
	if err != nil {
		return err
	}
	meta, err := n.Metadata(ctx, thread)
	if err != nil || meta.ID != thread {
		return store.ErrConflict
	}
	a, err := s.Store.WorktreeRestoreAttempt(ctx, scope, grant.ReceiptID)
	fresh := errors.Is(err, sql.ErrNoRows)
	if err != nil && !fresh {
		return err
	}
	if fresh {
		if info, err := os.Stat(meta.CWD); err == nil && info.IsDir() {
			return nil
		}
	}
	s.clearActionsReady(thread)
	if !compat.VerifiedWorktreeCodexVersion(n.AppServerVersion()) {
		s.workspaceRestoreFeedback(ctx, scope, grant.ReceiptID, "unsupported")
		return creation.ErrWorktreeRestoreUnsupported
	}
	identity, bridge, err := s.restoreWorkspaceIdentity(ctx, n, meta)
	if err != nil {
		s.workspaceRestoreFeedback(ctx, scope, grant.ReceiptID, restoreFeedbackCode(err))
		return err
	}
	var plan creation.WorktreeRestorePlan
	var checkpoint creation.WorktreeRestoreCheckpoint
	if fresh {
		input := creation.WorktreeRestoreInput{RestoreID: store.ID(), Identity: identity, CodexHome: n.CodexHome()}
		if bridge != nil {
			input.BridgeMarker = creation.CreationWorktreeRestoreMarker(*bridge)
		}
		plan, err = creation.PlanWorktreeRestore(ctx, input)
		if err != nil {
			s.workspaceRestoreFeedback(ctx, scope, grant.ReceiptID, restoreFeedbackCode(err))
			return err
		}
		raw, _ := json.Marshal(plan)
		a, err = s.Store.PrepareWorktreeRestoreAttempt(ctx, scope, grant.ReceiptID, string(raw), s.now().UnixMilli())
		if err != nil {
			return err
		}
	} else if json.Unmarshal([]byte(a.ParamsJSON), &plan) != nil || json.Unmarshal([]byte(a.ResultJSON), &checkpoint) != nil ||
		plan.Identity != identity || plan.CodexHome != n.CodexHome() {
		return store.ErrConflict
	}
	s.workspaceRestoreFeedback(ctx, scope, grant.ReceiptID, "restoring")
	if a.State == "dispatching" {
		if err := s.Store.RecoverWorktreeRestoreDispatches(ctx, scope, s.now().UnixMilli()); err != nil {
			return err
		}
		a.State = "unknown"
	}
	if a.State == "unknown" {
		checkpoint, err = creation.ReconcileWorktreeRestore(ctx, plan, checkpoint)
		if err != nil {
			s.workspaceRestoreFeedback(ctx, scope, grant.ReceiptID, "unknown")
			return err
		}
		raw, _ := json.Marshal(checkpoint)
		if checkpoint.Phase == "restored" {
			// Recording an already completed physical effect does not resume
			// execution or re-authorize an input canceled while we were down.
			if err := s.Store.FinishWorktreeRestoreAttempt(ctx, a, "confirmed", string(raw), "", s.now().UnixMilli()); err != nil {
				return err
			}
			s.workspaceRestoreFeedback(ctx, scope, grant.ReceiptID, "restored")
			return nil
		}
		input, err := s.Store.Submission(ctx, scope, a.ReceiptID)
		if err != nil {
			return err
		}
		if input.State == "removed" {
			if err := s.Store.AbandonCanceledWorktreeRestore(ctx, a, checkpoint.Phase, string(raw), s.now().UnixMilli()); err != nil {
				return err
			}
			return nil
		}
		if err := s.Store.ResumeWorktreeRestoreAttempt(ctx, a, checkpoint.Phase, string(raw), s.now().UnixMilli()); err != nil {
			return err
		}
		a.State, a.Phase, a.ResultJSON = "dispatching", checkpoint.Phase, string(raw)
	} else if a.State == "prepared" {
		claimed, err := s.Store.ClaimWorktreeRestoreAttempt(ctx, a, s.now().UnixMilli())
		if err != nil || !claimed {
			if err == nil {
				err = store.ErrConflict
			}
			return err
		}
		a.State = "dispatching"
	} else {
		return store.ErrConflict
	}
	persist := func(ctx context.Context, next creation.WorktreeRestoreCheckpoint) error {
		if strings.HasSuffix(next.Phase, "-dispatching") {
			input, err := s.Store.Submission(ctx, scope, a.ReceiptID)
			if err != nil || input.State != "received" {
				return store.ErrSubmissionStale
			}
		}
		// Recheck current metadata and policy before each filesystem effect.
		current, err := n.Metadata(ctx, thread)
		if err != nil || current.ID != thread {
			return store.ErrConflict
		}
		if _, err := s.workspaceRestorePolicy(ctx, current, identity); err != nil {
			return err
		}
		raw, _ := json.Marshal(next)
		if err := s.Store.CheckpointWorktreeRestoreAttempt(ctx, a, next.Phase, string(raw), s.now().UnixMilli()); err != nil {
			return err
		}
		a.Phase, a.ResultJSON = next.Phase, string(raw)
		return nil
	}
	result, restoreErr := creation.AdvanceWorktreeRestore(ctx, plan, checkpoint, persist)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if restoreErr != nil {
		if err := s.Store.FinishWorktreeRestoreAttempt(finishCtx, a, "unknown", `{}`, creation.WorktreeRestoreErrorCode(restoreErr), s.now().UnixMilli()); err != nil {
			return err
		}
		s.workspaceRestoreFeedback(ctx, scope, grant.ReceiptID, "unknown")
		return restoreErr
	}
	raw, _ := json.Marshal(result)
	if err := s.Store.FinishWorktreeRestoreAttempt(finishCtx, a, "confirmed", string(raw), "", s.now().UnixMilli()); err != nil {
		return err
	}
	s.workspaceRestoreFeedback(ctx, scope, grant.ReceiptID, "restored")
	return nil
}

func restoreFeedbackCode(err error) string {
	switch {
	case errors.Is(err, creation.ErrWorktreeSnapshotMissing):
		return "snapshot_missing"
	case errors.Is(err, creation.ErrWorktreeRestoreConflict), errors.Is(err, creation.ErrWorktreeRestoreStale):
		return "conflict"
	case errors.Is(err, creation.ErrWorktreeRestoreUnsupported), errors.Is(err, creation.ErrWorktreeProtocol):
		return "unsupported"
	default:
		return "identity_unverified"
	}
}

// Reuse the receipt's existing reply and durable outbox. Failure to deliver a
// status does not discard input or change the outcome of a Git operation.
func (s *Service) workspaceRestoreFeedback(ctx context.Context, scope store.SubmissionScope, receipt, status string) {
	if err := s.Store.StageSubmissions(ctx, scope); err != nil {
		s.notice("worktree.feedback_pending")
		return
	}
	if err := s.Store.StageQueueCardWithOptions(ctx, scope, receipt, store.QueueCardOptions{WorktreeRestore: status}); err != nil {
		s.notice("worktree.feedback_pending")
		return
	}
	if s.Gateway == nil || !s.Gateway.Ready() {
		return
	}
	b, err := s.Store.Binding(ctx, scope.HostID, scope.ThreadID)
	if err != nil || s.canDeliver(ctx, b) != nil {
		return
	}
	outbound := messages.Outbound{Store: s.Store, Gateway: s.Gateway, Membership: s.Gateway, BeginWrite: s.beginCardDelivery, Now: s.now}
	if reader, ok := s.Gateway.(messages.MessageReadbackGateway); ok {
		outbound.Readback = reader
	}
	for count := 0; count < 8 && ctx.Err() == nil; count++ {
		result, err := outbound.DispatchOne(ctx, scope.HostID, scope.ThreadID, s.now().UnixMilli())
		if err != nil || result.AttemptID == "" || result.State != "confirmed" {
			break
		}
	}
}

func (s *Service) authorizeRestoredWorkspace(ctx context.Context, t codexclient.Thread) error {
	scope := s.workspaceScope(t.ID)
	if _, err := s.Store.Binding(ctx, scope.HostID, t.ID); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	a, err := s.Store.LatestConfirmedWorktreeRestoreAttempt(ctx, scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var plan creation.WorktreeRestorePlan
	var result creation.WorktreeRestoreResult
	if json.Unmarshal([]byte(a.ParamsJSON), &plan) != nil || json.Unmarshal([]byte(a.ResultJSON), &result) != nil {
		return store.ErrConflict
	}
	if plan.Identity.RawCWD != t.CWD {
		return nil // Normal handoff retains the runtime's ordinary scope checks.
	}
	if _, err := s.workspaceRestorePolicy(ctx, t, plan.Identity); err != nil {
		return err
	}
	return creation.ValidateRestoredWorktree(ctx, plan, result)
}

func (s *Service) publishRestoredWorkspace(ctx context.Context, native workspaceRestoreNative, thread string, turns []codexclient.Turn) error {
	scope := s.workspaceScope(thread)
	a, err := s.Store.LatestConfirmedWorktreeRestoreAttempt(ctx, scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var plan creation.WorktreeRestorePlan
	var result creation.WorktreeRestoreResult
	if json.Unmarshal([]byte(a.ParamsJSON), &plan) != nil || json.Unmarshal([]byte(a.ResultJSON), &result) != nil {
		return store.ErrConflict
	}
	if !result.PublicationPending {
		return nil
	}
	meta, err := native.Metadata(ctx, thread)
	if err != nil || meta.ID != thread || meta.CWD != plan.Identity.RawCWD {
		return store.ErrConflict
	}
	if _, err := s.workspaceRestorePolicy(ctx, meta, plan.Identity); err != nil {
		return err
	}
	input, err := s.Store.Submission(ctx, scope, a.ReceiptID)
	if err != nil {
		return err
	}
	clients := map[string]bool{input.ClientID: input.State != "removed"}
	if input.State == "removed" {
		// A later ordinary input may protect the same restored checkout. This
		// only publishes its marker after exact native-history readback; the
		// canceled input is never submitted again.
		candidates, err := s.Store.WorktreeRestorePublicationCandidates(ctx, scope, a.ReceiptID)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			clients[candidate.ClientID] = true
		}
	}
	visible := false
	for _, turn := range turns {
		for _, item := range turn.Items {
			terminal := turn.Status == "completed" || turn.Status == "failed" || turn.Status == "interrupted"
			if item.Type == "userMessage" && item.ClientID != nil && clients[*item.ClientID] && turn.ID != "" && (meta.Status.Type == "active" || terminal) {
				visible = true
			}
		}
	}
	if !visible {
		return nil
	}
	if err := creation.PublishRestoredWorktree(ctx, plan, result); err != nil {
		return err
	}
	result.PublicationPending = false
	raw, _ := json.Marshal(result)
	return s.Store.MarkWorktreeRestorePublished(ctx, a, string(raw), s.now().UnixMilli())
}
