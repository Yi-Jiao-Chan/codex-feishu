package devpackage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureCommit = "0123456789abcdef0123456789abcdef01234567"

type fixtureRunner struct {
	build   func(BuildRequest) error
	inspect func() (GitState, error)
	version VersionInfo
}

func (r fixtureRunner) Build(_ context.Context, req BuildRequest) error {
	if r.build != nil {
		return r.build(req)
	}
	return os.WriteFile(req.BinaryPath, []byte("stable fake binary"), 0755)
}

func (r fixtureRunner) InspectGit(context.Context, string) (GitState, error) {
	if r.inspect != nil {
		return r.inspect()
	}
	return GitState{Commit: fixtureCommit}, nil
}

func (r fixtureRunner) Version(context.Context, string) (VersionInfo, error) {
	return r.version, nil
}

func fixtureVersion(released bool) VersionInfo {
	data := VersionData{
		"version":         json.RawMessage(`"0.0.1-dev"`),
		"commit":          json.RawMessage(`"` + fixtureCommit + `"`),
		"phase1_released": json.RawMessage(strings.ToLower(strconvBool(released))),
		"config_schema":   json.RawMessage(`1`),
		"codex_protocol":  json.RawMessage(`"0.162.0"`),
		"sdk":             json.RawMessage(`"fixture-sdk"`),
		"capabilities":    json.RawMessage(`{"verify_managed_tests":false}`),
	}
	return VersionInfo{OK: true, Data: data, Commit: fixtureCommit, Phase1Released: released, ConfigSchema: 1, CodexProtocol: "0.162.0", SDK: "fixture-sdk"}
}

func strconvBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func makeFixtureRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, spec := range sourceAllowlist {
		path := filepath.Join(repo, filepath.FromSlash(spec.source))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture "+spec.source+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "ignored.secret"), []byte("credential-looking data"), 0600); err != nil {
		t.Fatal(err)
	}
	return repo
}

func buildFixture(t *testing.T, repo, output string, version VersionInfo) Result {
	t.Helper()
	result, err := Build(context.Background(), Options{Repository: repo, Output: output, Runner: fixtureRunner{version: version}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFixtureArchiveHasOnlyAllowlistedContentAndVerifiableHashes(t *testing.T) {
	repo := makeFixtureRepo(t)
	output := filepath.Join(t.TempDir(), "out")
	result := buildFixture(t, repo, output, fixtureVersion(false))
	if result.State != "published" || result.Noop || result.ArchiveSHA256 == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	archivePath := filepath.Join(output, result.ArchiveName)
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := digest(archiveBytes); got != result.ArchiveSHA256 {
		t.Fatalf("archive digest = %s, result = %s", got, result.ArchiveSHA256)
	}
	files := readArchiveFiles(t, archiveBytes)
	for name := range files {
		if strings.Contains(name, "ignored.secret") {
			t.Fatalf("secret leaked into archive: %s", name)
		}
	}
	for _, name := range []string{"README.md", "artifact.json", "SHA256SUMS", "bin/codex-feishu", "compat/codex.json", "compat/feishu-permissions.json", "compat/codex-pinning.json", "docs/codex-feishu/AGENT-SETUP.md", "docs/codex-feishu/examples/codex-feishu.service.template"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("missing archive member %s", name)
		}
	}
	var artifact artifactDocument
	if err := json.Unmarshal(files["artifact.json"], &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.SchemaVersion != 1 || artifact.Channel != "development" || artifact.SourceCommit != fixtureCommit || artifact.BinarySHA256 != digest(files["bin/codex-feishu"]) {
		t.Fatalf("bad artifact metadata: %+v", artifact)
	}
	for name, want := range artifact.Members {
		if got := digest(files[name]); got != want {
			t.Fatalf("artifact member %s digest = %s, want %s", name, got, want)
		}
	}
	var compat codexCompatDocument
	if err := json.Unmarshal(files["compat/codex.json"], &compat); err != nil {
		t.Fatal(err)
	}
	if compat.CodexProtocol != "0.162.0" || compat.BridgeCommit != fixtureCommit || compat.SDK != "fixture-sdk" || compat.Phase1Released {
		t.Fatalf("bad compatibility metadata: %+v", compat)
	}
	verifyChecksumFile(t, files)
}

func readArchiveFiles(t *testing.T, archiveBytes []byte) map[string][]byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	files := map[string][]byte{}
	var top string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if top == "" {
			top = strings.TrimSuffix(h.Name, "/")
		}
		if h.Typeflag == tar.TypeReg {
			name := strings.TrimPrefix(h.Name, top+"/")
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			files[name] = data
			if h.ModTime.Unix() != 0 || h.Uid != 0 || h.Gid != 0 {
				t.Fatalf("non-deterministic metadata on %s: %+v", h.Name, h)
			}
		}
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	return files
}

func verifyChecksumFile(t *testing.T, files map[string][]byte) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(files["SHA256SUMS"])), "\n")
	if len(lines) != len(files)-1 {
		t.Fatalf("checksum lines = %d, regular members excluding SHA256SUMS = %d", len(lines), len(files)-1)
	}
	previous := ""
	for _, line := range lines {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 || previous != "" && parts[1] <= previous {
			t.Fatalf("checksum lines not sorted: %q", line)
		}
		previous = parts[1]
		data, ok := files[parts[1]]
		if !ok || digest(data) != parts[0] {
			t.Fatalf("bad checksum line: %q", line)
		}
	}
}

