package filepreview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func artifactFixture(t *testing.T, quota int64) (Options, string, *atomic.Int64) {
	t.Helper()
	f := newFixture(t)
	opts := f.opts
	opts.ArtifactRoots = []string{f.root}
	opts.ArtifactCacheQuotaBytes = quota
	opts.MaxFileBytes = 32
	now := new(atomic.Int64)
	now.Store(time.Now().Unix())
	return opts, f.root, now
}

func startArtifactFixture(t *testing.T, opts Options, now *atomic.Int64) *Service {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.now = func() time.Time { return time.Unix(now.Load(), 0) }
	if err := s.StartArtifactCache(); err != nil {
		t.Fatal(err)
	}
	return s
}

func waitForArtifact(t *testing.T, s *Service, target string, session *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := request(t, s, http.MethodGet, target, session)
		if w.Code != http.StatusAccepted {
			return w
		}
		if time.Now().After(deadline) {
			t.Fatal("artifact generation did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestArtifactLazyStableLinkAndPagination(t *testing.T) {
	opts, root, now := artifactFixture(t, 1<<20)
	s := startArtifactFixture(t, opts, now)
	content := strings.Repeat("a", artifactPageBytes-1) + "界\n<script>unsafeFixture()</script>"
	path := writeFixture(t, root, "trace.html", content)
	target := registeredURL(t, s, path)
	if w := request(t, s, http.MethodGet, target, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized request got %d", w.Code)
	}
	session := loggedIn(t, s)
	if w := request(t, s, http.MethodHead, target, session); w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("uncached HEAD got %d with %d bytes", w.Code, w.Body.Len())
	}
	cacheDir := filepath.Join(opts.Directory, "artifacts")
	if entries, err := os.ReadDir(cacheDir); err != nil || len(entries) != 0 {
		t.Fatal("registration or unauthenticated access generated an artifact", err)
	}
	var wg sync.WaitGroup
	first := make(chan *httptest.ResponseRecorder, 16)
	for range cap(first) {
		wg.Go(func() { first <- request(t, s, http.MethodGet, target, session) })
	}
	wg.Wait()
	close(first)
	accepted := 0
	for w := range first {
		if w.Code == http.StatusAccepted {
			accepted++
			if w.Header().Get("Refresh") != "2" || !strings.Contains(w.Body.String(), "<progress") {
				t.Fatal("pending preview did not show progress")
			}
		} else if w.Code != http.StatusOK {
			t.Fatalf("concurrent request got %d", w.Code)
		}
	}
	if accepted == 0 {
		t.Fatal("first visits did not start async generation")
	}
	w := waitForArtifact(t, s, target, session)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "查看源码") || strings.Contains(w.Body.String(), "unsafeFixture()") {
		t.Fatal("oversized HTML did not default to the document notice")
	}
	w = request(t, s, http.MethodGet, target+"/source", session)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "HTML 源码预览") ||
		strings.Contains(w.Body.String(), "界") || strings.Contains(w.Body.String(), "unsafeFixture()") {
		t.Fatalf("first page did not stop at the UTF-8 boundary, status %d", w.Code)
	}
	if entries, err := os.ReadDir(cacheDir); err != nil || len(entries) != 1 {
		t.Fatal("expected one published artifact after concurrent requests", err)
	}
	w = request(t, s, http.MethodGet, target+"/source?page=2", session)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<code>界") || strings.Contains(w.Body.String(), "<script>") ||
		!strings.Contains(w.Body.String(), "&lt;script&gt;unsafeFixture()&lt;/script&gt;") ||
		strings.Contains(w.Body.String(), "下载") || w.Header().Get("Content-Disposition") != "" {
		t.Fatalf("second page did not display escaped source, status %d", w.Code)
	}
	for _, suffix := range []string{"?page=0", "?page=3", "?page=2&page=1", "?page=9223372036854775807"} {
		if got := request(t, s, http.MethodGet, target+"/source"+suffix, session); got.Code != http.StatusBadRequest {
			t.Fatalf("invalid page %s got %d", suffix, got.Code)
		}
	}
	if old := request(t, s, http.MethodGet, target+"/download/"+s.mac("test", "old-version"), session); old.Code != http.StatusNotFound {
		t.Fatalf("retired download route got %d", old.Code)
	}
	relative := strings.TrimPrefix(target, s.baseURL) + "/source?page=2"
	form, csrf := csrfForm(t, s, relative)
	if login := postLogin(s, form, csrf, s.baseURL); login.Code != http.StatusSeeOther || login.Header().Get("Location") != relative {
		t.Fatal("login did not preserve the page")
	}

	writeFixture(t, root, "trace.html", strings.Repeat("replacement trace line\\n", 12))
	if again := registeredURL(t, s, path); again != target {
		t.Fatal("source edit changed the stable URL")
	}
	w = waitForArtifact(t, s, target+"/source", session)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "replacement trace line") {
		t.Fatal("source version change did not regenerate the artifact")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if deleted := request(t, s, http.MethodGet, target, session); deleted.Code != http.StatusNotFound {
		t.Fatalf("removed source was still served, status %d", deleted.Code)
	}
}

