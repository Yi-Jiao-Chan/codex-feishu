package filepreview

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRegistrySurvivesRestartAndSecretRevokesURLs(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, "report.md", "# persistent fixture")
	target := registeredURL(t, f.s, path)
	session := loggedIn(t, f.s)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if again := registeredURL(t, s, path); again != target {
		t.Fatal("stable ID changed on restart")
	}
	if w := request(t, s, http.MethodGet, target, session); w.Code != http.StatusOK {
		t.Fatal("persisted URL or session failed after restart")
	}
	_ = s.Close()
	newPassword := strings.Repeat("b", 64)
	if err := os.WriteFile(f.opts.SecretFile, []byte(newPassword+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if w := request(t, s, http.MethodGet, target, session); w.Code != http.StatusUnauthorized {
		t.Fatal("secret rotation did not revoke the old session")
	}
	form, csrf := csrfForm(t, s, "/")
	form.Set("password", newPassword)
	w := postLogin(s, form, csrf, s.baseURL)
	var newSession *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			newSession = c
		}
	}
	if w.Code != http.StatusSeeOther || newSession == nil {
		t.Fatal("new secret was not usable")
	}
	if w = request(t, s, http.MethodGet, target, newSession); w.Code != http.StatusNotFound {
		t.Fatal("old URL remained valid under a rotated secret")
	}
	newTarget := registeredURL(t, s, path)
	if newTarget == target || request(t, s, http.MethodGet, newTarget, newSession).Code != http.StatusOK {
		t.Fatal("rotated secret did not create a usable replacement URL")
	}
}

func TestConcurrentServicesDoNotLoseRegistrations(t *testing.T) {
	f := newFixture(t)
	other, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var paths []string
	for i := range 20 {
		paths = append(paths, writeFixture(t, f.root, fmt.Sprintf("source%d.go", i), "synthetic source"))
	}
	var wg sync.WaitGroup
	errors := make(chan error, len(paths)*2)
	for _, path := range paths {
		for _, service := range []*Service{f.s, other} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := service.Rewrite(context.Background(), "[file]("+path+")")
				errors <- err
			}()
		}
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	session := loggedIn(t, f.s)
	for _, path := range paths {
		id := other.recordID(fileRecord{Version: 1, Root: f.root, Relative: filepath.Base(path)})
		if w := request(t, f.s, http.MethodGet, f.opts.BaseURL+"/f/"+id, session); w.Code != http.StatusOK {
			t.Fatal("running service could not see concurrent CLI registration")
		}
	}
	info, err := os.Stat(f.opts.Directory)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("registry directory is not private")
	}
	entries, err := os.ReadDir(f.opts.Directory)
	if err != nil || len(entries) != len(paths) {
		t.Fatal("registry contains missing entries or incomplete temp files")
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatal("registry record is not a private regular file")
		}
	}
}

func TestRegisteredFileCannotEscapeByReplacement(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, "sub/source.go", "original inside source")
	target := registeredURL(t, f.s, path)
	session := loggedIn(t, f.s)
	outside := writeFixture(t, t.TempDir(), "source.go", "outside must never appear")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	w := request(t, f.s, http.MethodGet, target, session)
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "outside") {
		t.Fatal("symlink replacement escaped the registered root")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	w = request(t, f.s, http.MethodGet, target, session)
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "outside") {
		t.Fatal("ancestor symlink replacement escaped the registered root")
	}
	unknown := f.opts.BaseURL + "/f/" + strings.Repeat("A", 43)
	if w := request(t, f.s, http.MethodGet, unknown, session); w.Code != http.StatusNotFound {
		t.Fatal("unregistered ID did not return the same unavailable response")
	}
	for _, suffix := range []string{"/f/../source.go", "/f/" + strings.Repeat("A", 43) + "/../../source.go", "/source.go"} {
		if w := request(t, f.s, http.MethodGet, f.opts.BaseURL+suffix, session); w.Code != http.StatusNotFound {
			t.Fatal("a path-shaped request was accepted")
		}
	}
}

func TestSymlinkSwapWhileReadingNeverReturnsOutsideContent(t *testing.T) {
	f := newFixture(t)
	writeFixture(t, f.root, "sub/source.go", "inside")
	outside := t.TempDir()
	writeFixture(t, outside, "source.go", "outside")
	root := f.s.roots[0]
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			_ = os.Rename(filepath.Join(f.root, "sub"), filepath.Join(f.root, "held"))
			_ = os.Symlink(outside, filepath.Join(f.root, "sub"))
			_ = os.Remove(filepath.Join(f.root, "sub"))
			_ = os.Rename(filepath.Join(f.root, "held"), filepath.Join(f.root, "sub"))
		}
	}()
	for range 200 {
		file, err := f.s.readFile(root, "sub/source.go")
		if err == nil && string(file.content) != "inside" {
			t.Error("concurrent path replacement read outside contents")
			break
		}
	}
	wg.Wait()
}

