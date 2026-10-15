// Package filepreview registers explicit local text files and serves authenticated,
// read-only previews. It never exposes a filesystem path in a request parameter.
package filepreview

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxFileBytes = 1 << 20
	maxFileBytes        = 10 << 20
	sessionLifetime     = 7 * 24 * time.Hour
	csrfLifetime        = 15 * time.Minute
	sessionCookie       = "codex_filepreview_session"
	csrfCookie          = "codex_filepreview_csrf"
)

var (
	errClosed         = errors.New("file preview: service closed")
	errRegistry       = errors.New("file preview: private registry unavailable")
	errFileIO         = errors.New("file preview: file could not be read")
	errNotPreviewable = errors.New("file preview: file is not previewable")
)

// Options defines the public origin and the only directories eligible for preview.
// BaseURL must be an http(s) origin, without a path prefix, query or fragment.
// Directory holds private per-file manifests; SecretFile contains the login secret.
type Options struct {
	BaseURL      string
	AllowedRoots []string
	Directory    string
	SecretFile   string
	MaxFileBytes int64
}

type allowedRoot struct {
	path    string
	aliases []string
	handle  *os.Root
}

// Service implements http.Handler. Close must follow shutdown of its HTTP server.
// Multiple services may share Directory; registration uses independent atomic
// manifests, so a CLI registration is immediately visible to a running server.
type Service struct {
	mu            sync.RWMutex
	closed        bool
	baseURL       string
	scheme        string
	host          string
	key           [32]byte
	roots         []*allowedRoot
	rootsByPath   map[string]*allowedRoot
	directory     *os.Root
	directoryPath string
	secretPath    string
	maxBytes      int64
	now           func() time.Time
}

// New validates configuration and opens stable os.Root handles. It does not start
// a listener, change configuration, or read any workspace file contents.
func New(opts Options) (_ *Service, err error) {
	u, err := url.Parse(opts.BaseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.Contains(opts.BaseURL, "#") {
		return nil, errors.New("file preview: base URL must be an http(s) origin")
	}
	host, ok := canonicalHost(u.Host, u.Scheme)
	if !ok {
		return nil, errors.New("file preview: base URL host is invalid")
	}
	if opts.MaxFileBytes == 0 {
		opts.MaxFileBytes = defaultMaxFileBytes
	}
	if opts.MaxFileBytes < 1 || opts.MaxFileBytes > maxFileBytes {
		return nil, errors.New("file preview: file size limit must be between 1 byte and 10 MiB")
	}
	if len(opts.AllowedRoots) == 0 || opts.Directory == "" || opts.SecretFile == "" {
		return nil, errors.New("file preview: allowed roots, private directory and secret file are required")
	}
	key, secretPath, err := readSecret(opts.SecretFile)
	if err != nil {
		return nil, err
	}
	s := &Service{baseURL: strings.TrimSuffix(opts.BaseURL, "/"), scheme: u.Scheme, host: host,
		key: key, maxBytes: opts.MaxFileBytes, rootsByPath: make(map[string]*allowedRoot),
		secretPath: secretPath, now: time.Now}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	for _, name := range opts.AllowedRoots {
		if !filepath.IsAbs(name) {
			return nil, errors.New("file preview: allowed roots must be absolute directories")
		}
		alias := filepath.Clean(name)
		canonical, e := filepath.EvalSymlinks(alias)
		if e != nil || canonical == string(filepath.Separator) {
			return nil, errors.New("file preview: allowed root unavailable")
		}
		if existing := s.rootsByPath[canonical]; existing != nil {
			existing.aliases = append(existing.aliases, alias)
			continue
		}
		handle, e := os.OpenRoot(canonical)
		if e != nil {
			return nil, errors.New("file preview: allowed root unavailable")
		}
		root := &allowedRoot{path: canonical, aliases: []string{canonical, alias}, handle: handle}
		s.roots = append(s.roots, root)
		s.rootsByPath[canonical] = root
	}
	s.directory, s.directoryPath, err = openRegistry(opts.Directory)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Close releases directory handles. It is safe to call more than once.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	failed := false
	for _, root := range s.roots {
		if root.handle.Close() != nil {
			failed = true
		}
	}
	if s.directory != nil && s.directory.Close() != nil {
		failed = true
	}
	if failed {
		return errors.New("file preview: directory handles could not be closed")
	}
	return nil
}

func canonicalHost(host, scheme string) (string, bool) {
	u, err := url.Parse(scheme + "://" + host)
	if err != nil || u.Host != host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" || strings.ContainsAny(hostname, "%/\\ \t\r\n") {
		return "", false
	}
	if net.ParseIP(hostname) == nil {
		for _, c := range hostname {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
				return "", false
			}
		}
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(host, ":") {
			return "", false
		}
		port = "80"
		if scheme == "https" {
			port = "443"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", false
	}
	return net.JoinHostPort(hostname, strconv.Itoa(n)), true
}

func (s *Service) signedToken(purpose string, lifetime time.Duration) (string, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", errors.New("file preview: authentication unavailable")
	}
	payload := strconv.FormatInt(s.now().Add(lifetime).Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(nonce[:])
	return payload + "." + s.mac(purpose, payload), nil
}

