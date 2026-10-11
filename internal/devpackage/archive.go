package devpackage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

var epoch = time.Unix(0, 0).UTC()

type archiveFile struct {
	Name string
	Mode int64
	Data []byte
}

type artifactDocument struct {
	SchemaVersion int               `json:"schema_version"`
	Channel       string            `json:"channel"`
	SourceCommit  string            `json:"source_commit"`
	OS            string            `json:"os"`
	Arch          string            `json:"arch"`
	BinarySHA256  string            `json:"binary_sha256"`
	Version       VersionData       `json:"version"`
	Members       map[string]string `json:"members"`
}

type codexCompatDocument struct {
	SchemaVersion  int    `json:"schema_version"`
	CodexProtocol  string `json:"codex_protocol"`
	BridgeCommit   string `json:"bridge_commit"`
	SDK            string `json:"sdk"`
	Phase1Released bool   `json:"phase1_released"`
}

func buildArchive(top string, goos string, goarch string, commit string, binary []byte, source sourceSnapshot, version VersionInfo) ([]byte, string, error) {
	binaryDigest := digest(binary)
	compat, err := marshalJSON(codexCompatDocument{
		SchemaVersion:  1,
		CodexProtocol:  version.CodexProtocol,
		BridgeCommit:   commit,
		SDK:            version.SDK,
		Phase1Released: false,
	})
	if err != nil {
		return nil, "", errors.New("cannot encode compatibility metadata")
	}
	readme := []byte("# codex-feishu development artifact\n\nThis is a development/unreleased artifact. It is not a phase-one release and does not promote or enable release behavior.\n\nVerify checksums before execution with `sha256sum -c SHA256SUMS`. Read the configuration guide at `docs/codex-feishu/AGENT-SETUP.md`. No actual install or start is performed by this archive.\n")

	files := make(map[string]archiveFile, len(source.Files)+3)
	files["README.md"] = archiveFile{Name: "README.md", Mode: 0644, Data: readme}
	files["bin/codex-feishu"] = archiveFile{Name: "bin/codex-feishu", Mode: 0755, Data: binary}
	files["compat/codex.json"] = archiveFile{Name: "compat/codex.json", Mode: 0644, Data: compat}
	for name, data := range source.Files {
		if _, exists := files[name]; exists {
			return nil, "", errors.New("source destination collides with generated file")
		}
		files[name] = archiveFile{Name: name, Mode: 0644, Data: data}
	}

	members := make(map[string]string, len(files))
	for name, file := range files {
		members[name] = digest(file.Data)
	}
	artifact, err := marshalJSON(artifactDocument{
		SchemaVersion: 1,
		Channel:       "development",
		SourceCommit:  commit,
		OS:            goos,
		Arch:          goarch,
		BinarySHA256:  binaryDigest,
		Version:       version.Data,
		Members:       members,
	})
	if err != nil {
		return nil, "", errors.New("cannot encode artifact metadata")
	}
	files["artifact.json"] = archiveFile{Name: "artifact.json", Mode: 0644, Data: artifact}

	checksums := make(map[string]string, len(files))
	for name, file := range files {
		checksums[name] = digest(file.Data)
	}
	checksumData := formatChecksums(checksums)
	files["SHA256SUMS"] = archiveFile{Name: "SHA256SUMS", Mode: 0644, Data: checksumData}

	ordered := orderedArchiveFiles(files)
	archive, err := encodeTarGz(top, ordered)
	if err != nil {
		return nil, "", err
	}
	return archive, digest(archive), nil
}

func marshalJSON(value any) ([]byte, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func formatChecksums(checksums map[string]string) []byte {
	names := make([]string, 0, len(checksums))
	for name := range checksums {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s  %s\n", checksums[name], name)
	}
	return []byte(b.String())
}

func orderedArchiveFiles(files map[string]archiveFile) []archiveFile {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	// Keep metadata and executable locations stable across runs. Within each
	// directory, lexical order makes additions fail loudly in review rather
	// than depending on map iteration order.
	order := map[string]int{
		"bin/codex-feishu":                                           10,
		"compat/codex.json":                                          20,
		"compat/feishu-permissions.json":                             21,
		"docs/codex-feishu/AGENT-SETUP.md":                           30,
		"docs/codex-feishu/examples/acceptance-report.template.json": 40,
		"docs/codex-feishu/examples/codex-feishu.service.template":   41,
		"docs/codex-feishu/examples/config.example.toml":             42,
		"docs/codex-feishu/examples/setup-input.example.json":        43,
		"README.md":     50,
		"artifact.json": 60,
		"SHA256SUMS":    70,
	}
	sort.Slice(names, func(i, j int) bool {
		left, lok := order[names[i]]
		right, rok := order[names[j]]
		if lok && rok && left != right {
			return left < right
		}
		if lok != rok {
			return lok
		}
		return names[i] < names[j]
	})
	result := make([]archiveFile, 0, len(names))
	for _, name := range names {
		result = append(result, files[name])
	}
	return result
}

func encodeTarGz(top string, files []archiveFile) ([]byte, error) {
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	zw.Header.ModTime = epoch
	zw.Header.Name = ""
	zw.Header.Comment = ""
	zw.Header.Extra = nil
	// The package is intentionally Linux-only. Pin the gzip OS byte as well as
	// its timestamp so two builds on the supported host are byte-identical.
	zw.Header.OS = 3
	tw := tar.NewWriter(zw)
	dirs := []string{
		top + "/",
		top + "/bin/",
		top + "/compat/",
		top + "/docs/",
		top + "/docs/codex-feishu/",
		top + "/docs/codex-feishu/examples/",
	}
	for _, dir := range dirs {
		if err := writeTarHeader(tw, tar.Header{Name: dir, Mode: 0755, Typeflag: tar.TypeDir}); err != nil {
			return nil, errors.New("cannot encode archive directory")
		}
	}
	for _, file := range files {
		name := top + "/" + file.Name
		if err := writeTarHeader(tw, tar.Header{Name: name, Mode: file.Mode, Size: int64(len(file.Data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, errors.New("cannot encode archive file")
		}
		if _, err := tw.Write(file.Data); err != nil {
			return nil, errors.New("cannot write archive file")
		}
	}
	if err := tw.Close(); err != nil {
		return nil, errors.New("cannot close tar archive")
	}
	if err := zw.Close(); err != nil {
		return nil, errors.New("cannot close gzip archive")
	}
	return raw.Bytes(), nil
}

func writeTarHeader(tw *tar.Writer, header tar.Header) error {
	header.ModTime = epoch
	// USTAR has no atime/ctime fields. Keeping these Go zero values avoids
	// introducing PAX records while retaining a zero archive timestamp.
	header.AccessTime = time.Time{}
	header.ChangeTime = time.Time{}
	header.Uid = 0
	header.Gid = 0
	header.Uname = ""
	header.Gname = ""
	header.PAXRecords = nil
	header.Format = tar.FormatUSTAR
	return tw.WriteHeader(&header)
}

func writePrivateFile(path string, data []byte, mode uint32) error {
	f, err := openExclusive(path, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(os.FileMode(mode)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
