package config

import (
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// FilePreviewConfig is separate from discovery: finding a thread does not
// authorize browser access to all of its files.
type FilePreviewConfig struct {
	Enabled       bool     `toml:"enabled" json:"enabled"`
	BaseURL       string   `toml:"base_url" json:"base_url"`
	ListenAddress string   `toml:"listen_address" json:"listen_address"`
	AllowedRoots  []string `toml:"allowed_roots" json:"allowed_roots"`
	Directory     string   `toml:"directory" json:"directory"`
	SecretFile    string   `toml:"secret_file" json:"secret_file"`
	MaxFileBytes  int64    `toml:"max_file_bytes" json:"max_file_bytes"`
}

func (c Config) FilePreviewLimits() FilePreviewConfig {
	p := c.FilePreview
	if p.ListenAddress == "" {
		p.ListenAddress = "127.0.0.1:8787"
	}
	if p.Directory == "" {
		p.Directory = filepath.Join(filepath.Dir(c.Storage.DatabaseFile), "file-preview")
	}
	if p.SecretFile == "" {
		p.SecretFile = filepath.Join(filepath.Dir(c.Storage.DatabaseFile), "file-preview.key")
	}
	if p.MaxFileBytes == 0 {
		p.MaxFileBytes = 1 << 20
	}
	return p
}

func (c Config) ValidateFilePreview() error {
	p := c.FilePreviewLimits()
	if p.MaxFileBytes < 1 || p.MaxFileBytes > 10<<20 {
		return errors.New("file_preview.max_file_bytes must be between 1 and 10 MiB")
	}
	for _, path := range []string{p.Directory, p.SecretFile} {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("file preview state and secret paths must be absolute")
		}
	}
	if !p.Enabled {
		return nil
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		u.Path != "" && u.Path != "/" || u.RawPath != "" {
		return errors.New("file_preview.base_url must be an HTTP(S) origin without credentials, path, query or fragment")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("file preview URL port is invalid")
		}
	}
	host, port, err := net.SplitHostPort(p.ListenAddress)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 || host != "" && net.ParseIP(host) == nil {
		return errors.New("file_preview.listen_address must be an IP and a port")
	}
	if len(p.AllowedRoots) == 0 {
		return errors.New("enabled file preview requires explicit allowed_roots")
	}
	for _, root := range p.AllowedRoots {
		if !filepath.IsAbs(root) || strings.ContainsAny(root, "\x00\r\n") {
			return errors.New("file preview allowed roots must be absolute directories")
		}
		physical, err := filepath.EvalSymlinks(root)
		if err != nil || physical == string(filepath.Separator) {
			return errors.New("file preview allowed directory is unavailable or too broad")
		}
		info, err := os.Stat(physical)
		if err != nil || !info.IsDir() {
			return errors.New("file preview allowed directory is unavailable")
		}
	}
	return nil
}

// FilePreviewSecret reads only the explicitly configured private credential;
// values and fingerprints must never appear in diagnostics.
func (c Config) FilePreviewSecret() (string, error) {
	path := c.FilePreviewLimits().SecretFile
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return "", errors.New("file preview secret must be a private regular file with mode 0600")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		return "", errors.New("file preview secret must belong to the current user")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("file preview secret unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0600 {
		return "", errors.New("file preview secret changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, 259))
	s := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if err != nil || len(s) < 32 || len(s) > 256 || len(b) > 258 {
		return "", errors.New("file preview secret must contain a 32–256 character access code")
	}
	for _, r := range s {
		if r < 33 || r > 126 {
			return "", errors.New("file preview secret must contain one ASCII access code")
		}
	}
	return s, nil
}
