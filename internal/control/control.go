// Package control exposes only local management facts. It has no native RPC,
// message-sending or shell-execution endpoint.
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
	"codex-feishu/internal/store"
)

const protocol = 1
const maxWireBytes = 64 << 10

var (
	ErrUnavailable = errors.New("managed instance unavailable")
	ErrIdentity    = errors.New("managed instance configuration differs")
	ErrUnsafe      = errors.New("control endpoint ownership or permissions are invalid")
	ErrActive      = errors.New("control endpoint already active")
)

// Snapshot deliberately contains no thread IDs, messages, request bodies,
// credentials, native policies or error payloads.
type Snapshot struct {
	Running          bool                   `json:"running"`
	NativeConnected  bool                   `json:"native_connected"`
	FeishuConnected  bool                   `json:"feishu_connected"`
	HydratedBindings int                    `json:"hydrated_bindings"`
	DiscoveryEnabled bool                   `json:"discovery_enabled"`
	StartedAtMS      int64                  `json:"started_at_ms"`
	ObservedAtMS     int64                  `json:"observed_at_ms"`
	Persistent       store.PersistentHealth `json:"durable"`
}

type request struct {
	Schema   int    `json:"schema_version"`
	Method   string `json:"method"`
	Identity string `json:"identity"`
}
type response struct {
	Schema   int       `json:"schema_version"`
	Identity string    `json:"identity"`
	Code     string    `json:"code"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
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
	listener *net.UnixListener
	path     string
	identity string
	info     os.FileInfo
	provider func(context.Context) (Snapshot, error)
	mu       sync.Mutex
	closing  bool
	conns    map[*net.UnixConn]struct{}
	workers  sync.WaitGroup
	once     sync.Once
}

// Listen must be called while the caller owns the application's receiver lock.
// It only replaces a socket proven stale by ECONNREFUSED, never an active or
// ambiguous endpoint, regular file, symlink or foreign-owned socket.
func Listen(ctx context.Context, path, identity string, provider func(context.Context) (Snapshot, error)) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) || len(path) >= 108 || len(identity) != 64 || provider == nil {
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
	return &Server{listener: l, path: path, identity: identity, info: info, provider: provider, conns: map[*net.UnixConn]struct{}{}}, nil
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
	case r.Schema != protocol || r.Method != "health":
		result.Code = "unsupported"
	case r.Identity != s.identity:
		result.Code = "identity-conflict"
	default:
		snapshot, err := s.provider(ctx)
		if err != nil {
			result.Code = "health-unavailable"
		} else {
			result.Code, result.Snapshot = "ok", &snapshot
		}
	}
	// The provider returns only the bounded, typed aggregate snapshot.
	json.NewEncoder(conn).Encode(result)
}

// Query never creates an endpoint, receiver or database. Both peers' OS UIDs
// are checked; filesystem restrictions alone are not the authentication.
func Query(ctx context.Context, path, identity string) (Snapshot, error) {
	var snapshot Snapshot
	if _, err := socketInfo(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return snapshot, ErrUnavailable
		}
		return snapshot, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return snapshot, ErrUnavailable
	}
	defer conn.Close()
	c := conn.(*net.UnixConn)
	if err := sameUser(c); err != nil {
		return snapshot, err
	}
	deadline, _ := ctx.Deadline()
	c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if json.NewEncoder(c).Encode(request{protocol, "health", identity}) != nil || c.CloseWrite() != nil {
		return snapshot, ErrUnavailable
	}
	var result response
	if decode(c, &result) != nil || result.Schema != protocol {
		return snapshot, ErrUnavailable
	}
	if result.Code == "identity-conflict" || result.Identity != identity {
		return snapshot, ErrIdentity
	}
	if result.Code != "ok" || result.Snapshot == nil {
		return snapshot, ErrUnavailable
	}
	now := time.Now().UnixMilli()
	if result.Snapshot.ObservedAtMS < now-10_000 || result.Snapshot.ObservedAtMS > now+1_000 {
		return snapshot, ErrUnavailable
	}
	return *result.Snapshot, nil
}
