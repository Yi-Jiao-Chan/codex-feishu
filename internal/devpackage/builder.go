package devpackage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Options controls one development package build. Repository and Output must
// be absolute; Output is created when needed and is never treated as a file.
type Options struct {
	Repository string
	Output     string
	Runner     Runner
}

func (o Options) repository() string { return o.Repository }

// Result is safe for the CLI to serialize. It contains no command output,
// paths, usernames, or version error bodies.
type Result struct {
	State         string `json:"state"`
	Commit        string `json:"commit,omitempty"`
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
	ArchiveName   string `json:"archive_name,omitempty"`
	Noop          bool   `json:"noop"`
}

// Builder packages one archive using the supplied Runner. A nil Runner uses
// CommandRunner.
type Builder struct {
	Runner Runner
}

// Build is the package-level convenience entry point used by small clients.
func Build(ctx context.Context, options Options) (Result, error) {
	return (Builder{Runner: options.Runner}).Build(ctx, options)
}

// Build runs the complete two-gate workflow: inspect a clean source tree,
// compile and inspect the binary, then re-inspect the tree immediately before
// no-clobber publication.
func (b Builder) Build(ctx context.Context, options Options) (Result, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return Result{}, failure("unsupported-host")
	}
	if requested := os.Getenv("GOOS"); requested != "" && requested != runtime.GOOS {
		return Result{}, failure("cross-compilation")
	}
	if requested := os.Getenv("GOARCH"); requested != "" && requested != runtime.GOARCH {
		return Result{}, failure("cross-compilation")
	}
	repo, err := canonicalRepository(options.repository())
	if err != nil {
		return Result{}, failure("unsafe-repository")
	}
	output, err := validateOutputLocation(repo, options.Output)
	if err != nil {
		return Result{}, failure("unsafe-output")
	}
	if err := prepareOutputDirectory(output); err != nil {
		return Result{}, failure("unsafe-output")
	}
	runner := b.Runner
	if runner == nil {
		runner = CommandRunner{}
	}
	initial, err := runner.InspectGit(ctx, repo)
	if err != nil {
		return Result{}, preserveFailure("source-inspection-failed", err)
	}
	if !commitPattern.MatchString(initial.Commit) {
		return Result{}, failure("source-inspection-failed")
	}
	initialSources, err := readSources(repo)
	if err != nil {
		return Result{}, failure("invalid-source")
	}
	if len(initial.SourceHashes) != 0 && !sameHashes(initial.SourceHashes, initialSources.Hashes) {
		return Result{}, failure("source-changed")
	}

	stage, err := os.MkdirTemp(output, ".codex-feishu-devpackage-")
	if err != nil {
		return Result{}, failure("staging-failed")
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0700); err != nil {
		return Result{}, failure("staging-failed")
	}
	binDir := filepath.Join(stage, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		return Result{}, failure("staging-failed")
	}
	binaryPath := filepath.Join(binDir, "codex-feishu")
	if err := runner.Build(ctx, BuildRequest{Repository: repo, BinaryPath: binaryPath, Commit: initial.Commit}); err != nil {
		return Result{}, preserveFailure("build-failed", err)
	}
	if err := verifyBuiltBinary(binaryPath); err != nil {
		return Result{}, failure("build-failed")
	}
	version, err := runner.Version(ctx, binaryPath)
	if err != nil {
		return Result{}, preserveFailure("invalid-artifact", err)
	}
	if err := normalizeVersion(&version); err != nil {
		return Result{}, failure("invalid-artifact")
	}
	if err := validateVersion(version, initial.Commit); err != nil {
		return Result{}, failure("invalid-artifact")
	}
	binary, err := readRegularBinary(binaryPath)
	if err != nil {
		return Result{}, failure("build-failed")
	}
	// The source snapshot is re-read after the compiler and version command so
	// edits made while those commands run cannot enter the archive unnoticed.
	packagedSources, err := readSources(repo)
	if err != nil {
		return Result{}, failure("source-changed")
	}
	if !sameHashes(initialSources.Hashes, packagedSources.Hashes) {
		return Result{}, failure("source-changed")
	}

	top := fmt.Sprintf("codex-feishu-%s-%s-development-%s", runtime.GOOS, runtime.GOARCH, initial.Commit[:12])
	archive, archiveDigest, err := buildArchive(top, runtime.GOOS, runtime.GOARCH, initial.Commit, binary, packagedSources, version)
	if err != nil {
		return Result{}, failure("staging-failed")
	}
	archiveName := top + ".tar.gz"
	archivePath := filepath.Join(output, archiveName)
	checksumName := archiveName + ".sha256"
	checksumPath := filepath.Join(output, checksumName)
	checksum := []byte(archiveDigest + "  " + archiveName + "\n")
	archiveCandidate := filepath.Join(stage, archiveName)
	checksumCandidate := filepath.Join(stage, checksumName)
	if err := writePrivateFile(archiveCandidate, archive, 0600); err != nil {
		return Result{}, failure("staging-failed")
	}
	if err := writePrivateFile(checksumCandidate, checksum, 0600); err != nil {
		return Result{}, failure("staging-failed")
	}

	final, err := runner.InspectGit(ctx, repo)
	if err != nil {
		return Result{}, preserveFailure("source-inspection-failed", err)
	}
	if final.Commit != initial.Commit || !commitPattern.MatchString(final.Commit) {
		return Result{}, failure("source-changed")
	}
	if finalHashes := final.SourceHashes; len(finalHashes) != 0 {
		if !sameHashes(initialSources.Hashes, finalHashes) {
			return Result{}, failure("source-changed")
		}
	}
	finalSources, err := readSources(repo)
	if err != nil {
		return Result{}, failure("source-changed")
	}
	if !sameHashes(initialSources.Hashes, finalSources.Hashes) {
		return Result{}, failure("source-changed")
	}

	noop, err := publishNoClobber(output, archivePath, checksumPath, archiveCandidate, checksumCandidate, archive, checksum)
	if err != nil {
		return Result{}, failure("output-conflict")
	}
	return Result{State: stateFor(noop), Commit: initial.Commit, ArchiveSHA256: archiveDigest, ArchiveName: archiveName, Noop: noop}, nil
}

