// Package control exposes same-user health, pairing and owner-only test cards.
// It has no arbitrary message destination, native RPC or shell endpoint.
package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/pairing"
	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

const protocol = 1
const maxWireBytes = 64 << 10

var (
	ErrUnavailable = errors.New("managed instance unavailable")
	ErrIdentity    = errors.New("managed instance configuration differs")
	ErrUnsafe      = errors.New("control endpoint ownership or permissions are invalid")
	ErrActive      = errors.New("control endpoint already active")
	ErrUnsupported = errors.New("management method unsupported")
)

// Snapshot deliberately contains no thread IDs, messages, request bodies,
// credentials, native policies or error payloads.
type Snapshot struct {
	Running                             bool                   `json:"running"`
	NativeConnected                     bool                   `json:"native_connected"`
	FeishuConnected                     bool                   `json:"feishu_connected"`
	HydratedBindings                    int                    `json:"hydrated_bindings"`
	DiscoveryEnabled                    bool                   `json:"discovery_enabled"`
	PairingSupported                    bool                   `json:"pairing_supported"`
	VerificationCardSupported           bool                   `json:"verification_card_supported"`
	VerificationGroupSupported          bool                   `json:"verification_group_supported"`
	VerificationThreadCreationSupported bool                   `json:"verification_thread_creation_supported"`
	StartedAtMS                         int64                  `json:"started_at_ms"`
	ObservedAtMS                        int64                  `json:"observed_at_ms"`
	Persistent                          store.PersistentHealth `json:"durable"`
}

