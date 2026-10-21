package filepreview

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
)

func visualizationMarkup(path string) string {
	payload, _ := json.Marshal(map[string]string{"path": path})
	return "\ue200visualize\ue202" + string(payload) + "\ue201"
}

func TestVisualizationUsesDurableAuthenticatedStaticPreview(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, "图表 (草稿).html", "<h1>synthetic chart</h1><script>alert(1)</script>")
	body := "结果：\n\n" + visualizationMarkup(path)
	got, err := f.s.Rewrite(context.Background(), body)
	if err != nil || strings.Contains(got, path) || strings.Contains(got, "\ue200") ||
		!strings.Contains(got, "查看可视化 · 静态预览") || !strings.Contains(got, "交互版请回 Codex 查看") {
		t.Fatalf("visualization was not presented as a static preview: %q, %v", got, err)
	}
	target := strings.TrimRight(previewURLPattern.FindString(got), ")")
	if target == "" || request(t, f.s, http.MethodGet, target, nil).Code != http.StatusUnauthorized {
		t.Fatal("visualization bypassed preview authentication")
	}
	session := loggedIn(t, f.s)
	if w := request(t, f.s, http.MethodGet, target, session); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "synthetic chart") || !strings.Contains(w.Body.String(), `sandbox=""`) {
		t.Fatal("visualization did not use the existing isolated HTML preview")
	}
	if again, err := f.s.Rewrite(context.Background(), got); err != nil || again != got {
		t.Fatal("rendered visualization changed on repeated preparation")
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if again, err := restarted.Rewrite(context.Background(), body); err != nil || again != got ||
		request(t, restarted, http.MethodGet, target, session).Code != http.StatusOK {
		t.Fatal("visualization link or authentication was not stable after restart")
	}
}

func TestVisualizationSharesExistingFileLink(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, "chart.html", "<p>chart</p>")
	target := registeredURL(t, f.s, path)
	marker := visualizationMarkup(path)
	for _, link := range []string{"[查看方案](" + path + ")", "[查看方案](" + target + ")", "[查看方案](" + path + "#L2)"} {
		for _, body := range []string{link + "\n\n" + marker, marker + "\n\n" + link} {
			got, err := f.s.Rewrite(context.Background(), body)
			if err != nil || strings.Count(got, target) != 1 || strings.Contains(got, "\ue200") ||
				strings.Contains(got, "查看可视化") || !strings.Contains(got, "查看方案") {
				t.Fatalf("duplicate visualization entry remained: %q, %v", got, err)
			}
		}
	}
	got, err := f.s.Rewrite(context.Background(), marker+"\n\n"+marker)
	if err != nil || strings.Count(got, target) != 1 {
		t.Fatal("duplicate visualization markers produced duplicate links")
	}
	// A link shown as source code is not a usable entry for the visualization.
	body := "`[source](" + target + ")`\n\n" + marker
	if got, err := f.s.Rewrite(context.Background(), body); err != nil || strings.Count(got, target) != 2 {
		t.Fatal("a code example suppressed the visualization entry")
	}
}

func TestVisualizationPayloadIsNotParsedAsMarkdown(t *testing.T) {
	f := newFixture(t)
	ordinary := writeFixture(t, f.root, "report.md", "# synthetic report")
	path := writeFixture(t, f.root, filepath.Join("[report]("+ordinary+")", "chart.html"), "<p>actual chart</p>")
	got, err := f.s.Rewrite(context.Background(), visualizationMarkup(path))
	if err != nil || !strings.Contains(got, "/f/") || strings.Contains(got, codexclient.VisualizationFallback) {
		t.Fatalf("Markdown inside a visualization path changed its file identity: %q, %v", got, err)
	}
	// A link consumed with a malformed envelope must not suppress a later,
	// valid visualization of the same file.
	simple := writeFixture(t, f.root, "chart.html", "<p>another chart</p>")
	payload, _ := json.Marshal(map[string]string{"path": simple, "note": "[not visible](" + simple + ")"})
	malformed := "\ue200visualize\ue202" + string(payload) + "\ue201"
	got, err = f.s.Rewrite(context.Background(), malformed+"\n\n"+visualizationMarkup(simple))
	if err != nil || strings.Count(got, "/f/") != 1 || strings.Count(got, codexclient.VisualizationFallback) != 1 {
		t.Fatalf("a consumed payload suppressed the usable visualization: %q, %v", got, err)
	}
}

func TestVisualizationKeepsExplicitRootAndFileGuards(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, ".codex/visualizations/day/thread/chart.html", "<p>chart</p>")
	opts := f.opts
	opts.AllowedRoots = []string{"/", f.root}
	broad, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer broad.Close()
	if got, err := broad.Rewrite(context.Background(), visualizationMarkup(path)); err != nil || got != codexclient.VisualizationFallback {
		t.Fatal("filesystem root implicitly authorized hidden visualizations")
	}
	opts.AllowedRoots = append(opts.AllowedRoots, filepath.Join(f.root, ".codex", "visualizations"))
	narrow, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer narrow.Close()
	if got, err := narrow.Rewrite(context.Background(), visualizationMarkup(path)); err != nil || !strings.Contains(got, "/f/") {
		t.Fatal("explicit visualization root did not permit the chart")
	}
	denied := []string{
		writeFixture(t, f.root, ".codex/other/chart.html", "private sibling"),
		writeFixture(t, f.root, ".codex/visualizations/.private/chart.html", "hidden"),
		writeFixture(t, f.root, ".codex/visualizations/secrets.html", "credential name"),
		writeFixture(t, f.root, ".codex/visualizations/binary.html", "binary\x00data"),
		filepath.Join(f.root, ".codex/visualizations/missing.html"),
	}
	outside := writeFixture(t, t.TempDir(), "outside.html", "outside")
	alias := filepath.Join(f.root, ".codex/visualizations/alias.html")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	denied = append(denied, alias)
	for _, path := range denied {
		if got, err := narrow.Rewrite(context.Background(), visualizationMarkup(path)); err != nil || got != codexclient.VisualizationFallback {
			t.Fatalf("protected or missing visualization was exposed: %q, %v", got, err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := narrow.Rewrite(context.Background(), visualizationMarkup(path)); err != nil || got != codexclient.VisualizationFallback {
		t.Fatal("deleted visualization did not receive a readable fallback")
	}
}

func TestVisualizationFailureReturnsOriginalDisplayAndNoPartialURL(t *testing.T) {
	f := newFixture(t)
	ordinary := writeFixture(t, f.root, "report.md", "# synthetic report")
	chart := writeFixture(t, f.root, "chart.html", "<p>synthetic chart</p>")
	id := f.s.recordID(fileRecord{Version: 1, Root: f.root, Relative: "chart.html"})
	if err := os.Mkdir(filepath.Join(f.opts.Directory, id+".json"), 0700); err != nil {
		t.Fatal(err)
	}
	body := "[report](" + ordinary + ")\n\n" + visualizationMarkup(chart)
	got, err := f.s.Rewrite(context.Background(), body)
	if err == nil || got != body || strings.Contains(err.Error(), chart) || strings.Contains(err.Error(), f.opts.Directory) {
		t.Fatal("failed visualization registration returned a partial display or leaked a path")
	}
}