func stateFor(noop bool) string {
	if noop {
		return "noop"
	}
	return "published"
}

func sameHashes(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, hash := range left {
		if right[name] != hash {
			return false
		}
	}
	return true
}

func validateVersion(version VersionInfo, commit string) error {
	if !version.OK {
		return errors.New("version command was not ok")
	}
	if version.Commit != commit {
		return errors.New("built binary commit does not match source")
	}
	if version.ConfigSchema != 1 {
		return errors.New("built binary config schema is unsupported")
	}
	if version.CodexProtocol == "" || strings.TrimSpace(version.CodexProtocol) == "" {
		return errors.New("built binary has no Codex protocol")
	}
	if version.Phase1Released {
		return errors.New("release artifact cannot be created by development packager")
	}
	if version.Data == nil {
		return errors.New("version data is missing")
	}
	return nil
}

func normalizeVersion(version *VersionInfo) error {
	if version == nil || version.Data == nil {
		return errors.New("version data is missing")
	}
	if value, err := dataString(version.Data, "commit"); err == nil && version.Commit == "" {
		version.Commit = value
	}
	if value, err := dataString(version.Data, "codex_protocol"); err == nil && version.CodexProtocol == "" {
		version.CodexProtocol = value
	}
	if value, err := dataString(version.Data, "sdk"); err == nil && version.SDK == "" {
		version.SDK = value
	}
	if value, err := dataInt(version.Data, "config_schema"); err == nil && version.ConfigSchema == 0 {
		version.ConfigSchema = value
	}
	phase, err := dataBool(version.Data, "phase1_released")
	if err != nil {
		return errors.New("version release state is missing")
	}
	if version.Phase1Released != phase {
		return errors.New("version release state is inconsistent")
	}
	return nil
}

func verifyBuiltBinary(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("build did not produce a regular binary")
	}
	if err := os.Chmod(path, 0700); err != nil {
		return errors.New("cannot protect built binary")
	}
	return nil
}

func readRegularBinary(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("binary is not a regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read built binary")
	}
	return b, nil
}
