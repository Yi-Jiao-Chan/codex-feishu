package verification

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"codex-feishu/internal/codexclient"
)

var ErrResourcesUnavailable = errors.New("original verification resources unavailable")

// NativeSessions owns local resources for explicitly authorized thread probes.
// It accepts only a request ID: the model, private workspace and fixed native
// policy are selected internally. It has no input, binding, archive or RPC API.
// Directory ownership markers are retained on failure and Close; never remove
// a workspace to retry an ambiguous creation. A service restart does not adopt
// an old model port or silently replace the original request's resources.
type NativeSessions struct {
	native *NativeManager
	root   string
	life   context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	active map[string]*nativeSession
	calls  sync.WaitGroup
}

type nativeSession struct {
	spec   codexclient.LocalVerificationSpec
	model  *LocalModel
	life   context.Context
	cancel context.CancelFunc
}

type NativeSessionResult struct {
	NativeResult
	ResourcesReady           bool  `json:"resources_ready"`
	ResourceRecoveryRequired bool  `json:"resource_recovery_required"`
	ModelRequests            int64 `json:"model_requests"`
}

// NewNativeSessions only validates the existing private physical resource root.
// The caller owns the service lifetime and must Close before closing its store.
// NativeManager's identity and root are immutable for this service instance.
func NewNativeSessions(parent context.Context, native *NativeManager, root string) (*NativeSessions, error) {
	if parent == nil || native == nil || native.Store == nil || native.HostID == "" || native.AppID == "" || native.OwnerID == "" || native.Commit == "" || !ValidBinaryHash(native.BinarySHA256) {
		return nil, ErrInvalid
	}
	if err := privateResourceDirectory(root); err != nil {
		return nil, err
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(parent)
	return &NativeSessions{native: native, root: root, life: life, cancel: cancel, active: map[string]*nativeSession{}}, nil
}

// Begin serializes resource provisioning, then invokes the durable native
// creation step on the existing initialized connection. It never creates a
// replacement for an existing journal or partial workspace after a restart.
func (s *NativeSessions) Begin(ctx context.Context, id string) (NativeSessionResult, error) {
	if !ValidRequestID(id) {
		return NativeSessionResult{}, ErrInvalid
	}
	call, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.life, cancel)
	defer func() { stop(); cancel() }()
	entry, result, err := s.prepare(call, id)
	if err != nil || entry == nil {
		return result, err
	}
	defer s.calls.Done()
	stopModel := context.AfterFunc(entry.life, cancel)
	defer stopModel()
	if s.life.Err() != nil || entry.life.Err() != nil {
		cancel()
	}
	// RPC runs without the resource mutex. State can observe dispatching as
	// unknown while a response is outstanding; Close still joins this finish.
	nativeResult, err := s.native.Begin(call, id, entry.spec)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resultLocked(id, nativeResult), err
}

func (s *NativeSessions) prepare(call context.Context, id string) (*nativeSession, NativeSessionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.life.Err(); err != nil {
		return nil, NativeSessionResult{}, err
	}
	if err := call.Err(); err != nil {
		return nil, NativeSessionResult{}, err
	}
	// Reads remain scoped to the original deployment owner. No local resource
	// creation precedes knowing whether this ID has an existing native journal.
	_, err := s.native.Store.VerificationNative(call, id, s.native.HostID, s.native.AppID, s.native.OwnerID)
	if err == nil {
		entry := s.active[id]
		if !s.resourcesLive(entry) {
			result, readErr := s.native.State(call, id)
			return nil, NativeSessionResult{NativeResult: result, ResourceRecoveryRequired: readErr == nil}, readErr
		}
		s.calls.Add(1)
		return entry, NativeSessionResult{}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, NativeSessionResult{}, err
	}
	// If journal persistence failed after local provisioning, only the same
	// live entry can retry preparation; its directory and endpoint stay fixed.
	entry := s.active[id]
	if entry == nil {
		if n, _, _ := s.native.current(); n == nil {
			return nil, NativeSessionResult{}, ErrNotReady
		}
		cwd, err := s.workspace(id)
		if err != nil {
			return nil, NativeSessionResult{ResourceRecoveryRequired: errors.Is(err, ErrResourcesUnavailable)}, err
		}
		// The endpoint is disposable and bounded to this verification window;
		// it must not keep an unused or held fixture alive for the whole service.
		modelLife, stopModel := context.WithTimeout(s.life, 5*time.Minute)
		model, err := StartLocalModel(modelLife)
		if err != nil {
			stopModel()
			return nil, NativeSessionResult{ResourceRecoveryRequired: true}, err
		}
		entry = &nativeSession{model: model, life: modelLife, cancel: stopModel, spec: codexclient.LocalVerificationSpec{RunID: id, CWD: cwd, ModelURL: model.URL()}}
		s.active[id] = entry
	}
	if !s.resourcesLive(entry) {
		return nil, NativeSessionResult{ResourceRecoveryRequired: true}, ErrResourcesUnavailable
	}
	s.calls.Add(1)
	return entry, NativeSessionResult{}, nil
}