type request struct {
	Schema             int                  `json:"schema_version"`
	Method             string               `json:"method"`
	Identity           string               `json:"identity"`
	Pair               *pairRequest         `json:"pair,omitempty"`
	Verification       *verificationRequest `json:"verification,omitempty"`
	VerificationGroup  *verificationRequest `json:"verification_group,omitempty"`
	VerificationThread *verificationRequest `json:"verification_thread,omitempty"`
}
type verificationRequest struct {
	RequestID string `json:"request_id"`
}
type pairRequest struct {
	RequestID  string `json:"request_id"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}
type response struct {
	Schema             int                               `json:"schema_version"`
	Identity           string                            `json:"identity"`
	Code               string                            `json:"code"`
	Snapshot           *Snapshot                         `json:"snapshot,omitempty"`
	Pair               *pairing.Challenge                `json:"pair,omitempty"`
	Verification       *verification.Result              `json:"verification,omitempty"`
	VerificationGroup  *verification.GroupResult         `json:"verification_group,omitempty"`
	VerificationThread *verification.NativeSessionResult `json:"verification_thread,omitempty"`
}

type GroupVerificationProvider interface {
	Begin(context.Context, string) (verification.GroupResult, error)
	State(context.Context, string) (verification.GroupResult, error)
}

type VerificationProvider interface {
	Begin(context.Context, string) (verification.Result, error)
	State(context.Context, string) (verification.Result, error)
}

type ThreadVerificationProvider interface {
	Begin(context.Context, string) (verification.NativeSessionResult, error)
	State(context.Context, string) (verification.NativeSessionResult, error)
}

type PairingProvider interface {
	Begin(string, time.Duration) (pairing.Challenge, error)
	State(string) (pairing.Result, error)
	Cancel(string) (pairing.Result, error)
}

// Identity binds the running configuration, not just an App ID. A changed
// config cannot be reported as applied merely because an old service is live.
func Identity(configPath string, cfg config.Config) (string, error) {
	if !filepath.IsAbs(configPath) {
		return "", ErrIdentity
	}
	physical, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		return "", ErrIdentity
	}
	b, err := json.Marshal(struct {
		Path   string
		Config config.Config
	}{physical, cfg})
	if err != nil {
		return "", ErrIdentity
	}
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:]), nil
}

// Like the per-application receiver lock, this location is independent of XDG
// overrides and database choices. It never depends on a caller's cwd.
func Path(appID string) (string, error) {
	if appID == "" {
		return "", ErrUnsafe
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", ErrUnsafe
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return "", ErrUnsafe
	}
	hash := sha256.Sum256([]byte(appID))
	path := filepath.Join(home, ".local", "state", "codex-feishu", "control", hex.EncodeToString(hash[:12])+".sock")
	if len(path) >= 108 {
		return "", errors.New("control Unix socket path is too long")
	}
	return path, nil
}

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err != nil || !info.IsDir() || !owned(info) || info.Mode().Perm() != 0700 {
		return ErrUnsafe
	}
	// Reject aliases of the endpoint directory; Path canonicalizes HOME only.
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || physical != filepath.Clean(path) {
		return ErrUnsafe
	}
	return nil
}
func socketInfo(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || len(path) >= 108 {
		return nil, ErrUnsafe
	}
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 || !owned(info) || info.Mode().Perm() != 0600 {
		return nil, ErrUnsafe
	}
	return info, nil
}

type Server struct {
	// Set before Serve; production binds the manager to the same service,
	// configured owner and already-locked database as the receiver.
	Verification        VerificationProvider
	VerificationGroups  GroupVerificationProvider
	VerificationThreads ThreadVerificationProvider
	listener            *net.UnixListener
	path                string
	identity            string
	info                os.FileInfo
	provider            func(context.Context) (Snapshot, error)
	pairing             PairingProvider
	mu                  sync.Mutex
	closing             bool
	conns               map[*net.UnixConn]struct{}
	workers             sync.WaitGroup
	once                sync.Once
}

// Listen must be called while the caller owns the application's receiver lock.
// It only replaces a socket proven stale by ECONNREFUSED, never an active or
// ambiguous endpoint, regular file, symlink or foreign-owned socket.
func Listen(ctx context.Context, path, identity string, provider func(context.Context) (Snapshot, error), pairProviders ...PairingProvider) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) || len(path) >= 108 || len(identity) != 64 || provider == nil || len(pairProviders) > 1 {
		return nil, ErrUnsafe
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, ErrUnsafe
	}
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if info, err := socketInfo(path); err == nil {
		dialCtx, cancel := context.WithTimeout(ctx, time.Second)
		conn, dialErr := (&net.Dialer{}).DialContext(dialCtx, "unix", path)
		cancel()
		if dialErr == nil {
			conn.Close()
			return nil, ErrActive
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return nil, ErrUnavailable
		}
		current, statErr := socketInfo(path)
		if statErr != nil || !os.SameFile(info, current) {
			return nil, ErrUnsafe
		}
		if err := os.Remove(path); err != nil {
			return nil, ErrUnsafe
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, ErrUnavailable
	}
	l.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, ErrUnsafe
	}
	info, err := socketInfo(path)
	if err != nil {
		l.Close()
		return nil, err
	}
	server := &Server{listener: l, path: path, identity: identity, info: info, provider: provider, conns: map[*net.UnixConn]struct{}{}}
	if len(pairProviders) == 1 {
		server.pairing = pairProviders[0]
	}
	return server, nil
}

func (s *Server) Close() {
	s.once.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.listener.Close()
		for conn := range s.conns {
			conn.Close()
		}
		s.mu.Unlock()
		if info, err := os.Lstat(s.path); err == nil && info.Mode()&os.ModeSocket != 0 && os.SameFile(info, s.info) {
			os.Remove(s.path)
		}
	})
}

func (s *Server) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, s.Close)
	defer stop()
	defer s.workers.Wait()
	defer s.Close()
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrUnavailable
		}
		s.mu.Lock()
		if s.closing || len(s.conns) >= 8 {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		s.conns[conn] = struct{}{}
		s.workers.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.workers.Done()
			defer func() {
				conn.Close()
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
			}()
			s.handle(ctx, conn)
		}()
	}
}

func decode(r io.Reader, dst any) error {
	b, err := io.ReadAll(io.LimitReader(r, maxWireBytes+1))
	if err != nil || len(b) > maxWireBytes {
		return ErrUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return ErrUnavailable
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrUnavailable
	}
	return nil
}

func (s *Server) handle(parent context.Context, conn *net.UnixConn) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if sameUser(conn) != nil {
		return
	}
	r := request{}
	if decode(conn, &r) != nil {
		return
	}
	result := response{Schema: protocol, Identity: s.identity}
	switch {
	case r.Schema != protocol:
		result.Code = "unsupported"
	case r.Identity != s.identity:
		result.Code = "identity-conflict"
	case r.Method == "health" && r.Pair == nil && r.Verification == nil && r.VerificationGroup == nil && r.VerificationThread == nil:
		snapshot, err := s.provider(ctx)
		if err != nil {
			result.Code = "health-unavailable"
		} else {
			result.Code, result.Snapshot = "ok", &snapshot
		}
	case (r.Method == "verify.group.begin" || r.Method == "verify.group.state") && s.VerificationGroups != nil && r.VerificationGroup != nil && r.Pair == nil && r.Verification == nil && r.VerificationThread == nil:
		var probe verification.GroupResult
		var err error
		if !verification.ValidRequestID(r.VerificationGroup.RequestID) {
			err = verification.ErrInvalid
		} else if r.Method == "verify.group.begin" {
			probe, err = s.VerificationGroups.Begin(ctx, r.VerificationGroup.RequestID)
		} else {
			probe, err = s.VerificationGroups.State(ctx, r.VerificationGroup.RequestID)
		}
		switch {
		case err == nil:
			result.Code, result.VerificationGroup = "ok", &probe
		case errors.Is(err, verification.ErrInvalid):
			result.Code = "verification-invalid"
		case errors.Is(err, verification.ErrNotFound):
			result.Code = "verification-not-found"
		default:
			result.Code = "verification-unavailable"
		}
	case (r.Method == "verify.card.begin" || r.Method == "verify.card.state") && s.Verification != nil && r.Verification != nil && r.Pair == nil && r.VerificationGroup == nil && r.VerificationThread == nil:
		var probe verification.Result
		var err error
		if !verification.ValidRequestID(r.Verification.RequestID) {
			err = verification.ErrInvalid
		} else if r.Method == "verify.card.begin" {
			probe, err = s.Verification.Begin(ctx, r.Verification.RequestID)
		} else {
			probe, err = s.Verification.State(ctx, r.Verification.RequestID)
		}
		if err == nil {
			result.Code, result.Verification = "ok", &probe
		} else if errors.Is(err, verification.ErrInvalid) {
			result.Code = "verification-invalid"
		} else if errors.Is(err, verification.ErrNotFound) {
			result.Code = "verification-not-found"
		} else {
			result.Code = "verification-unavailable"
		}
	case (r.Method == "verify.thread.begin" || r.Method == "verify.thread.state") && s.VerificationThreads != nil && r.VerificationThread != nil && r.Pair == nil && r.Verification == nil && r.VerificationGroup == nil:
		var probe verification.NativeSessionResult
		var err error
		if !verification.ValidRequestID(r.VerificationThread.RequestID) {
			err = verification.ErrInvalid
		} else if r.Method == "verify.thread.begin" {
			probe, err = s.VerificationThreads.Begin(ctx, r.VerificationThread.RequestID)
		} else {
			probe, err = s.VerificationThreads.State(ctx, r.VerificationThread.RequestID)
		}
		switch {
		case err == nil:
			result.Code, result.VerificationThread = "ok", &probe
		case errors.Is(err, verification.ErrInvalid):
			result.Code = "verification-invalid"
		case errors.Is(err, verification.ErrNotFound):
			result.Code = "verification-not-found"
		default:
			result.Code = "verification-unavailable"
		}
	case (r.Method == "pair.begin" || r.Method == "pair.state" || r.Method == "pair.cancel") && s.pairing != nil && r.Pair != nil && r.Verification == nil && r.VerificationGroup == nil && r.VerificationThread == nil:
		var challenge pairing.Challenge
		var err error
		switch r.Method {
		case "pair.begin":
			if r.Pair.TTLSeconds < 1 || r.Pair.TTLSeconds > 300 {
				err = pairing.ErrInvalid
			} else {
				challenge, err = s.pairing.Begin(r.Pair.RequestID, time.Duration(r.Pair.TTLSeconds)*time.Second)
			}
		case "pair.state", "pair.cancel":
			if r.Pair.TTLSeconds != 0 {
				err = pairing.ErrInvalid
			} else if r.Method == "pair.state" {
				challenge.Result, err = s.pairing.State(r.Pair.RequestID)
			} else {
				challenge.Result, err = s.pairing.Cancel(r.Pair.RequestID)
			}
		}
		switch {
		case err == nil:
			result.Code, result.Pair = "ok", &challenge
		case errors.Is(err, pairing.ErrBusy):
			result.Code = "pair-busy"
		case errors.Is(err, pairing.ErrNotFound):
			result.Code = "pair-not-found"
		case errors.Is(err, pairing.ErrInvalid):
			result.Code = "pair-invalid"
		case errors.Is(err, pairing.ErrClosed):
			result.Code = "pair-closed"
		default:
			result.Code = "pair-unavailable"
		}
	default:
		result.Code = "unsupported"
	}
	// The provider returns only the bounded, typed aggregate snapshot.
	json.NewEncoder(conn).Encode(result)
}

// Query never creates an endpoint, receiver or database. Both peers' OS UIDs
// are checked; filesystem restrictions alone are not the authentication.
func exchange(ctx context.Context, path string, req request) (response, error) {
	var result response
	if _, err := socketInfo(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, ErrUnavailable
		}
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return result, ErrUnavailable
	}
	defer conn.Close()
	c := conn.(*net.UnixConn)
	if err := sameUser(c); err != nil {
		return result, err
	}
	deadline, _ := ctx.Deadline()
	c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if json.NewEncoder(c).Encode(req) != nil || c.CloseWrite() != nil {
		return result, ErrUnavailable
	}
	if decode(c, &result) != nil || result.Schema != protocol {
		return result, ErrUnavailable
	}
	if result.Code == "identity-conflict" || result.Identity != req.Identity {
		return result, ErrIdentity
	}
	switch result.Code {
	case "ok":
		return result, nil
	case "unsupported":
		return result, ErrUnsupported
	case "pair-busy":
		return result, pairing.ErrBusy
	case "pair-not-found":
		return result, pairing.ErrNotFound
	case "pair-invalid":
		return result, pairing.ErrInvalid
	case "pair-closed":
		return result, pairing.ErrClosed
	case "verification-invalid":
		return result, verification.ErrInvalid
	case "verification-not-found":
		return result, verification.ErrNotFound
	default:
		return result, ErrUnavailable
	}
}

func Query(ctx context.Context, path, identity string) (Snapshot, error) {
	result, err := exchange(ctx, path, request{Schema: protocol, Method: "health", Identity: identity})
	if err != nil {
		return Snapshot{}, err
	}
	if result.Snapshot == nil || result.Pair != nil || result.Verification != nil || result.VerificationGroup != nil || result.VerificationThread != nil {
		return Snapshot{}, ErrUnavailable
	}
	now := time.Now().UnixMilli()
	if result.Snapshot.ObservedAtMS < now-10_000 || result.Snapshot.ObservedAtMS > now+1_000 {
		return Snapshot{}, ErrUnavailable
	}
	return *result.Snapshot, nil
}
