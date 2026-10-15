package app

import (
	"context"
	"database/sql"
	"errors"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/creation"
	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

// Explicit creation has its own directory policy. Its durable native identity
// and target authorize continued sync; a source label alone grants nothing.
func (s *Service) authorizeCreation(ctx context.Context, t codexclient.Thread, automatic bool) (bool, error) {
	i, err := s.Store.BridgeCreationThread(ctx, s.Config.Host.ID, s.Config.Feishu.AppID, t.ID)
	if errors.Is(err, sql.ErrNoRows) {
		if t.BridgeCreated() {
			return true, store.ErrIneligible
		}
		return false, nil
	}
	if err != nil {
		return true, err
	}
	cwd := i.ExecutionDirectory()
	if automatic || i.State != "confirmed" || t.Ephemeral || t.ParentThreadID != nil || t.ForkedFromID != nil || cwd == "" || t.CWD != cwd || !s.Config.Operator(i.CreatorID) || i.OwnerID != s.Config.Access.OwnerOpenID {
		return true, store.ErrIneligible
	}
	directories := creation.DirectoryService{Roots: s.Config.Creation.AllowedRoots}
	if choice, err := directories.Validate(ctx, i.Directory, ""); err != nil || choice.Path != i.Directory {
		return true, store.ErrIneligible
	}
	if i.WorkspaceMode == "worktree" {
		// Scope follows the authorized source repository; the private checkout
		// need not be directly inside the selectable directory roots.
		if choice, err := directories.Validate(ctx, i.GitRoot, ""); err != nil || choice.Path != i.GitRoot {
			return true, store.ErrIneligible
		}
		if err := creation.ValidateCreatedWorktree(ctx, i); err != nil {
			return true, store.ErrIneligible
		}
	}
	if i.ProjectID != "" && (t.ProjectID == nil || *t.ProjectID != i.ProjectID) {
		return true, store.ErrConflict
	}
	b, err := s.Store.Binding(ctx, s.Config.Host.ID, t.ID)
	if err != nil {
		return true, err
	}
	if b.IntentID != i.ID || b.RootMessageID != i.RootMessageID || b.RepairRequired {
		return true, store.ErrConflict
	}
	return true, nil
}

func (s *Service) creationAttachGate(ctx context.Context, thread string) error {
	i, err := s.Store.BridgeCreationThread(ctx, s.Config.Host.ID, s.Config.Feishu.AppID, thread)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if i.State != "confirmed" {
		return store.ErrIneligible
	}
	return nil
}

func (s *Service) verifyFollowTarget(ctx context.Context, b store.Binding) error {
	if b.RepairRequired {
		return store.ErrTargetRepair
	}
	if b.FollowMode != "topic" {
		return nil
	}
	reader, _ := s.Gateway.(messages.MessageReadbackGateway)
	err := messages.VerifyTopicRoot(ctx, reader, b)
	if errors.Is(err, messages.ErrTopicRootGone) {
		if save := s.Store.MarkBindingRepair(ctx, b.HostID, b.ThreadID, "topic_root_unavailable"); save != nil {
			return save
		}
		return store.ErrTargetRepair
	}
	return err
}