func TestDeterministicArchiveAndSecondRunNoop(t *testing.T) {
	repo := makeFixtureRepo(t)
	a := buildFixture(t, repo, filepath.Join(t.TempDir(), "a"), fixtureVersion(false))
	b := buildFixture(t, repo, filepath.Join(t.TempDir(), "b"), fixtureVersion(false))
	if a.ArchiveSHA256 != b.ArchiveSHA256 {
		t.Fatalf("deterministic archive digests differ: %s != %s", a.ArchiveSHA256, b.ArchiveSHA256)
	}
	out := filepath.Join(t.TempDir(), "same")
	firstResult := buildFixture(t, repo, out, fixtureVersion(false))
	secondResult := buildFixture(t, repo, out, fixtureVersion(false))
	if firstResult.State != "published" || !secondResult.Noop || secondResult.State != "noop" {
		t.Fatalf("unexpected second-run results: first=%+v second=%+v", firstResult, secondResult)
	}
}

func TestDirtySymlinkSourceChangedAndPendingReleaseRefuse(t *testing.T) {
	repo := makeFixtureRepo(t)
	output := filepath.Join(t.TempDir(), "out")
	dirty := fixtureRunner{version: fixtureVersion(false), inspect: func() (GitState, error) { return GitState{}, errors.New("dirty") }}
	if _, err := (Builder{Runner: dirty}).Build(context.Background(), Options{Repository: repo, Output: output}); err == nil {
		t.Fatal("dirty tree was accepted")
	}
	service := filepath.Join(repo, "docs/codex-feishu/examples/codex-feishu.service.template")
	if err := os.Remove(service); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, "ignored.secret"), service); err != nil {
		t.Fatal(err)
	}
	if _, err := (Builder{Runner: fixtureRunner{version: fixtureVersion(false)}}).Build(context.Background(), Options{Repository: repo, Output: output}); err == nil {
		t.Fatal("symlink source was accepted")
	}
	_ = os.Remove(service)
	if err := os.WriteFile(service, []byte("fixture service\n"), 0600); err != nil {
		t.Fatal(err)
	}
	changing := fixtureRunner{version: fixtureVersion(false), build: func(req BuildRequest) error {
		if err := os.WriteFile(req.BinaryPath, []byte("stable fake binary"), 0755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(repo, "docs/codex-feishu/examples/codex-feishu.service.template"), []byte("changed"), 0600)
	}}
	if _, err := (Builder{Runner: changing}).Build(context.Background(), Options{Repository: repo, Output: output}); err == nil {
		t.Fatal("source tree change was accepted")
	}
	if _, err := (Builder{Runner: fixtureRunner{version: fixtureVersion(true)}}).Build(context.Background(), Options{Repository: repo, Output: output}); err == nil {
		t.Fatal("pending acceptance was treated as a releasable artifact")
	}
}

func TestConflictingArchivePreservesExistingBytes(t *testing.T) {
	repo := makeFixtureRepo(t)
	out := filepath.Join(t.TempDir(), "out")
	first := buildFixture(t, repo, out, fixtureVersion(false))
	archivePath := filepath.Join(out, first.ArchiveName)
	original, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	conflict := append([]byte(nil), original...)
	conflict[0] ^= 0xff
	if err := os.WriteFile(archivePath, conflict, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Builder{Runner: fixtureRunner{version: fixtureVersion(false)}}).Build(context.Background(), Options{Repository: repo, Output: out}); err == nil {
		t.Fatal("conflicting archive was replaced")
	}
	got, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, conflict) {
		t.Fatal("conflicting archive changed")
	}
	if sha256.Sum256(got) == sha256.Sum256(original) {
		t.Fatal("conflicting fixture did not differ")
	}
}
