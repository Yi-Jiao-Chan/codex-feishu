package creation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type worktreeBirth struct {
	Version  int    `json:"version"`
	Identity string `json:"identity"`
}

// A private receipt outside the checkout survives a crash after git add. It
// binds recovery to that checkout generation, not merely its reusable path.
func creationWorktreeResult(ctx context.Context, p worktreeRequest, create bool) (string, error) {
	identity, err := worktreeOwnerIdentity(ctx, p)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(worktreeBirth{Version: 1, Identity: identity})
	if err := worktreePrivateFile(ctx, p, ".codex-feishu-"+p.DraftID+".identity", raw, create, identity); err != nil {
		return "", err
	}
	current, err := worktreeOwnerIdentity(ctx, p)
	if err != nil || current != identity {
		return "", ErrWorktreeIdentity
	}
	return string(raw), nil
}

func verifyCreationWorktreeGeneration(ctx context.Context, i store.CreationIntent, a store.CreationAttempt) error {
	if !i.ManagedWorktreePublication() && a.Result == "{}" {
		return nil // Historical records are audited separately, never rewritten.
	}
	var birth worktreeBirth
	if json.Unmarshal([]byte(a.Result), &birth) != nil || birth.Version != 1 || birth.Identity == "" {
		return ErrWorktreeIdentity
	}
	identity, err := worktreeOwnerIdentity(ctx, frozenWorktree(i))
	if err != nil || identity != birth.Identity {
		return ErrWorktreeIdentity
	}
	return nil
}

// Publish a complete private record without replacing a concurrent writer.
// Empty .codex-worktree-name is the desktop scanner's publication boundary.
func worktreePrivateFile(ctx context.Context, p worktreeRequest, name string, raw []byte, create bool, expectedIdentity string) error {
	w, err := resolveWorktreeOwner(ctx, p)
	if err != nil {
		return err
	}
	defer w.close()
	identity, err := w.identity()
	if err != nil || identity != expectedIdentity {
		return ErrWorktreeIdentity
	}
	if err := worktreeMarker(p, false); err != nil {
		return err
	}
	parent := filepath.Dir(p.Destination)
	root, before, err := openOwnerDirectory(parent)
	if err != nil {
		return ErrWorktreeStorage
	}
	defer root.Close()
	markerName := ".codex-feishu-" + p.DraftID + ".json"
	if filepath.Base(parent) == p.DraftID && filepath.Base(p.Destination) == "checkout" {
		markerName = "creation.json"
	}
	marker, markerInfo, err := readOwnerControl(root, markerName)
	expectedMarker, _ := json.Marshal(p)
	checkoutInfo, checkoutErr := root.Lstat(filepath.Base(p.Destination))
	if err != nil || markerInfo.Mode().Perm()&0077 != 0 || !bytes.Equal(marker, expectedMarker) || checkoutErr != nil || !checkoutInfo.IsDir() || !os.SameFile(checkoutInfo, w.checkoutInfo) {
		return ErrWorktreeIdentity
	}
	if create {
		tmp := ".codex-feishu-write-" + store.ID()
		f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return ErrWorktreeStorage
		}
		defer root.Remove(tmp)
		_, err = f.Write(raw)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return ErrWorktreeStorage
		}
		if err := w.verify(ctx, p); err != nil {
			return err
		}
		current, err := os.Lstat(parent)
		if err != nil || !os.SameFile(current, before) {
			return ErrWorktreeIdentity
		}
		if err := root.Link(tmp, name); err != nil && !errors.Is(err, os.ErrExist) {
			return ErrWorktreeStorage
		}
		dir, err := root.Open(".")
		if err != nil {
			return ErrWorktreeStorage
		}
		err = dir.Sync()
		dir.Close()
		if err != nil {
			return ErrWorktreeStorage
		}
	}
	actual, info, err := readOwnerControl(root, name)
	if err != nil || info.Mode().Perm()&0077 != 0 || !bytes.Equal(actual, raw) {
		return ErrWorktreeIdentity
	}
	afterRoot, after, err := openOwnerDirectory(parent)
	if err != nil {
		return ErrWorktreeIdentity
	}
	afterRoot.Close()
	if !os.SameFile(before, after) {
		return ErrWorktreeIdentity
	}
	return w.verify(ctx, p)
}

type worktreeOwnerRequest struct {
	Version  int    `json:"version"`
	ThreadID string `json:"threadId"`
	Identity string `json:"identity"`
}

func (m *Manager) ownerRequest(ctx context.Context, i store.CreationIntent) (worktreeOwnerRequest, error) {
	if i.NativeThreadID == "" {
		return worktreeOwnerRequest{}, ErrWorktreeIdentity
	}
	a, err := m.Store.CreationAttempt(ctx, i.DraftID, "worktree")
	if err != nil || a.State != "confirmed" || a.RemoteID != i.WorktreePath {
		return worktreeOwnerRequest{}, ErrWorktreeIdentity
	}
	if err := verifyCreationWorktreeGeneration(ctx, i, a); err != nil {
		return worktreeOwnerRequest{}, err
	}
	identity, err := worktreeOwnerIdentity(ctx, frozenWorktree(i))
	return worktreeOwnerRequest{Version: 1, ThreadID: i.NativeThreadID, Identity: identity}, err
}

