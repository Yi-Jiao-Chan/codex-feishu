package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/filepreview"
)

type previewHTTP struct {
	preview  *filepreview.Service
	listener net.Listener
	server   *http.Server
}

func openFilePreview(cfg config.Config) (*filepreview.Service, error) {
	p := cfg.FilePreviewLimits()
	return filepreview.New(filepreview.Options{
		BaseURL: p.BaseURL, AllowedRoots: p.AllowedRoots, Directory: p.Directory,
		SecretFile: p.SecretFile, MaxFileBytes: p.MaxFileBytes,
	})
}

func startFilePreview(cfg config.Config) (*previewHTTP, error) {
	p, err := openFilePreview(cfg)
	if err != nil {
		return nil, errors.New("file preview storage or access credential unavailable")
	}
	listener, err := net.Listen("tcp", cfg.FilePreviewLimits().ListenAddress)
	if err != nil {
		p.Close()
		return nil, errors.New("file preview listen address unavailable")
	}
	return &previewHTTP{preview: p, listener: listener, server: &http.Server{
		Handler: p, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8 << 10,
		ErrorLog: log.New(io.Discard, "", 0),
	}}, nil
}

func (p *previewHTTP) Serve(ctx context.Context) error {
	done := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if p.server.Shutdown(shutdownCtx) != nil {
				p.server.Close()
			}
		case <-done:
		}
	}()
	err := p.server.Serve(p.listener)
	close(done)
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return errors.New("file preview listener stopped")
}

func (p *previewHTTP) Close() {
	p.server.Close()
	p.listener.Close()
	p.preview.Close()
}

func runFilePreview(ctx context.Context, args []string) int {
	if len(args) == 0 {
		emit(output{Command: "file-preview", State: "subcommand-required", NextSteps: []string{"use init-secret, show-secret, or link"}})
		return 2
	}
	flags := flag.NewFlagSet("file-preview "+args[0], flag.ContinueOnError)
	path := flags.String("config", "", "absolute TOML configuration path")
	destination := flags.String("output", "", "init-secret: new absolute credential file")
	file := flags.String("file", "", "link: absolute file path, optionally followed by :line[:column]")
	jsonOutput := flags.Bool("json", false, "JSON output (normal JSON never includes access codes)")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 {
		return 2
	}
	if args[0] == "init-secret" {
		if *path != "" || *file != "" || !filepath.IsAbs(*destination) || strings.ContainsAny(*destination, "\x00\r\n") {
			emit(output{Command: "file-preview", State: "invalid-secret-output"})
			return 2
		}
		if err := createPreviewSecret(*destination); err != nil {
			emit(output{Command: "file-preview", State: "secret-not-created", NextSteps: []string{"use a new file inside an existing private directory; an existing credential is never overwritten"}})
			return 3
		}
		emit(output{Command: "file-preview", OK: true, State: "secret-created", Paths: map[string]string{"secret_file": *destination}, NextSteps: []string{"use file-preview show-secret --config /absolute/config.toml for an explicit credential reveal; diagnostics and URLs never contain the code"}})
		return 0
	}
	if args[0] == "show-secret" {
		if *jsonOutput || !filepath.IsAbs(*path) || *file != "" || *destination != "" || strings.ContainsAny(*path, "\x00\r\n") {
			emit(output{Command: "file-preview", State: "invalid-show-secret-request"})
			return 2
		}
		cfg, err := config.Load(*path)
		if err != nil || !cfg.FilePreview.Enabled {
			emit(output{Command: "file-preview", State: "preview-not-configured"})
			return 2
		}
		secret, err := cfg.FilePreviewSecret()
		if err != nil {
			emit(output{Command: "file-preview", State: "secret-unavailable"})
			return 3
		}
		_, _ = fmt.Fprintln(os.Stdout, secret)
		return 0
	}
	if args[0] != "link" || !filepath.IsAbs(*path) || *file == "" || strings.ContainsAny(*file, "\x00\r\n<>`") || *destination != "" {
		emit(output{Command: "file-preview", State: "invalid-link-request"})
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil || !cfg.FilePreview.Enabled {
		emit(output{Command: "file-preview", State: "preview-not-configured"})
		return 2
	}
	p, err := openFilePreview(cfg)
	if err != nil {
		emit(output{Command: "file-preview", State: "preview-unavailable"})
		return 3
	}
	defer p.Close()
	input := "[查看文件](<" + *file + ">)"
	link, err := p.Rewrite(ctx, input)
	if err != nil || link == input {
		emit(output{Command: "file-preview", State: "file-not-previewable"})
		return 3
	}
	// Only the public link is returned; local filenames, registry records and
	// credentials do not enter this diagnostic envelope.
	start := strings.LastIndex(link, "](")
	if start < 0 || !strings.HasSuffix(link, ")") {
		emit(output{Command: "file-preview", State: "link-unavailable"})
		return 7
	}
	address := strings.Trim(link[start+2:len(link)-1], "<>")
	emit(output{Command: "file-preview", OK: true, State: "link-registered", Data: map[string]any{"url": address, "ui_verified": false}})
	return 0
}

func createPreviewSecret(path string) error {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("credential directory must be private")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return errors.New("access code generation unavailable")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("new access code file unavailable")
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if _, err := io.WriteString(f, hex.EncodeToString(b)+"\n"); err != nil {
		return errors.New("access code write unavailable")
	}
	if err := f.Sync(); err != nil {
		return errors.New("access code sync unavailable")
	}
	if err := f.Close(); err != nil {
		return errors.New("access code close unavailable")
	}
	ok = true
	return nil
}