// State reads the journal without starting a model, creating/changing files,
// reopening a native client or submitting input. Resource readiness belongs to
// this live instance; confirmed native birth is a separate historical fact.
func (s *NativeSessions) State(ctx context.Context, id string) (NativeSessionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.native.State(ctx, id)
	if err != nil {
		return NativeSessionResult{}, err
	}
	return s.resultLocked(id, result), nil
}

// Release only opens the local fixture's first-response gate. The eventual
// managed verifier owns when to call it after its queue/steer observations.
// It performs no native or Feishu operation and cannot select another target.
func (s *NativeSessions) Release(id string) error {
	if !ValidRequestID(id) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.resourcesLive(s.active[id]) {
		return ErrResourcesUnavailable
	}
	s.active[id].model.Release()
	return nil
}

func (s *NativeSessions) Close() error {
	// Cancellation precedes the lock so an in-flight native call cannot keep
	// Close waiting indefinitely for its request's original context.
	s.cancel()
	s.mu.Lock()
	var errs []error
	for id, entry := range s.active {
		entry.cancel()
		if err := entry.model.Close(); err != nil {
			errs = append(errs, err)
		}
		delete(s.active, id)
	}
	s.mu.Unlock()
	// No Add can occur after cancellation because prepare checks life while
	// holding mu. Join known/unknown result persistence before store shutdown.
	s.calls.Wait()
	return errors.Join(errs...)
}

func (s *NativeSessions) resultLocked(id string, result NativeResult) NativeSessionResult {
	entry := s.active[id]
	ready := s.resourcesLive(entry)
	r := NativeSessionResult{NativeResult: result, ResourcesReady: ready, ResourceRecoveryRequired: !ready}
	if entry != nil {
		r.ModelRequests = entry.model.Calls()
	}
	return r
}

func (s *NativeSessions) resourcesLive(entry *nativeSession) bool {
	if entry == nil || s.life.Err() != nil || entry.life.Err() != nil {
		return false
	}
	if privateResourceDirectory(s.root) != nil || privateResourceDirectory(entry.spec.CWD) != nil {
		return false
	}
	select {
	case <-entry.model.Done():
		return false
	default:
		return true
	}
}

type workspaceOwner struct {
	Schema       int    `json:"schema"`
	RequestID    string `json:"request_id"`
	HostID       string `json:"host_id"`
	AppID        string `json:"app_id"`
	OwnerID      string `json:"owner_id"`
	Commit       string `json:"bridge_commit"`
	BinarySHA256 string `json:"binary_sha256"`
}

func (s *NativeSessions) workspace(id string) (string, error) {
	if err := privateResourceDirectory(s.root); err != nil {
		return "", err
	}
	workspace := filepath.Join(s.root, id)
	if err := os.Mkdir(workspace, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", ErrResourcesUnavailable
		}
		return "", errors.New("cannot create private verification workspace")
	}
	// Retain even a partial directory on any error: this ID cannot be reused
	// to create a different test after a process stops between local steps.
	if err := syncResourceDirectory(s.root); err != nil {
		return "", err
	}
	owner := workspaceOwner{Schema: 1, RequestID: id, HostID: s.native.HostID, AppID: s.native.AppID, OwnerID: s.native.OwnerID, Commit: s.native.Commit, BinarySHA256: s.native.BinarySHA256}
	body, err := json.Marshal(owner)
	if err != nil {
		return "", ErrInvalid
	}
	f, err := os.OpenFile(filepath.Join(workspace, "owner.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", errors.New("cannot persist verification workspace owner")
	}
	_, writeErr := f.Write(body)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return "", errors.New("cannot persist verification workspace owner")
	}
	cwd := filepath.Join(workspace, "cwd")
	if err := os.Mkdir(cwd, 0700); err != nil {
		return "", errors.New("cannot create verification working directory")
	}
	if err := syncResourceDirectory(workspace); err != nil {
		return "", err
	}
	if err := privateResourceDirectory(cwd); err != nil {
		return "", err
	}
	return cwd, nil
}

func privateResourceDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalid
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || physical != path {
		return ErrInvalid
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return ErrInvalid
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrInvalid
	}
	return nil
}

func syncResourceDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("cannot persist verification workspace")
	}
	defer f.Close()
	if err = f.Sync(); err != nil {
		return errors.New("cannot persist verification workspace")
	}
	return nil
}