func (m *Manager) ensureCreationWorktreeOwner(ctx context.Context, n Native, i store.CreationIntent) (bool, error) {
	if _, err := managedWorktreeHome(n); err != nil {
		return false, err
	}
	p, err := m.ownerRequest(ctx, i)
	if err != nil {
		return false, err
	}
	raw, _ := json.Marshal(p)
	a, err := m.Store.PrepareCreationAttempt(ctx, i.DraftID, "worktree-owner", string(raw), m.now().UnixMilli())
	if err != nil {
		return false, err
	}
	if a.State == "confirmed" || a.State == "unknown" {
		owner, err := readWorktreeOwner(ctx, frozenWorktree(i))
		if err != nil || owner != i.NativeThreadID {
			return false, ErrWorktreeIdentity
		}
		if a.State == "confirmed" {
			return true, nil
		}
		return true, m.Store.FinishCreationAttempt(ctx, a, "confirmed", i.NativeThreadID, `{}`)
	}
	if a.State != "prepared" {
		return false, nil
	}
	if !i.ManagedWorktreePublication() {
		legacy, _ := legacyWorktreePath(m.Config.Storage.DatabaseFile, i.DraftID)
		if i.WorktreePath != legacy {
			// A missing owner on an already exposed historical path needs a
			// deliberate repair with desktop cleanup quiescent, not this lane.
			owner, err := readWorktreeOwner(ctx, frozenWorktree(i))
			if err != nil || owner != i.NativeThreadID {
				return false, ErrWorktreeProtocol
			}
		}
	}
	ok, err := m.Store.DispatchCreationAttempt(ctx, a)
	if err != nil || !ok {
		return false, err
	}
	err = ensureWorktreeOwnerWithIdentity(ctx, frozenWorktree(i), i.NativeThreadID, p.Identity)
	outcome := "confirmed"
	if err != nil {
		outcome = "unknown"
	}
	if finishErr := m.Store.FinishCreationAttempt(ctx, a, outcome, i.NativeThreadID, `{}`); finishErr != nil {
		return false, finishErr
	}
	return err == nil, err
}

func (m *Manager) checkCreationWorktreeOwner(ctx context.Context, i store.CreationIntent) error {
	p, err := m.ownerRequest(ctx, i)
	if err != nil {
		return err
	}
	a, err := m.Store.CreationAttempt(ctx, i.DraftID, "worktree-owner")
	raw, _ := json.Marshal(p)
	if err != nil || a.State != "confirmed" || a.RemoteID != i.NativeThreadID || a.Params != string(raw) {
		return ErrWorktreeIdentity
	}
	owner, err := readWorktreeOwner(ctx, frozenWorktree(i))
	if err != nil || owner != i.NativeThreadID {
		return ErrWorktreeIdentity
	}
	return nil
}

// Do not expose a new owner while its first input is merely queued. The App
// protects active owners; a completed first turn may enter normal idle cleanup.
func initialWorktreeTurnVisible(ctx context.Context, n Native, i store.CreationIntent, receipt store.Receipt) (bool, error) {
	thread, err := n.Metadata(ctx, i.NativeThreadID)
	if err != nil {
		return false, err
	}
	var source string
	expected, _ := codexclient.CreationThreadSource(i.ID)
	if thread.ID != i.NativeThreadID || thread.CWD != i.ExecutionDirectory() || thread.Ephemeral || json.Unmarshal(thread.ThreadSource, &source) != nil || source != expected {
		return false, ErrWorktreeIdentity
	}
	turns, err := n.Turns(ctx, i.NativeThreadID)
	if err != nil {
		return false, err
	}
	for _, turn := range turns {
		for _, item := range turn.Items {
			if item.Type != "userMessage" || item.ClientID == nil || *item.ClientID != receipt.ClientID {
				continue
			}
			terminal := turn.Status == "completed" || turn.Status == "failed" || turn.Status == "interrupted"
			return thread.Status.Type == "active" || terminal, nil
		}
	}
	return false, nil
}

func (m *Manager) publishCreationWorktree(ctx context.Context, n Native, i store.CreationIntent, receipt store.Receipt) (bool, error) {
	if !i.ManagedWorktreePublication() {
		return true, nil
	}
	if _, err := managedWorktreeHome(n); err != nil {
		return false, err
	}
	if err := m.checkCreationWorktreeOwner(ctx, i); err != nil {
		return false, err
	}
	ready, err := initialWorktreeTurnVisible(ctx, n, i, receipt)
	if err != nil || !ready {
		return false, err
	}
	p, err := m.ownerRequest(ctx, i)
	if err != nil {
		return false, err
	}
	raw, _ := json.Marshal(p)
	a, err := m.Store.PrepareCreationAttempt(ctx, i.DraftID, "worktree-publish", string(raw), m.now().UnixMilli())
	if err != nil {
		return false, err
	}
	if a.State == "confirmed" || a.State == "unknown" {
		if err := worktreePrivateFile(ctx, frozenWorktree(i), ".codex-worktree-name", nil, false, p.Identity); err != nil {
			return false, err
		}
		if a.State == "confirmed" {
			return true, nil
		}
		return true, m.Store.FinishCreationAttempt(ctx, a, "confirmed", i.NativeThreadID, `{}`)
	}
	if a.State != "prepared" {
		return false, nil
	}
	ok, err := m.Store.DispatchCreationAttempt(ctx, a)
	if err != nil || !ok {
		return false, err
	}
	err = worktreePrivateFile(ctx, frozenWorktree(i), ".codex-worktree-name", nil, true, p.Identity)
	if err == nil {
		err = m.checkCreationWorktreeOwner(ctx, i)
	}
	outcome := "confirmed"
	if err != nil {
		outcome = "unknown"
	}
	if finishErr := m.Store.FinishCreationAttempt(ctx, a, outcome, i.NativeThreadID, `{}`); finishErr != nil {
		return false, finishErr
	}
	return err == nil, err
}
