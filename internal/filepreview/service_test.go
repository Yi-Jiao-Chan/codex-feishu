package filepreview

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testPassword = "abcde0123456789abcde0123456789abcde0123456789abcde0123456789abcdef"

type fixture struct {
	s    *Service
	opts Options
	root string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "workspace")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "preview.key")
	if err := os.WriteFile(secret, []byte(testPassword+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := Options{BaseURL: "http://127.0.0.1:8427", AllowedRoots: []string{root},
		Directory: filepath.Join(dir, ".state", "preview"), SecretFile: secret}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &fixture{s: s, opts: opts, root: root}
}

func writeFixture(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

var previewURLPattern = regexp.MustCompile(`https?://[^\s<>]+/f/[A-Za-z0-9_-]{43}(?:#L[0-9]+)?`)

func registeredURL(t *testing.T, s *Service, path string) string {
	t.Helper()
	result, err := s.Rewrite(context.Background(), "[file](<"+path+">)")
	if err != nil {
		t.Fatal(err)
	}
	target := previewURLPattern.FindString(result)
	if target == "" {
		t.Fatal("file was not registered")
	}
	return strings.TrimRight(target, ")")
}

func request(t *testing.T, s *Service, method, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func csrfForm(t *testing.T, s *Service, next string) (url.Values, *http.Cookie) {
	t.Helper()
	w := request(t, s, http.MethodGet, s.baseURL+next, nil)
	if w.Code != http.StatusOK && w.Code != http.StatusUnauthorized {
		t.Fatalf("login page status = %d", w.Code)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == csrfCookie {
			cookie = c
		}
	}
	if cookie == nil || !strings.Contains(w.Body.String(), `name="csrf" value="`+cookie.Value+`"`) {
		t.Fatal("signed form token not found")
	}
	return url.Values{"password": {testPassword}, "csrf": {cookie.Value}, "next": {next}}, cookie
}

func postLogin(s *Service, form url.Values, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, s.baseURL+"/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func loggedIn(t *testing.T, s *Service) *http.Cookie {
	t.Helper()
	form, csrf := csrfForm(t, s, "/")
	w := postLogin(s, form, csrf, s.baseURL)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("login status = %d, location = %q", w.Code, w.Header().Get("Location"))
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	t.Fatal("session cookie missing")
	return nil
}

func TestLoginSessionAndReadOnlyPreview(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, "main.go", "package fixture\n// safe current source\n")
	target := registeredURL(t, f.s, path)
	w := request(t, f.s, http.MethodGet, target, nil)
	if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "safe current source") {
		t.Fatal("unauthenticated request exposed a file")
	}
	if w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatal("login page must preserve a browser's same-origin form POST Origin")
	}
	parsed, _ := url.Parse(target)
	form, csrf := csrfForm(t, f.s, parsed.Path)
	form.Set("password", "wrong")
	w = postLogin(f.s, form, csrf, f.s.baseURL)
	if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "wrong") {
		t.Fatal("incorrect password was accepted or reflected")
	}
	form, csrf = csrfForm(t, f.s, parsed.Path)
	w = postLogin(f.s, form, csrf, f.s.baseURL)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != parsed.Path {
		t.Fatal("login did not return to the registered file")
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil || session.Secure || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/" || session.MaxAge <= 0 {
		t.Fatal("incorrect HTTP session attributes")
	}
	w = request(t, f.s, http.MethodGet, target, session)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "safe current source") ||
		!strings.Contains(w.Body.String(), `id="L2"`) || !strings.Contains(w.Body.String(), `href="#L2"`) ||
		!strings.Contains(w.Body.String(), "修改时间") || strings.Contains(w.Body.String(), f.root) {
		t.Fatal("preview content, line anchors or metadata missing, or server path leaked")
	}
	if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'none'") {
		t.Fatal("security response headers missing")
	}
	if w = request(t, f.s, http.MethodHead, target, session); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatal("HEAD did not return metadata only")
	}
	if w = request(t, f.s, http.MethodPut, target, session); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("file route accepted a write method")
	}
	newTime := time.Unix(1728000000, 0)
	if err := os.WriteFile(path, []byte("updated source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, newTime, newTime); err != nil {
		t.Fatal(err)
	}
	w = request(t, f.s, http.MethodGet, target, session)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "updated source") || !strings.Contains(w.Body.String(), newTime.UTC().Format(time.RFC3339)) {
		t.Fatal("preview did not read the latest content and mtime")
	}
	if w = request(t, f.s, http.MethodGet, f.opts.BaseURL+"/", session); strings.Contains(w.Body.String(), "main.go") {
		t.Fatal("root route listed registered files")
	}
}