func TestRegistryAndSecretRefuseSymlinksAndInsecurePermissions(t *testing.T) {
	f := newFixture(t)
	for _, mutate := range []func() func(){
		func() func() {
			_ = os.Chmod(f.opts.SecretFile, 0644)
			return func() { _ = os.Chmod(f.opts.SecretFile, 0600) }
		},
		func() func() {
			_ = os.Chmod(f.opts.Directory, 0755)
			return func() { _ = os.Chmod(f.opts.Directory, 0700) }
		},
	} {
		undo := mutate()
		s, err := New(f.opts)
		undo()
		if err == nil {
			_ = s.Close()
			t.Fatal("insecure private file/directory permissions were accepted")
		}
	}
	secretAlias := filepath.Join(filepath.Dir(f.opts.SecretFile), "alias.key")
	if err := os.Symlink(f.opts.SecretFile, secretAlias); err != nil {
		t.Fatal(err)
	}
	opts := f.opts
	opts.SecretFile = secretAlias
	if s, err := New(opts); err == nil {
		_ = s.Close()
		t.Fatal("secret symlink was accepted")
	}
	registryAlias := filepath.Join(filepath.Dir(f.opts.Directory), "alias")
	if err := os.Symlink(f.opts.Directory, registryAlias); err != nil {
		t.Fatal(err)
	}
	opts = f.opts
	opts.Directory = registryAlias
	if s, err := New(opts); err == nil {
		_ = s.Close()
		t.Fatal("registry symlink was accepted")
	}
	path := writeFixture(t, f.root, "public.go", "public")
	id := f.s.recordID(fileRecord{Version: 1, Root: f.root, Relative: "public.go"})
	if err := os.Symlink(f.opts.SecretFile, filepath.Join(f.opts.Directory, id+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Rewrite(context.Background(), "[file]("+path+")"); err == nil {
		t.Fatal("registry record symlink was accepted")
	}
}

func TestSelfPostingLoginPreservesFileRoute(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, "source.go", "synthetic source")
	target := registeredURL(t, f.s, path)
	u, _ := url.Parse(target)
	form, csrf := csrfForm(t, f.s, u.Path)
	// A real browser retains #L42 locally; the HTTP request excludes it.
	w := request(t, f.s, http.MethodGet, target, nil)
	if !strings.Contains(w.Body.String(), `<form method="post" action="">`) {
		t.Fatal("login does not post to its current URL including its browser fragment")
	}
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", f.s.baseURL)
	r.AddCookie(csrf)
	w = httptest.NewRecorder()
	f.s.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != u.Path {
		t.Fatal("self-posting login did not redirect to the same file route")
	}
	if data, _ := os.ReadFile(path); string(data) != "synthetic source" {
		t.Fatal("login mutated the registered file")
	}
}

func TestConfigurationRejectsBroadRootsAndNonOrigins(t *testing.T) {
	f := newFixture(t)
	for _, base := range []string{"http://user:pass@127.0.0.1:8427", "http://127.0.0.1:8427/prefix", "http://127.0.0.1:8427?", "http://127.0.0.1:8427#", "http://127.0.0.1:8427/#fragment", "file:///tmp", "http://127.0.0.1:0"} {
		opts := f.opts
		opts.BaseURL = base
		if service, err := New(opts); err == nil {
			_ = service.Close()
			t.Fatal("URL outside the origin-only contract was accepted")
		}
	}
	opts := f.opts
	opts.AllowedRoots = []string{"/"}
	if service, err := New(opts); err == nil {
		_ = service.Close()
		t.Fatal("filesystem-wide allowed root was accepted")
	}
	for _, contents := range []string{"short", " " + testPassword, testPassword + " ", testPassword + "\n\n", testPassword + "\r", strings.Repeat("x", 257)} {
		if err := os.WriteFile(f.opts.SecretFile, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if service, err := New(f.opts); err == nil {
			_ = service.Close()
			t.Fatal("invalid secret encoding was accepted")
		}
	}
	if err := os.WriteFile(f.opts.SecretFile, []byte(testPassword+"\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	service, err := New(f.opts)
	if err != nil {
		t.Fatal("one CRLF line ending was not accepted")
	}
	_ = service.Close()
}
