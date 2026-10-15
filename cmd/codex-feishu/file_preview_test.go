package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/config"
	"github.com/pelletier/go-toml/v2"
)

func previewCommandStdout(t *testing.T, args []string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = w
	defer func() {
		os.Stdout = previous
		_ = r.Close()
		_ = w.Close()
	}()
	code := run(context.Background(), args)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(body)
}

func writePreviewFixtureConfig(t *testing.T, path string, cfg config.Config) {
	t.Helper()
	b, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewSecretCreationDoesNotExposeOrReplaceCredentials(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0700)
	path := filepath.Join(root, "preview.key")
	code, out := candidateCommandOutput(t, []string{"file-preview", "init-secret", "--output", path, "--json"})
	if code != 0 || !out.OK {
		t.Fatalf("credential creation failed: %d %+v", code, out)
	}
	b, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(b))) != 64 {
		t.Fatal("access code was not generated", err)
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), strings.TrimSpace(string(b))) {
		t.Fatal("normal CLI output exposed access code")
	}
	if err := createPreviewSecret(path); err == nil {
		t.Fatal("existing credential was overwritten")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(b) {
		t.Fatal("failed init changed existing access code")
	}
}

func TestPreviewShowSecretExplicitlyRevealsConfiguredCredential(t *testing.T) {
	path, cfg, _, _ := doctorFixture(t)
	secretPath := filepath.Join(filepath.Dir(path), "preview.key")
	secret := "fixture-preview-secret-0123456789abcdef"
	if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.FilePreview = config.FilePreviewConfig{Enabled: true, BaseURL: "http://10.0.0.1:8787", SecretFile: secretPath, AllowedRoots: cfg.Discovery.AllowedRoots}
	writePreviewFixtureConfig(t, path, cfg)

	code, stdout := previewCommandStdout(t, []string{"file-preview", "show-secret", "--config", path})
	if code != 0 || stdout != secret+"\n" {
		t.Fatalf("show-secret output mismatch: code=%d stdout=%q", code, stdout)
	}
}

func TestPreviewShowSecretRejectsInvalidRequestsWithoutExposingCredential(t *testing.T) {
	path, cfg, _, _ := doctorFixture(t)
	secretPath := filepath.Join(filepath.Dir(path), "preview.key")
	secret := "fixture-preview-secret-0123456789abcdef"
	if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.FilePreview = config.FilePreviewConfig{Enabled: true, BaseURL: "http://10.0.0.1:8787", SecretFile: secretPath, AllowedRoots: cfg.Discovery.AllowedRoots}
	writePreviewFixtureConfig(t, path, cfg)

	for _, args := range [][]string{
		{"file-preview", "show-secret"},
		{"file-preview", "show-secret", "--config", "config.toml"},
		{"file-preview", "show-secret", "--config", path, "--file", filepath.Join(filepath.Dir(path), "report.md")},
		{"file-preview", "show-secret", "--config", path, "--output", filepath.Join(filepath.Dir(path), "out.key")},
		{"file-preview", "show-secret", "--config", path, "--json=true"},
	} {
		code, out := candidateCommandOutput(t, args)
		if code != 2 || out.OK || out.State == "" {
			t.Fatalf("invalid show-secret request accepted: args=%v code=%d output=%+v", args, code, out)
		}
		encoded, _ := json.Marshal(out)
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("invalid show-secret request exposed credential: args=%v output=%s", args, encoded)
		}
	}
}

func TestPreviewShowSecretRejectsDisabledAndUnavailableCredentials(t *testing.T) {
	path, cfg, _, _ := doctorFixture(t)
	secretPath := filepath.Join(filepath.Dir(path), "preview.key")
	secret := "fixture-preview-secret-0123456789abcdef"
	if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.FilePreview = config.FilePreviewConfig{SecretFile: secretPath}
	writePreviewFixtureConfig(t, path, cfg)
	code, out := candidateCommandOutput(t, []string{"file-preview", "show-secret", "--config", path})
	if code != 2 || out.OK {
		t.Fatalf("disabled preview unexpectedly revealed credential: code=%d output=%+v", code, out)
	}

	cfg.FilePreview = config.FilePreviewConfig{Enabled: true, BaseURL: "http://10.0.0.1:8787", SecretFile: secretPath, AllowedRoots: cfg.Discovery.AllowedRoots}
	writePreviewFixtureConfig(t, path, cfg)
	if err := os.Chmod(secretPath, 0644); err != nil {
		t.Fatal(err)
	}
	code, out = candidateCommandOutput(t, []string{"file-preview", "show-secret", "--config", path})
	if code != 3 || out.OK || out.State != "secret-unavailable" {
		t.Fatalf("unsafe preview credential was accepted: code=%d output=%+v", code, out)
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), secret) {
		t.Fatal("unavailable credential appeared in diagnostic output")
	}
	if err := os.Remove(secretPath); err != nil {
		t.Fatal(err)
	}
	code, out = candidateCommandOutput(t, []string{"file-preview", "show-secret", "--config", path})
	if code != 3 || out.OK || out.State != "secret-unavailable" {
		t.Fatalf("missing preview credential was not rejected: code=%d output=%+v", code, out)
	}
}

func TestPreviewHTTPStopsWithServiceContext(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0700)
	secret := filepath.Join(root, "preview.key")
	if err := createPreviewSecret(secret); err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.Storage.DatabaseFile = filepath.Join(root, "bridge.db")
	cfg.FilePreview = config.FilePreviewConfig{Enabled: true, BaseURL: "http://127.0.0.1:8787", ListenAddress: "127.0.0.1:0", AllowedRoots: []string{root}, SecretFile: secret}
	p, err := startFilePreview(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- p.Serve(ctx) }()
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", "http://"+p.listener.Addr().String()+"/login", nil)
	req.Host = "127.0.0.1:8787"
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	resp.Body.Close()
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("preview listener did not join on bridge shutdown")
	}
	if c, err := net.DialTimeout("tcp", p.listener.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Fatal("preview listener outlived the bridge")
	}
}

func TestDoctorPreviewNeverExposesCredentialOrClaimsClientAcceptance(t *testing.T) {
	path, cfg, deps, _ := doctorFixture(t)
	secret := filepath.Join(filepath.Dir(path), "preview.key")
	if err := os.WriteFile(secret, []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.FilePreview = config.FilePreviewConfig{Enabled: true, BaseURL: "http://10.0.0.1:8787", SecretFile: secret, AllowedRoots: cfg.Discovery.AllowedRoots}
	writePreviewFixtureConfig(t, path, cfg)
	o, code := inspectDoctor(context.Background(), path, false, true, deps)
	if code != 0 || !o.OK {
		t.Fatalf("configured preview prerequisites failed: %d %+v", code, o)
	}
	data := o.Data.(map[string]any)["file_preview"].(map[string]any)
	if data["enabled"] != true || data["ui_verified"] != false {
		t.Fatal("preview claimed browser acceptance")
	}
	encoded, _ := json.Marshal(o)
	if strings.Contains(string(encoded), strings.Repeat("a", 64)) {
		t.Fatal("doctor exposed preview credential")
	}
	if err := os.Chmod(secret, 0644); err != nil {
		t.Fatal(err)
	}
	o, code = inspectDoctor(context.Background(), path, false, true, deps)
	if code != 3 || o.OK {
		t.Fatal("doctor accepted public preview access code")
	}
}