func TestLoginCSRFHostAndRedirectChecks(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name, next, origin string
		change             func(url.Values, **http.Cookie)
		status             int
	}{
		{"missing cookie", "/", "", func(_ url.Values, c **http.Cookie) { *c = nil }, http.StatusForbidden},
		{"missing token", "/", "", func(v url.Values, _ **http.Cookie) { v.Del("csrf") }, http.StatusForbidden},
		{"forged token", "/", "", func(v url.Values, c **http.Cookie) { (*c).Value += "a"; v.Set("csrf", (*c).Value) }, http.StatusForbidden},
		{"cross origin", "/", "http://evil.invalid", nil, http.StatusForbidden},
		{"null origin", "/", "null", nil, http.StatusForbidden},
		{"external next", "https://evil.invalid/", "", nil, http.StatusBadRequest},
		{"network next", "//evil.invalid/", "", nil, http.StatusBadRequest},
		{"encoded next", "/%2f%2fevil.invalid/", "", nil, http.StatusBadRequest},
		{"query next", "/?next=https://evil.invalid", "", nil, http.StatusBadRequest},
		{"duplicate field", "/", "", func(v url.Values, _ **http.Cookie) { v.Add("password", testPassword) }, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form, cookie := csrfForm(t, f.s, "/")
			form.Set("next", tc.next)
			if tc.change != nil {
				tc.change(form, &cookie)
			}
			w := postLogin(f.s, form, cookie, tc.origin)
			if w.Code != tc.status || w.Header().Get("Location") != "" {
				t.Fatalf("status = %d, want %d; redirect = %q", w.Code, tc.status, w.Header().Get("Location"))
			}
		})
	}
	for _, target := range []string{"http://evil.invalid/", f.opts.BaseURL + "/login?password=example", f.opts.BaseURL + "/f/%2e%2e"} {
		w := request(t, f.s, http.MethodGet, target, nil)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusForbidden {
			t.Fatalf("unsafe request status = %d", w.Code)
		}
	}
	form, cookie := csrfForm(t, f.s, "/")
	r := httptest.NewRequest(http.MethodPost, f.opts.BaseURL+"/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("cross-site form submission accepted")
	}
}

func TestHTTPSCookiesAndExpiredSession(t *testing.T) {
	f := newFixture(t)
	opts := f.opts
	opts.BaseURL = "https://preview.example.invalid"
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	clock := time.Unix(1800000000, 0)
	s.now = func() time.Time { return clock }
	cookie := loggedIn(t, s)
	if !cookie.Secure || !cookie.HttpOnly {
		t.Fatal("HTTPS cookie did not require a secure connection")
	}
	clock = clock.Add(sessionLifetime + time.Second)
	if w := request(t, s, http.MethodGet, opts.BaseURL+"/", cookie); w.Code != http.StatusUnauthorized {
		t.Fatal("expired session was accepted")
	}
	cookie.Value += "x"
	if w := request(t, s, http.MethodGet, opts.BaseURL+"/", cookie); w.Code != http.StatusUnauthorized {
		t.Fatal("tampered session was accepted")
	}
}

func TestAuthenticatedMarkdownIsSafe(t *testing.T) {
	f := newFixture(t)
	content := "# Safe title\n\n**strong**\n\n| A | B |\n| - | - |\n| x | y |\n\n" +
		"<script>alert('raw')</script>\n\n<img src=x onerror=alert(1)>\n\n" +
		"[script](javascript:alert(1)) [entity](java&#x73;cript:alert(2)) [data](data:text/html,test) <javascript:alert(3)> <ftp://example.invalid/file>\n\n" +
		"[website](https://example.invalid/page?x=1&y=2) [local](/etc/passwd)\n\n" +
		"![remote](https://tracker.invalid/pixel)\n"
	path := writeFixture(t, f.root, "report.md", content)
	target := registeredURL(t, f.s, path)
	w := request(t, f.s, http.MethodGet, target, loggedIn(t, f.s))
	if w.Code != http.StatusOK {
		t.Fatalf("markdown status = %d", w.Code)
	}
	body := w.Body.String()
	for _, active := range []string{"<script", "<img", `href="javascript:`, `href="java&#`, `href="data:`, `href="ftp:`, `href="/etc/passwd"`} {
		if strings.Contains(body, active) {
			t.Fatalf("unsafe HTML was rendered: %s", active)
		}
	}
	for _, expected := range []string{"<h1>Safe title</h1>", "<strong>strong</strong>", "<table>", `href="https://example.invalid/page?x=1&amp;y=2"`, "[图片：remote]", `id="L1"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("safe markdown feature missing: %s", expected)
		}
	}
}

func TestActualHTTPBrowserFlow(t *testing.T) {
	f := newFixture(t)
	server := httptest.NewUnstartedServer(nil)
	opts := f.opts
	opts.BaseURL = "http://" + server.Listener.Addr().String()
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server.Config.Handler = s
	server.Start()
	defer server.Close()
	path := writeFixture(t, f.root, "hello.txt", "browser fixture")
	target := registeredURL(t, s, path)
	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusUnauthorized || len(response.Cookies()) != 1 {
		t.Fatal("real HTTP request did not receive the login flow")
	}
}