func TestArtifactPreparationReportsWrittenBytes(t *testing.T) {
	opts, root, now := artifactFixture(t, 1<<20)
	s := startArtifactFixture(t, opts, now)
	path := writeFixture(t, root, "progress.txt", strings.Repeat("a", 100))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	target := registeredURL(t, s, path)
	version := s.artifactVersion(filepath.Base(target), info)
	entry := &artifactEntry{size: info.Size(), pending: true}
	s.artifacts.mu.Lock()
	s.artifacts.entries[version] = entry
	s.artifacts.mu.Unlock()
	session := loggedIn(t, s)
	writer := artifactProgressWriter{Writer: io.Discard, copied: &entry.copied}
	for _, sample := range []struct {
		chunk int
		want  string
	}{
		{37, "37%"},
		{63, "正在完成校验并发布"},
	} {
		if _, err := writer.Write(make([]byte, sample.chunk)); err != nil {
			t.Fatal(err)
		}
		w := request(t, s, http.MethodGet, target, session)
		if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), sample.want) {
			t.Fatalf("preparation sample %q not displayed, status %d", sample.want, w.Code)
		}
	}
}

func TestArtifactSlidingExpirySurvivesRestartAndReleasesQuota(t *testing.T) {
	opts, root, now := artifactFixture(t, 100)
	s := startArtifactFixture(t, opts, now)
	first := registeredURL(t, s, writeFixture(t, root, "first.txt", strings.Repeat("a", 60)))
	second := registeredURL(t, s, writeFixture(t, root, "second.txt", strings.Repeat("b", 60)))
	session := loggedIn(t, s)
	if got := waitForArtifact(t, s, first, session); got.Code != http.StatusOK {
		t.Fatalf("first artifact got %d", got.Code)
	}
	if got := request(t, s, http.MethodGet, second, session); got.Code != http.StatusInsufficientStorage {
		t.Fatalf("quota overflow got %d", got.Code)
	}
	now.Add(int64((47 * time.Hour).Seconds()))
	if got := request(t, s, http.MethodGet, first, session); got.Code != http.StatusOK {
		t.Fatalf("pre-expiry access got %d", got.Code)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	orphan := writeFixture(t, filepath.Join(opts.Directory, "artifacts"), ".pending-"+s.mac("test", "interrupted"), "partial")
	now.Add(int64((47 * time.Hour).Seconds()))
	s = startArtifactFixture(t, opts, now)
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandoned partial artifact survived restart", err)
	}
	if got := request(t, s, http.MethodGet, second, session); got.Code != http.StatusInsufficientStorage {
		t.Fatalf("restart lost refreshed expiry, got %d", got.Code)
	}
	now.Add(int64(time.Hour.Seconds()))
	s.artifacts.mu.Lock()
	s.artifacts.purge(s.now())
	used := s.artifacts.used
	s.artifacts.mu.Unlock()
	if used != 0 {
		t.Fatal("inactive artifact remained after two days")
	}
	if got := request(t, s, http.MethodGet, first, session); got.Code != http.StatusAccepted {
		t.Fatalf("expired artifact was not regenerated, got %d", got.Code)
	}
	if got := waitForArtifact(t, s, first, session); got.Code != http.StatusOK {
		t.Fatalf("regenerated artifact got %d", got.Code)
	}
}

func TestArtifactSnapshotKeepsSmallHTMLAndChecksText(t *testing.T) {
	opts, root, now := artifactFixture(t, 2<<20)
	opts.MaxFileBytes = 1024
	s := startArtifactFixture(t, opts, now)
	session := loggedIn(t, s)
	target := registeredURL(t, s, writeFixture(t, root, "chart.html", "<p>chart fixture</p>"))
	if w := waitForArtifact(t, s, target, session); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/html") {
		t.Fatalf("small HTML did not get an interactive preview, status %d", w.Code)
	}
	if w := request(t, s, http.MethodGet, target+"/html", session); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "chart fixture") {
		t.Fatalf("small HTML frame got %d", w.Code)
	}
	for name, contents := range map[string]string{
		"binary.txt": "text\x00text", "copy.txt": strings.Repeat("\u2003", artifactBuffer/2) + testPassword + strings.Repeat("\n", artifactBuffer),
	} {
		link := registeredURL(t, s, writeFixture(t, root, name, contents))
		if w := waitForArtifact(t, s, link, session); w.Code != http.StatusNotFound {
			t.Fatalf("rejected contents became readable, status %d", w.Code)
		}
	}
}