func (s *Service) mac(purpose, value string) string {
	h := hmac.New(sha256.New, s.key[:])
	_, _ = io.WriteString(h, "codex-filepreview-v1\x00"+purpose+"\x00"+value)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (s *Service) validToken(token, purpose string, lifetime time.Duration) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 160 || !validID(parts[1]) {
		return false
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	now := s.now().Unix()
	if err != nil || expires <= now || expires > now+int64(lifetime/time.Second)+60 {
		return false
	}
	want := s.mac(purpose, parts[0]+"."+parts[1])
	return subtle.ConstantTimeCompare([]byte(parts[2]), []byte(want)) == 1
}

func singleCookie(r *http.Request, name string) string {
	cookies := r.CookiesNamed(name)
	if len(cookies) != 1 {
		return ""
	}
	return cookies[0].Value
}

func (s *Service) cookie(name, value string, duration time.Duration) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true,
		Secure: s.scheme == "https", SameSite: http.SameSiteLaxMode,
		MaxAge: int(duration / time.Second)}
}

// ServeHTTP serves only login and registered file previews. Requests and errors
// deliberately carry no server paths or file contents in diagnostic messages.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	// no-referrer makes a browser's ordinary form POST use Origin: null.
	// Keep the origin for our own login while suppressing cross-origin referrers.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'none'; img-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	gotHost, ok := canonicalHost(r.Host, s.scheme)
	if !ok || gotHost != s.host || (r.URL.IsAbs() && (r.URL.Scheme != s.scheme || r.URL.Host != r.Host)) {
		writeStatus(w, r, http.StatusForbidden)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" {
		writeStatus(w, r, http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		writeStatus(w, r, http.StatusServiceUnavailable)
		return
	}
	s.mu.RUnlock()
	if r.URL.Path == "/login" {
		s.serveLogin(w, r)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/f/")
	if r.URL.Path != "/" && (!strings.HasPrefix(r.URL.Path, "/f/") || !validID(id)) {
		writeStatus(w, r, http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPost {
		// The login form posts to its current URL so the browser preserves #Lx
		// through authentication. This route only authenticates; it never writes
		// the registered file or reads its contents before authentication.
		s.serveLogin(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, POST")
		writeStatus(w, r, http.StatusMethodNotAllowed)
		return
	}
	if !s.validToken(singleCookie(r, sessionCookie), "session", sessionLifetime) {
		s.showLogin(w, r, r.URL.Path, "", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/" {
		writePage(w, r, http.StatusOK, "文件预览", "请通过已分享的文件链接打开预览。")
		return
	}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		writeStatus(w, r, http.StatusServiceUnavailable)
		return
	}
	record, err := s.loadRecord(id)
	var file previewFile
	if err == nil {
		root := s.rootsByPath[record.Root]
		if root == nil {
			err = errNotPreviewable
		} else {
			file, err = s.readFile(root, record.Relative)
		}
	}
	s.mu.RUnlock()
	if err != nil {
		// A missing, revoked or no longer readable registration has one response.
		writeStatus(w, r, http.StatusNotFound)
		return
	}
	page, err := renderPreview(file)
	if err != nil {
		writeStatus(w, r, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(page)
	}
}

func (s *Service) serveLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		s.showLogin(w, r, "/", "", http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, HEAD, POST")
		writeStatus(w, r, http.StatusMethodNotAllowed)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" || !s.sameOrigin(r) {
		writeStatus(w, r, http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		writeStatus(w, r, http.StatusBadRequest)
		return
	}
	for key, vals := range r.PostForm {
		if (key != "password" && key != "csrf" && key != "next") || len(vals) != 1 {
			writeStatus(w, r, http.StatusBadRequest)
			return
		}
	}
	next := r.PostForm.Get("next")
	if next == "" {
		next = "/"
	}
	if !validNext(next) {
		writeStatus(w, r, http.StatusBadRequest)
		return
	}
	if r.URL.Path != "/login" && next != r.URL.Path {
		writeStatus(w, r, http.StatusBadRequest)
		return
	}
	csrf := singleCookie(r, csrfCookie)
	if !s.validToken(csrf, "csrf", csrfLifetime) || subtle.ConstantTimeCompare([]byte(csrf), []byte(r.PostForm.Get("csrf"))) != 1 {
		writeStatus(w, r, http.StatusForbidden)
		return
	}
	password := sha256.Sum256([]byte(r.PostForm.Get("password")))
	if subtle.ConstantTimeCompare(password[:], s.key[:]) != 1 {
		s.showLogin(w, r, next, "口令不正确。", http.StatusUnauthorized)
		return
	}
	token, err := s.signedToken("session", sessionLifetime)
	if err != nil {
		writeStatus(w, r, http.StatusServiceUnavailable)
		return
	}
	http.SetCookie(w, s.cookie(sessionCookie, token, sessionLifetime))
	http.SetCookie(w, s.cookie(csrfCookie, "", -time.Second))
	// next is restricted to our fixed routes; no caller-supplied origin is used.
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func validNext(next string) bool {
	return next == "/" || strings.HasPrefix(next, "/f/") && validID(strings.TrimPrefix(next, "/f/"))
}

func (s *Service) sameOrigin(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	for _, name := range []string{"Origin", "Referer"} {
		values := r.Header.Values(name)
		if len(values) > 1 {
			return false
		}
		if len(values) == 0 {
			continue // The signed double-submit token also protects older clients.
		}
		u, err := url.Parse(values[0])
		if err != nil || u.Scheme != s.scheme || u.User != nil || u.Opaque != "" ||
			(name == "Origin" && (u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(values[0], "#"))) {
			return false
		}
		host, ok := canonicalHost(u.Host, u.Scheme)
		if !ok || host != s.host {
			return false
		}
	}
	return true
}

func writeStatus(w http.ResponseWriter, r *http.Request, status int) {
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = fmt.Fprintln(w, http.StatusText(status))
	}
}
