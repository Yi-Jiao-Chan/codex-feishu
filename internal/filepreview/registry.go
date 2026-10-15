package filepreview

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const maxRecordBytes = 16 << 10

type fileRecord struct {
	Version  int    `json:"version"`
	Root     string `json:"root"`
	Relative string `json:"relative"`
}

type previewFile struct {
	name     string
	content  []byte
	modified time.Time
}

func ownedPrivate(info fs.FileInfo, mode fs.FileMode) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid() && info.Mode().Perm() == mode &&
		info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}

func readSecret(name string) ([32]byte, string, error) {
	var zero [32]byte
	abs, err := filepath.Abs(name)
	if err != nil {
		return zero, "", errors.New("file preview: private secret file unavailable")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return zero, "", errors.New("file preview: private secret file unavailable")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return zero, "", errors.New("file preview: private secret file unavailable")
	}
	defer root.Close()
	body, err := readPrivateFile(root, filepath.Base(abs), 1024)
	if err != nil {
		return zero, "", errors.New("file preview: secret must be an owned 0600 regular file without symlinks")
	}
	secret := string(body)
	if strings.HasSuffix(secret, "\n") {
		secret = strings.TrimSuffix(strings.TrimSuffix(secret, "\n"), "\r")
	}
	if len(secret) < 32 || len(secret) > 256 {
		return zero, "", errors.New("file preview: secret must contain 32 to 256 visible ASCII characters")
	}
	for _, c := range secret {
		if c < '!' || c > '~' {
			return zero, "", errors.New("file preview: secret must contain 32 to 256 visible ASCII characters")
		}
	}
	return sha256.Sum256([]byte(secret)), filepath.Join(parent, filepath.Base(abs)), nil
}

func openRegistry(name string) (*os.Root, string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, "", errRegistry
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, "", errRegistry
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || !ownedPrivate(info, 0700) {
		return nil, "", errRegistry
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", errRegistry
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, "", errRegistry
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) || !ownedPrivate(opened, 0700) {
		_ = root.Close()
		return nil, "", errRegistry
	}
	return root, canonical, nil
}

func readPrivateFile(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !ownedPrivate(info, 0600) || info.Size() > limit {
		return nil, errRegistry
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || !ownedPrivate(opened, 0600) || opened.Size() > limit {
		return nil, errRegistry
	}
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errRegistry
	}
	return body, nil
}

func validID(id string) bool {
	if len(id) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(id)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == id
}

func (s *Service) recordID(record fileRecord) string {
	return s.mac("file", record.Root+"\x00"+record.Relative)
}

func (s *Service) checkRegistry() error {
	info, err := s.directory.Stat(".")
	if err != nil || !info.IsDir() || !ownedPrivate(info, 0700) {
		return errRegistry
	}
	current, err := os.Lstat(s.directoryPath)
	if err != nil || !current.IsDir() || !os.SameFile(info, current) {
		return errRegistry
	}
	return nil
}

func (s *Service) loadRecord(id string) (fileRecord, error) {
	var record fileRecord
	if !validID(id) || s.checkRegistry() != nil {
		return record, errRegistry
	}
	body, err := readPrivateFile(s.directory, id+".json", maxRecordBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return record, fs.ErrNotExist
		}
		return record, errRegistry
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Version != 1 ||
		!filepath.IsAbs(record.Root) || filepath.Clean(record.Root) != record.Root ||
		!allowedRelative(record.Relative) || s.recordID(record) != id {
		return fileRecord{}, errRegistry
	}
	return record, nil
}

func (s *Service) persistRecord(record fileRecord) (string, error) {
	id := s.recordID(record)
	existing, err := s.loadRecord(id)
	if err == nil {
		if existing != record {
			return "", errRegistry
		}
		if s.syncRegistry() != nil {
			return "", errRegistry
		}
		return id, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", errRegistry
	}
	body, err := json.Marshal(record)
	if err != nil || len(body) > maxRecordBytes {
		return "", errRegistry
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", errRegistry
	}
	tmp := ".pending-" + base64.RawURLEncoding.EncodeToString(nonce[:])
	f, err := s.directory.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return "", errRegistry
	}
	defer func() { _ = s.directory.Remove(tmp) }()
	_, writeErr := f.Write(body)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || s.checkRegistry() != nil {
		return "", errRegistry
	}
	// Every writer of this ID writes identical authenticated metadata. Independent
	// records avoid a shared read/modify/write index and lost CLI registrations.
	if s.directory.Rename(tmp, id+".json") != nil || s.syncRegistry() != nil {
		return "", errRegistry
	}
	return id, nil
}