func TestArtifactCopyValidatesUTF8AcrossChunks(t *testing.T) {
	text := strings.Repeat("a", artifactBuffer-1) + "界\n"
	out := new(bytes.Buffer)
	secret := sha256.Sum256([]byte(testPassword))
	if err := copyArtifact(context.Background(), out, strings.NewReader(text), int64(len(text)), secret); err != nil || out.String() != text {
		t.Fatal("valid rune across a chunk boundary was lost", err)
	}
	invalid := strings.Repeat("a", artifactBuffer-1) + "\xe7b"
	if err := copyArtifact(context.Background(), io.Discard, strings.NewReader(invalid), int64(len(invalid)), secret); !errors.Is(err, errNotPreviewable) {
		t.Fatal("invalid rune across chunk boundary was accepted", err)
	}
}

func TestEightMiBArtifactOpensRenderedViewWithSeparateSource(t *testing.T) {
	opts, root, now := artifactFixture(t, 32<<20)
	opts.MaxFileBytes = 8 << 20
	s := startArtifactFixture(t, opts, now)
	session := loggedIn(t, s)
	for _, fixture := range []struct{ name, start, end, rendered string }{
		{"report.html", "<h1>HTML fixture heading</h1><p>", "</p><strong>HTML tail</strong>", "HTML tail"},
		{"report.md", "# Markdown fixture heading\n\n", "\n\n**Markdown tail**\n", "<strong>Markdown tail</strong>"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			content := fixture.start + strings.Repeat("x", (8<<20)-len(fixture.start)-len(fixture.end)) + fixture.end
			target := registeredURL(t, s, writeFixture(t, root, fixture.name, content))
			w := waitForArtifact(t, s, target, session)
			body := w.Body.String()
			if w.Code != http.StatusOK || !strings.Contains(body, "查看源码") || strings.Contains(body, `id="L1"`) ||
				!strings.Contains(w.Header().Get("Content-Security-Policy"), "'sha256-"+sourceFragmentHash()+"'") {
				t.Fatal("default view did not separate source or preserve old line links")
			}
			if isHTMLFile(fixture.name) {
				if w.Body.Len() > 32<<10 || !strings.Contains(body, `sandbox="allow-scripts"`) {
					t.Fatal("HTML preview shell included report bytes or lost the sandbox")
				}
				w = request(t, s, http.MethodGet, target+"/html", session)
				if w.Header().Get("Content-Security-Policy") != htmlDocumentCSP {
					t.Fatal("HTML report did not retain the sandbox response policy")
				}
			}
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), fixture.rendered) {
				t.Fatal("rendered document was incomplete")
			}
			source := request(t, s, http.MethodGet, target+"/source", session)
			if source.Code != http.StatusOK || !strings.Contains(source.Body.String(), `id="L1"`) ||
				!strings.Contains(source.Body.String(), "返回渲染视图") || source.Header().Get("Content-Security-Policy") != previewCSP {
				t.Fatal("separate source view is unavailable or lost line anchors")
			}
		})
	}
}

func TestArtifactTwoGiBPreviewAndHardLimit(t *testing.T) {
	opts, root, now := artifactFixture(t, 4<<30)
	s := startArtifactFixture(t, opts, now)
	source := writeFixture(t, root, "maximum.txt", "text")
	if err := os.Truncate(source, maxArtifactBytes); err != nil {
		t.Fatal(err)
	}
	target := registeredURL(t, s, source)
	id := filepath.Base(target)
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	version := s.artifactVersion(id, info)
	// Seed a sparse, private snapshot to exercise previewing the final page
	// without making the unit suite write two GiB of real data to disk.
	cachePath := writeFixture(t, filepath.Join(opts.Directory, "artifacts"), version+".artifact", "text")
	if err := os.Truncate(cachePath, maxArtifactBytes); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = startArtifactFixture(t, opts, now)
	session := loggedIn(t, s)
	w := request(t, s, http.MethodGet, target+"?page=32768", session)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "第 32768 / 32768 页") ||
		!strings.Contains(w.Body.String(), "2147483648") || w.Body.Len() > 4*artifactPageBytes {
		t.Fatalf("2 GiB preview got %d with %d bytes", w.Code, w.Body.Len())
	}
	if err := os.Truncate(source, maxArtifactBytes+1); err != nil {
		t.Fatal(err)
	}
	body := "[file](" + source + ")"
	out, err := s.Rewrite(context.Background(), body)
	if err != nil || out != body {
		t.Fatal("source over 2 GiB was registered", err)
	}
}
