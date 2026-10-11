package devpackage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

const maxSourceBytes = 2 << 20

type sourceSpec struct {
	source string
	dest   string
}

// Keep this list literal and small. In particular, do not replace it with a
// repository glob: the archive must never accidentally absorb credentials,
// databases, research notes, logs, or a newly added source file.
var sourceAllowlist = []sourceSpec{
	{source: "docs/codex-feishu/AGENT-SETUP.md", dest: "docs/codex-feishu/AGENT-SETUP.md"},
	{source: "docs/codex-feishu/examples/config.example.toml", dest: "docs/codex-feishu/examples/config.example.toml"},
	{source: "docs/codex-feishu/examples/setup-input.example.json", dest: "docs/codex-feishu/examples/setup-input.example.json"},
	{source: "docs/codex-feishu/examples/acceptance-report.template.json", dest: "docs/codex-feishu/examples/acceptance-report.template.json"},
	{source: "compat/feishu-permissions.json", dest: "compat/feishu-permissions.json"},
	{source: "docs/codex-feishu/examples/codex-feishu.service.template", dest: "docs/codex-feishu/examples/codex-feishu.service.template"},
}

type sourceSnapshot struct {
	Files  map[string][]byte
	Hashes map[string]string
}

func sourceHashes(repo string) (map[string]string, error) {
	snapshot, err := readSources(repo)
	if err != nil {
		return nil, err
	}
	return snapshot.Hashes, nil
}

func readSources(repo string) (sourceSnapshot, error) {
	files := make(map[string][]byte, len(sourceAllowlist))
	hashes := make(map[string]string, len(sourceAllowlist))
	for _, spec := range sourceAllowlist {
		data, err := readSafeSource(repo, spec.source)
		if err != nil {
			return sourceSnapshot{}, fmt.Errorf("source input %s unavailable", spec.source)
		}
		copyData := append([]byte(nil), data...)
		files[spec.dest] = copyData
		hash := sha256.Sum256(copyData)
		hashes[spec.source] = hex.EncodeToString(hash[:])
	}
	return sourceSnapshot{Files: files, Hashes: hashes}, nil
}

func readSafeSource(repo, relative string) ([]byte, error) {
	if filepath.IsAbs(relative) || relative == "." || filepath.Clean(relative) != relative || strings.HasPrefix(relative, "../") || strings.Contains(relative, string(filepath.Separator)+"../") {
		return nil, errors.New("invalid source path")
	}
	path := filepath.Join(repo, filepath.FromSlash(relative))
	if err := rejectSymlinkParents(repo, path); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("source is not a regular file")
	}
	if before.Size() > maxSourceBytes {
		return nil, errors.New("source exceeds size limit")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxSourceBytes+1))
	statErr := error(nil)
	opened, e := f.Stat()
	if e != nil {
		statErr = e
	}
	closeErr := f.Close()
	after, lstatErr := os.Lstat(path)
	if readErr != nil || statErr != nil || closeErr != nil || lstatErr != nil {
		return nil, errors.New("source read failed")
	}
	if len(data) > maxSourceBytes || before.Size() != int64(len(data)) || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		return nil, errors.New("source changed while reading")
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() {
		return nil, errors.New("source is not a regular file")
	}
	return data, nil
}

func rejectSymlinkParents(root, target string) error {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("source escapes repository")
	}
	cur := root
	rootInfo, err := os.Lstat(cur)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("repository path is unsafe")
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return errors.New("source parent unavailable")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("source path contains symlink")
		}
		if !info.IsDir() && cur != target {
			return errors.New("source parent is not a directory")
		}
	}
	return nil
}

func canonicalRepository(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("repository must be absolute")
	}
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("repository path is unsafe")
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", errors.New("repository path cannot be resolved")
	}
	realInfo, err := os.Stat(real)
	if err != nil || !realInfo.IsDir() {
		return "", errors.New("repository path is not a directory")
	}
	return filepath.Clean(real), nil
}

func validateOutputLocation(repo, output string) (string, error) {
	if output == "" || !filepath.IsAbs(output) {
		return "", errors.New("output must be absolute")
	}
	output = filepath.Clean(output)
	if output == string(filepath.Separator) || output == filepath.VolumeName(output)+string(filepath.Separator) {
		return "", errors.New("output directory is too broad")
	}
	if err := rejectOutputSymlinks(output); err != nil {
		return "", err
	}
	repo, err := canonicalRepository(repo)
	if err != nil {
		return "", err
	}
	relRepo, err := filepath.Rel(repo, output)
	if err != nil {
		return "", errors.New("output path cannot be compared")
	}
	insideRepo := relRepo == "." || (relRepo != ".." && !strings.HasPrefix(relRepo, ".."+string(filepath.Separator)))
	if insideRepo {
		dist := filepath.Join(repo, "dist")
		relDist, err := filepath.Rel(dist, output)
		if err != nil || relDist == ".." || strings.HasPrefix(relDist, ".."+string(filepath.Separator)) {
			return "", errors.New("output inside repository must be under ignored dist")
		}
		if err := ensureIgnored(repo, output); err != nil {
			return "", err
		}
	}
	return output, nil
}

func rejectOutputSymlinks(path string) error {
	volume := filepath.VolumeName(path)
	cur := volume + string(filepath.Separator)
	rest := strings.TrimPrefix(path, cur)
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return errors.New("output parent unavailable")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("output path contains symlink")
		}
		if !info.IsDir() {
			return errors.New("output path contains non-directory")
		}
	}
	return nil
}

func ensureIgnored(repo, output string) error {
	rel, err := filepath.Rel(repo, output)
	if err != nil {
		return errors.New("output path cannot be compared")
	}
	// The command receives a path relative to the repository and never uses a
	// shell. --no-index also makes this check work for a not-yet-created dist.
	ignoredPath := strings.TrimSuffix(filepath.ToSlash(rel), "/") + "/"
	cmd := exec.Command("git", "-C", repo, "check-ignore", "--no-index", "--quiet", "--", ignoredPath)
	if err := cmd.Run(); err != nil {
		return errors.New("output directory is not ignored")
	}
	return nil
}

func prepareOutputDirectory(path string) error {
	if err := makeDirectories(path); err != nil {
		return err
	}
	return verifyPrivateDirectory(path)
}

func makeDirectories(path string) error {
	volume := filepath.VolumeName(path)
	cur := volume + string(filepath.Separator)
	rest := strings.TrimPrefix(path, cur)
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(cur, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return errors.New("cannot create output directory")
			}
			info, err = os.Lstat(cur)
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("output path is unsafe")
		}
	}
	return nil
}

func verifyPrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("output directory is unavailable")
	}
	if info.Mode().Perm() != 0700 || !ownedByCurrentUser(info) {
		return errors.New("output directory must be owned and mode 0700")
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(os.Geteuid())
}

func verifyPrivateRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("existing output file is not regular")
	}
	if info.Mode().Perm() != 0600 || !ownedByCurrentUser(info) {
		return errors.New("existing output file must be owned and mode 0600")
	}
	return nil
}