func (s *Service) syncRegistry() error {
	if s.checkRegistry() != nil {
		return errRegistry
	}
	f, err := s.directory.Open(".")
	if err != nil {
		return errRegistry
	}
	defer f.Close()
	return f.Sync()
}

func allowedRelative(relative string) bool {
	if relative == "" || !filepath.IsLocal(relative) || filepath.Clean(relative) != relative || strings.ContainsAny(relative, "\\\x00\r\n") {
		return false
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if strings.HasPrefix(component, ".") || credentialName(component) {
			return false
		}
	}
	return textName(filepath.Base(relative))
}

func credentialName(name string) bool {
	lower := strings.ToLower(name)
	for _, word := range strings.FieldsFunc(lower, func(r rune) bool { return r == '-' || r == '_' || r == '.' || r == ' ' }) {
		switch word {
		case "secret", "secrets", "credential", "credentials", "token", "tokens", "password", "passwords", "passwd", "shadow", "kubeconfig":
			return true
		}
	}
	return lower == "authorized_keys" || lower == "known_hosts" || strings.HasPrefix(lower, "id_rsa") ||
		strings.HasPrefix(lower, "id_dsa") || strings.HasPrefix(lower, "id_ecdsa") || strings.HasPrefix(lower, "id_ed25519")
}

func textName(name string) bool {
	switch strings.ToLower(name) {
	case "readme", "license", "licence", "makefile", "dockerfile", "justfile":
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown", ".mdown", ".txt", ".go", ".mod", ".sum", ".py", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx",
		".rs", ".java", ".c", ".h", ".hpp", ".cpp", ".cc", ".sh", ".bash", ".zsh", ".fish", ".toml", ".yaml", ".yml",
		".json", ".jsonc", ".ini", ".cfg", ".conf", ".css", ".scss", ".sql", ".proto", ".graphql", ".gql", ".rb", ".php",
		".swift", ".kt", ".kts", ".scala", ".lua", ".r", ".ex", ".exs", ".erl", ".hrl", ".pl", ".pm", ".ps1",
		".html", ".htm", ".xml", ".csv", ".tsv", ".diff", ".patch":
		return true
	}
	return false
}

func isWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && (relative == "." || filepath.IsLocal(relative))
}

func deniedFileError(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrInvalid) ||
		errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EXDEV) || errors.Is(err, os.ErrClosed) {
		return errNotPreviewable
	}
	return errFileIO
}

// readFile traverses directories using held os.Root handles and refuses symlinks
// at every component. Each opened inode is compared with its lstat result; Root
// additionally constrains resolution during a concurrent symlink replacement.
func (s *Service) readFile(root *allowedRoot, relative string) (previewFile, error) {
	var result previewFile
	if !allowedRelative(relative) {
		return result, errNotPreviewable
	}
	abs := filepath.Join(root.path, relative)
	if abs == s.secretPath || isWithin(abs, s.directoryPath) {
		return result, errNotPreviewable
	}
	parts := strings.Split(relative, string(filepath.Separator))
	current := root.handle
	var openedRoots []*os.Root
	defer func() {
		for _, handle := range openedRoots {
			_ = handle.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		info, err := current.Lstat(part)
		if err != nil {
			return result, deniedFileError(err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return result, errNotPreviewable
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			return result, deniedFileError(err)
		}
		openedRoots = append(openedRoots, next)
		openedInfo, err := next.Stat(".")
		if err != nil {
			return result, deniedFileError(err)
		}
		if !os.SameFile(info, openedInfo) {
			return result, errNotPreviewable
		}
		current = next
	}
	name := parts[len(parts)-1]
	info, err := current.Lstat(name)
	if err != nil {
		return result, deniedFileError(err)
	}
	if !info.Mode().IsRegular() || info.Size() > s.maxBytes {
		return result, errNotPreviewable
	}
	f, err := current.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return result, deniedFileError(err)
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil {
		return result, deniedFileError(err)
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() || openedInfo.Size() > s.maxBytes {
		return result, errNotPreviewable
	}
	body, err := io.ReadAll(io.LimitReader(f, s.maxBytes+1))
	if err != nil {
		return result, deniedFileError(err)
	}
	if int64(len(body)) > s.maxBytes || !utf8.Valid(body) {
		return result, errNotPreviewable
	}
	if sha256.Sum256(bytes.TrimSpace(body)) == s.key {
		return result, errNotPreviewable // Also excludes a hard-link alias of our secret.
	}
	for _, b := range body {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			return result, errNotPreviewable
		}
	}
	// The descriptor stays pinned to this regular file if its pathname changes.
	return previewFile{name: name, content: body, modified: openedInfo.ModTime()}, nil
}
