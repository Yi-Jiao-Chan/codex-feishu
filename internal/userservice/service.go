// Package userservice manages only the current user's codex-feishu systemd unit.
// Command output and underlying errors are deliberately excluded from results.
package userservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"codex-feishu/internal/config"
	"codex-feishu/internal/setup"
)

const UnitName = "codex-feishu.service"
const marker = "# Managed by codex-feishu userservice v1\n"

// CommandRunner receives separate arguments, never a shell command.
type CommandRunner interface {
	Run(context.Context, string, ...string) (CommandResult, error)
}

type CommandResult struct {
	Stdout   string
	ExitCode int
}

type ExecRunner struct{}

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if left := 65536 - b.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (CommandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out limitedOutput
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	err := cmd.Run()
	r := CommandResult{Stdout: out.String()}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		r.ExitCode = exit.ExitCode()
		return r, nil
	}
	return r, err
}

type Check struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Required    bool   `json:"required"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

type ServiceState struct {
	Unit              string `json:"unit"`
	UnitPath          string `json:"unit_path"`
	Installed         bool   `json:"installed"`
	Managed           bool   `json:"managed"`
	IdentityMatches   bool   `json:"identity_matches"`
	ManagerAvailable  bool   `json:"manager_available"`
	LoadState         string `json:"load_state"`
	ActiveState       string `json:"active_state"`
	SubState          string `json:"sub_state"`
	UnitFileState     string `json:"unit_file_state"`
	Linger            string `json:"linger"`             // yes, no, unknown
	LogoutPersistence string `json:"logout_persistence"` // pass or pending; distinct from active
}

type Result struct {
	CandidateTest bool         `json:"candidate_test_deployment,omitempty"`
	OK            bool         `json:"ok"`
	State         string       `json:"state"`
	Code          int          `json:"-"`
	Changed       bool         `json:"changed"`
	BackupPath    string       `json:"backup_path,omitempty"`
	Service       ServiceState `json:"service"`
	Checks        []Check      `json:"checks"`
}

type Options struct {
	Runner              CommandRunner
	UnitDir             string // default: $XDG_CONFIG_HOME/systemd/user, or ~/.config/systemd/user
	CandidateTest       bool
	CandidateBinaryPath string
}

type Manager struct {
	runner              CommandRunner
	dir                 string
	uid                 int
	candidateTest       bool
	candidateBinaryPath string
}

type Request struct {
	BinaryPath string
	ConfigPath string
}

func New(opts Options) (*Manager, error) {
	dir := opts.UnitDir
	if dir == "" {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, errors.New("user home unavailable")
			}
			base = filepath.Join(home, ".config")
		}
		dir = filepath.Join(base, "systemd", "user")
	}
	if !safeAbsolute(dir) {
		return nil, errors.New("systemd user directory must be an absolute path without control characters")
	}
	if opts.CandidateBinaryPath != "" && !safeAbsolute(opts.CandidateBinaryPath) {
		return nil, errors.New("candidate binary path must be an absolute path without control characters")
	}
	if opts.Runner == nil {
		opts.Runner = ExecRunner{}
	}
	return &Manager{runner: opts.Runner, dir: filepath.Clean(dir), uid: os.Getuid(), candidateTest: opts.CandidateTest, candidateBinaryPath: opts.CandidateBinaryPath}, nil
}

func safeAbsolute(s string) bool {
	if !filepath.IsAbs(s) {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

func (m *Manager) initial() Result {
	return Result{CandidateTest: m.candidateTest, Service: ServiceState{Unit: UnitName, UnitPath: filepath.Join(m.dir, UnitName), Linger: "unknown", LogoutPersistence: "pending"}, Checks: []Check{}}
}

func fail(r Result, code int, state, id, detail, remedy string) Result {
	r.OK, r.Code, r.State = false, code, state
	r.Checks = append(r.Checks, Check{id, "fail", true, detail, remedy})
	return r
}

func identity(path string) (string, error) {
	if !safeAbsolute(path) {
		return "", errors.New("invalid config path")
	}
	// Canonicalize aliases so symlink spellings do not produce a second deployment.
	path, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", errors.New("config unavailable")
	}
	c, err := config.Load(path)
	if err != nil {
		return "", errors.New("config invalid")
	}
	h := sha256.Sum256([]byte(path + "\x00" + c.Host.ID + "\x00" + c.Feishu.AppID + "\x00" + c.Access.OwnerOpenID))
	return hex.EncodeToString(h[:]), nil
}

func managedIdentity(b []byte) string {
	s := string(b)
	if !strings.HasPrefix(s, marker) {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimPrefix(s, marker), "\n")
	v := strings.TrimPrefix(line, "# Deployment identity: ")
	if line == v || len(v) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(v); err != nil {
		return ""
	}
	return v
}

func (m *Manager) readUnit() ([]byte, bool, error) {
	p := filepath.Join(m.dir, UnitName)
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Size() > 65536 || !ok || int(owner.Uid) != m.uid {
		return nil, true, errors.New("unsafe unit file")
	}
	b, err := os.ReadFile(p)
	return b, true, err
}

type observation struct {
	Result
	unit        []byte
	fragment    string
	dropIns     bool
	needsReload bool
}

// ReadStatus only reads configuration, files, systemd and the current user's
// loginctl properties. A stopped or absent unit does not imply a missing manager.
func (m *Manager) ReadStatus(ctx context.Context, configPath string) Result {
	return m.observe(ctx, configPath).Result
}

func (m *Manager) observe(ctx context.Context, configPath string) observation {
	o := observation{Result: m.initial()}
	b, exists, err := m.readUnit()
	o.unit, o.Service.Installed = b, exists
	if err != nil {
		o.Result = fail(o.Result, 6, "conflict", "unit_ownership", "The fixed unit is not a safe regular file owned by this user.", "Move the conflicting unit aside explicitly before installing.")
		return o
	}
	id, err := identity(configPath)
	if err != nil {
		o.Result = fail(o.Result, 2, "invalid_input", "config", "A valid absolute configuration path is required.", "Validate the requested configuration before managing its service.")
		return o
	}
	o.Service.Managed = exists && managedIdentity(b) != ""
	o.CandidateTest = m.candidateTest || o.Service.Managed && bytes.Contains(b, []byte("\n# Candidate binary SHA256: "))
	o.Service.IdentityMatches = o.Service.Managed && managedIdentity(b) == id
	if exists && !o.Service.IdentityMatches {
		o.Result = fail(o.Result, 6, "conflict", "unit_identity", "The fixed unit belongs to a manual service or a different deployment.", "Use the original deployment configuration or explicitly remove the conflicting unit.")
		return o
	}
	manager, err := m.runner.Run(ctx, "systemctl", "--user", "show", "--property=Version", "--value", "--no-pager")
	if err != nil || manager.ExitCode != 0 || strings.TrimSpace(manager.Stdout) == "" {
		o.Result = fail(o.Result, 3, "blocked", "user_manager", "The current user's systemd manager cannot be contacted.", "Run in this user's systemd session and check XDG_RUNTIME_DIR and the user bus; do not use sudo.")
		return o
	}
	o.Service.ManagerAvailable = true
	o.Checks = append(o.Checks, Check{"user_manager", "pass", true, "The current user's systemd manager is reachable.", ""})
	unit, err := m.runner.Run(ctx, "systemctl", "--user", "show", UnitName, "--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=UnitFileState", "--property=FragmentPath", "--property=DropInPaths", "--property=NeedDaemonReload", "--no-pager")
	props := properties(unit.Stdout)
	if err != nil || !knownLoad(props["LoadState"]) || !knownActive(props["ActiveState"]) {
		o.Result = fail(o.Result, 3, "blocked", "unit_status", "The manager did not provide a usable unit status.", "Inspect the fixed unit through systemctl --user before retrying.")
		return o
	}
	// systemctl may return nonzero for a missing unit; valid properties still
	// describe it. Availability was checked independently above.
	o.Service.LoadState, o.Service.ActiveState = props["LoadState"], props["ActiveState"]
	o.Service.SubState = safeEnum(props["SubState"])
	o.Service.UnitFileState = safeEnum(props["UnitFileState"])
	o.fragment, o.dropIns = props["FragmentPath"], props["DropInPaths"] != ""
	o.needsReload = props["NeedDaemonReload"] == "yes"
	if !exists && o.Service.LoadState != "not-found" {
		o.Result = fail(o.Result, 6, "conflict", "loaded_unit", "The fixed unit is loaded without a verifiable managed unit file.", "Inspect the existing runtime or system-provided service explicitly before installing.")
		return o
	}
	if o.dropIns || o.fragment != "" && filepath.Clean(o.fragment) != o.Service.UnitPath {
		o.Result = fail(o.Result, 6, "conflict", "loaded_unit", "The manager uses another unit location or extra drop-in configuration.", "Inspect and remove the conflicting override explicitly before using this installer.")
		return o
	}
	linger, err := m.runner.Run(ctx, "loginctl", "show-user", strconv.Itoa(m.uid), "--property=Linger", "--value", "--no-pager")
	if err == nil && linger.ExitCode == 0 {
		v := strings.TrimSpace(linger.Stdout)
		if v == "yes" || v == "no" {
			o.Service.Linger = v
		}
	}
	if o.Service.Linger == "yes" {
		o.Service.LogoutPersistence = "pass"
		o.Checks = append(o.Checks, Check{"logout_persistence", "pass", true, "loginctl reports linger enabled for the current user.", ""})
	} else {
		detail := "Linger is disabled; an active service in this SSH session does not prove it survives logout."
		if o.Service.Linger == "unknown" {
			detail = "The current user's linger setting could not be verified; logout persistence remains unproven."
		}
		o.Checks = append(o.Checks, Check{"logout_persistence", "pending", true, detail, "Have the host administrator arrange user linger or a verified existing supervisor, then recheck; this tool does not request sudo or change linger."})
	}
	if o.Service.UnitFileState != "enabled" {
		o.Checks = append(o.Checks, Check{"boot_autostart", "pending", true, "The fixed unit is not persistently enabled for this user.", "Install the managed unit to enable it for the user default target."})
	} else {
		o.Checks = append(o.Checks, Check{"boot_autostart", "pass", true, "The fixed unit is persistently enabled.", ""})
	}
	o.OK, o.State = true, "stopped"
	if !exists {
		o.State = "not_installed"
	} else if o.Service.ActiveState == "active" {
		o.State = "running"
	}
	return o
}

func properties(s string) map[string]string {
	r := make(map[string]string)
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			r[k] = v
		}
	}
	return r
}

func safeEnum(s string) string {
	if len(s) > 64 {
		return "unknown"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r == '-') {
			return "unknown"
		}
	}
	return s
}

func knownLoad(s string) bool {
	switch s {
	case "loaded", "not-found", "error", "bad-setting", "masked", "merged", "stub":
		return true
	}
	return false
}

func knownActive(s string) bool {
	switch s {
	case "active", "inactive", "failed", "activating", "deactivating", "reloading", "maintenance", "refreshing":
		return true
	}
	return false
}

func setAutostart(r *Result, state string) {
	r.Service.UnitFileState = state
	for i := range r.Checks {
		c := &r.Checks[i]
		if c.ID != "boot_autostart" {
			continue
		}
		c.Status, c.Detail = "pending", "The unit is not persistently enabled."
		if state == "unknown" {
			c.Detail = "Autostart state is unknown after the requested operation."
		}
		if state == "" {
			c.Status, c.Required, c.Detail, c.Remediation = "skipped", false, "No managed unit remains installed.", ""
		}
	}
}

// quoteArg follows systemd.syntax quoting and escapes percent specifiers.
// render disables environment substitution with the ':' command prefix:
// escaping dollars with '$$' would incorrectly alter the executable path,
// which systemd resolves separately before expanding argv.
func quoteArg(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "%", "%%")
	return "\"" + s + "\""
}

func render(binary, path, id string) []byte {
	return []byte(marker + "# Deployment identity: " + id + "\n[Unit]\nDescription=Codex Feishu bridge\n\n[Service]\nType=simple\nExecStart=:" + quoteArg(binary) + " serve --config " + quoteArg(path) + "\nRestart=on-failure\nRestartSec=5s\nUMask=0077\n\n[Install]\nWantedBy=default.target\n")
}

// candidateUnit freezes the actual executable bytes as well as the deployment
// identity. Database inspection is read-only and precedes installer mutations.
func candidateUnit(ctx context.Context, binary, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !safeAbsolute(binary) || !safeAbsolute(path) {
		return nil, errors.New("invalid candidate paths")
	}
	binary, err := filepath.EvalSymlinks(filepath.Clean(binary))
	if err != nil || !safeAbsolute(binary) {
		return nil, errors.New("candidate binary unavailable")
	}
	path, err = filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil || !safeAbsolute(path) {
		return nil, errors.New("candidate configuration unavailable")
	}
	c, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := setup.CheckCandidateDatabase(ctx, c.Storage.DatabaseFile); err != nil {
		return nil, err
	}
	id, err := identity(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(binary)
	if err != nil {
		return nil, errors.New("candidate binary unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	const maxBinarySize = 256 << 20
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Size() > maxBinarySize {
		return nil, errors.New("candidate binary is not an executable regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxBinarySize+1))
	if err != nil || n > maxBinarySize {
		return nil, errors.New("candidate binary cannot be inspected")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body := render(binary, path, id)
	body = bytes.Replace(body, []byte("# Deployment identity: "+id+"\n"), []byte("# Deployment identity: "+id+"\n# Candidate binary SHA256: "+hex.EncodeToString(h.Sum(nil))+"\n"), 1)
	return bytes.Replace(body, []byte("ExecStart=:"+quoteArg(binary)+" serve --config "), []byte("ExecStart=:"+quoteArg(binary)+" serve --candidate-test --config "), 1), nil
}

func candidateFailure(r Result) Result {
	return fail(r, 3, "blocked", "candidate_preflight", "Candidate executable, configuration or current-schema database could not be verified.", "Preserve existing data; use the selected candidate binary and an absent or current-schema private database.")
}

// lock serializes mutations by this installer. It never follows a lock symlink.
func (m *Manager) lock(create bool) (*os.File, error) {
	if create {
		if err := os.MkdirAll(m.dir, 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(m.dir)
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(owner.Uid) != m.uid {
		return nil, errors.New("unsafe directory")
	}
	if create {
		if err := os.Chmod(m.dir, 0700); err != nil {
			return nil, err
		}
	}
	fd, err := syscall.Open(filepath.Join(m.dir, ".codex-feishu.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "service lock")
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }

func (m *Manager) systemctl(ctx context.Context, verb string) bool {
	args := []string{"--user", verb}
	if verb != "daemon-reload" {
		args = append(args, UnitName)
	}
	r, err := m.runner.Run(ctx, "systemctl", args...)
	return err == nil && r.ExitCode == 0
}

// Install writes a private unit atomically, reloads the manager if necessary,
// and enables the fixed unit. It never starts it or changes Codex's daemon.
func (m *Manager) Install(ctx context.Context, req Request) Result {
	r := m.initial()
	if !safeAbsolute(req.BinaryPath) || !safeAbsolute(req.ConfigPath) {
		return fail(r, 2, "invalid_input", "paths", "Binary and configuration paths must be absolute and contain no control characters.", "Supply absolute paths.")
	}
	binary, err := filepath.EvalSymlinks(filepath.Clean(req.BinaryPath))
	if err != nil {
		return fail(r, 3, "blocked", "binary", "The selected executable is unavailable.", "Build or install the bridge executable first.")
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fail(r, 3, "blocked", "binary", "The selected binary is not an executable regular file.", "Select an executable bridge binary.")
	}
	path, err := filepath.EvalSymlinks(filepath.Clean(req.ConfigPath))
	if err != nil {
		return fail(r, 2, "invalid_input", "config", "The configuration file is unavailable.", "Apply a valid configuration first.")
	}
	id, err := identity(path)
	if err != nil {
		return fail(r, 2, "invalid_input", "config", "The configuration is invalid.", "Validate the configuration before installing.")
	}
	if !safeAbsolute(binary) || !safeAbsolute(path) {
		return fail(r, 2, "invalid_input", "paths", "Canonical paths contain control characters.", "Use safe absolute paths.")
	}
	desired := render(binary, path, id)
	if m.candidateTest {
		if m.candidateBinaryPath != "" {
			selected, selectErr := filepath.EvalSymlinks(filepath.Clean(m.candidateBinaryPath))
			if selectErr != nil || selected != binary {
				return fail(r, 6, "conflict", "candidate_binary", "Installation does not use the selected candidate executable.", "Select the same candidate binary for installation and startup.")
			}
		}
		desired, err = candidateUnit(ctx, binary, path)
		if err != nil {
			return candidateFailure(r)
		}
	}
	f, err := m.lock(true)
	if err != nil {
		return fail(r, 6, "conflict", "service_lock", "The private unit directory is unsafe or another service operation holds its lock.", "Check directory ownership and wait for the other operation.")
	}
	defer unlock(f)
	o := m.observe(ctx, path)
	if !o.OK {
		return o.Result
	}
	if m.candidateTest {
		if m.candidateBinaryPath != "" {
			selected, selectErr := filepath.EvalSymlinks(filepath.Clean(m.candidateBinaryPath))
			if selectErr != nil || selected != binary {
				return fail(o.Result, 6, "conflict", "candidate_binary", "The selected candidate executable changed during installation.", "Review the selected executable before retrying.")
			}
		}
		current, checkErr := candidateUnit(ctx, binary, path)
		if checkErr != nil {
			return candidateFailure(o.Result)
		}
		if !bytes.Equal(current, desired) {
			return fail(o.Result, 6, "conflict", "candidate_binary", "The candidate deployment changed during installation.", "Review the selected executable and configuration before retrying.")
		}
	}
	unitMode := os.FileMode(0)
	if info, err := os.Lstat(o.Service.UnitPath); err == nil {
		unitMode = info.Mode().Perm()
	}
	written := !bytes.Equal(desired, o.unit) || unitMode != 0600
	reload := written || o.needsReload || o.Service.LoadState != "loaded" || o.fragment == ""
	if m.candidateTest && !inactive(o.Service.ActiveState) && (reload || o.Service.UnitFileState != "enabled") {
		return fail(o.Result, 6, "conflict", "candidate_active_unit", "An active or transitioning unit cannot be changed by candidate installation.", "Explicitly stop the original deployment before reviewing a changed installation.")
	}
	if written {
		if len(o.unit) > 0 {
			o.BackupPath, err = backup(m.dir, o.unit)
			if err != nil {
				return fail(o.Result, 7, "failed", "unit_backup", "The existing unit could not be backed up safely.", "Check private-directory access before retrying.")
			}
		}
		if err := atomicWrite(o.Service.UnitPath, desired); err != nil {
			return fail(o.Result, 7, "failed", "unit_write", "The unit could not be written atomically.", "Check private-directory access before retrying.")
		}
		o.Changed = true
	}
	if reload {
		o.Changed = true
	}
	if reload && !m.systemctl(ctx, "daemon-reload") {
		if written {
			if len(o.unit) > 0 {
				err = atomicWrite(o.Service.UnitPath, o.unit)
			} else {
				err = os.Remove(o.Service.UnitPath)
			}
		}
		if err != nil || !m.systemctl(ctx, "daemon-reload") {
			return fail(o.Result, 5, "unknown", "daemon_reload", "Manager reload failed and restoration could not be confirmed.", "Inspect the unit and user manager before retrying; do not assume the installation succeeded.")
		}
		o.Changed = false
		return fail(o.Result, 7, "rolled_back", "daemon_reload", "Manager reload failed; the previous unit was restored and reloaded.", "Inspect the user manager before retrying.")
	}
	if o.Service.UnitFileState != "enabled" {
		o.Changed = true
		if !m.systemctl(ctx, "enable") {
			o.Service.Installed, o.Service.Managed, o.Service.IdentityMatches = true, true, true
			o.Service.LoadState = "loaded"
			setAutostart(&o.Result, "unknown")
			return fail(o.Result, 5, "unknown", "enable", "The unit is installed, but enabling autostart could not be confirmed.", "Inspect systemctl --user is-enabled codex-feishu.service before retrying.")
		}
	}
	post := m.observe(ctx, path)
	post.Changed, post.BackupPath = o.Changed, o.BackupPath
	if !post.OK || post.Service.LoadState != "loaded" || post.Service.UnitFileState != "enabled" || post.needsReload || m.candidateTest && !bytes.Equal(post.unit, desired) {
		return fail(post.Result, 5, "unknown", "install_confirmation", "The requested installation could not be confirmed by the user manager.", "Inspect the fixed unit and its autostart state before retrying.")
	}
	post.State = "installed"
	if !post.Changed {
		post.State = "unchanged"
	}
	post.Checks = append(post.Checks, Check{"installation", "pass", true, "The managed unit is installed and enabled; installation does not start it.", ""})
	if written && o.Service.ActiveState == "active" {
		post.Checks = append(post.Checks, Check{"running_configuration", "pending", true, "The unit was updated while active; the running process may still use its previous launch configuration.", "Explicitly stop and start the bridge when ready to apply the new unit."})
	}
	return post.Result
}

func backup(dir string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".codex-feishu.service.backup-")
	if err != nil {
		return "", err
	}
	p := f.Name()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(p)
		return "", err
	}
	return p, nil
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".codex-feishu.service.tmp-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func (m *Manager) Start(ctx context.Context, configPath string) Result {
	return m.transition(ctx, configPath, true)
}
func (m *Manager) Stop(ctx context.Context, configPath string) Result {
	return m.transition(ctx, configPath, false)
}

func (m *Manager) transition(ctx context.Context, path string, start bool) Result {
	r := m.initial()
	var desired []byte
	if start && m.candidateTest {
		var err error
		desired, err = candidateUnit(ctx, m.candidateBinaryPath, path)
		if err != nil {
			return candidateFailure(r)
		}
	}
	f, err := m.lock(false)
	if err != nil {
		return fail(r, 6, "conflict", "service_lock", "The managed unit directory is unavailable or locked.", "Install the unit or wait for the current operation.")
	}
	defer unlock(f)
	o := m.observe(ctx, path)
	if !o.OK {
		return o.Result
	}
	if !o.Service.Installed || o.Service.LoadState != "loaded" {
		return fail(o.Result, 3, "blocked", "installation", "The requested deployment has no loaded managed unit.", "Install the service before starting or stopping it.")
	}
	if start && o.needsReload {
		return fail(o.Result, 3, "blocked", "daemon_reload", "The user manager has not loaded the current unit file.", "Install the managed unit to refresh the user manager before starting.")
	}
	if start && m.candidateTest {
		info, modeErr := os.Lstat(o.Service.UnitPath)
		if modeErr != nil || info.Mode().Perm() != 0600 {
			return fail(o.Result, 6, "conflict", "candidate_unit_permissions", "The candidate unit file does not have private 0600 permissions.", "Explicitly stop the original deployment and install the reviewed candidate before starting.")
		}
		current, err := candidateUnit(ctx, m.candidateBinaryPath, path)
		if err != nil {
			return candidateFailure(o.Result)
		}
		if !bytes.Equal(desired, current) || !bytes.Equal(current, o.unit) {
			return fail(o.Result, 6, "conflict", "candidate_unit", "The installed unit does not match the selected candidate executable and deployment.", "Explicitly stop the original deployment and install the reviewed candidate before starting.")
		}
	}
	return m.setActive(ctx, path, o.Result, start)
}

func inactive(s string) bool { return s == "inactive" || s == "failed" }

func (m *Manager) setActive(ctx context.Context, path string, r Result, start bool) Result {
	verb, target := "stop", "stopped"
	if start {
		verb, target = "start", "running"
	}
	if start && r.Service.ActiveState == "active" || !start && inactive(r.Service.ActiveState) {
		r.State = target
		return r
	}
	r.Changed = true
	if !m.systemctl(ctx, verb) {
		return fail(r, 5, "unknown", "service_transition", "The requested service transition could not be confirmed.", "Read status before retrying the transition.")
	}
	post := m.observe(ctx, path).Result
	post.Changed = true
	if !post.OK || start && post.Service.ActiveState != "active" || !start && !inactive(post.Service.ActiveState) {
		return fail(post, 5, "unknown", "service_transition", "The manager has not confirmed the requested active state.", "Read status before retrying the transition.")
	}
	post.State = target
	return post
}

// Uninstall only removes this deployment's managed unit and autostart links.
// Configuration, secrets, database and recovery backups are always retained.
func (m *Manager) Uninstall(ctx context.Context, path string) Result {
	r := m.initial()
	_, exists, err := m.readUnit()
	if err != nil {
		return fail(r, 6, "conflict", "unit_ownership", "The fixed unit cannot be safely inspected.", "Inspect its ownership and file type explicitly.")
	}
	if !exists {
		r.OK, r.State = true, "not_installed"
		return r
	}
	f, err := m.lock(false)
	if err != nil {
		return fail(r, 6, "conflict", "service_lock", "The managed unit directory is unavailable or locked.", "Wait for the other operation or check directory ownership.")
	}
	defer unlock(f)
	o := m.observe(ctx, path)
	if !o.OK {
		return o.Result
	}
	if !o.Service.Installed {
		o.State = "not_installed"
		return o.Result
	}
	if o.Service.LoadState != "loaded" && o.Service.LoadState != "not-found" {
		return fail(o.Result, 3, "blocked", "unit_status", "The unit cannot be safely stopped in its current load state.", "Inspect the unit through systemctl --user before uninstalling.")
	}
	r = m.setActive(ctx, path, o.Result, false)
	if !r.OK {
		return r
	}
	// disable may affect existing autostart links, so retain the file on failure.
	if !m.systemctl(ctx, "disable") {
		r.Changed = true
		setAutostart(&r, "unknown")
		return fail(r, 5, "unknown", "disable", "The unit was stopped, but removing autostart links could not be confirmed; its unit file was retained.", "Inspect user autostart links before retrying uninstall.")
	}
	r.Changed = true
	setAutostart(&r, "disabled")
	r.BackupPath, err = backup(m.dir, o.unit)
	if err != nil {
		return fail(r, 7, "failed", "unit_backup", "The stopped unit could not be backed up; its file was retained.", "Check private-directory access before retrying.")
	}
	if err = os.Remove(r.Service.UnitPath); err != nil {
		return fail(r, 7, "failed", "unit_remove", "The stopped unit could not be removed.", "Check private-directory access before retrying.")
	}
	if !m.systemctl(ctx, "daemon-reload") {
		if atomicWrite(r.Service.UnitPath, o.unit) != nil || !m.systemctl(ctx, "daemon-reload") {
			return fail(r, 5, "unknown", "daemon_reload", "Removal reload failed and restoring the stopped unit could not be confirmed.", "Inspect the unit and manager before retrying.")
		}
		return fail(r, 7, "rolled_back", "daemon_reload", "Removal reload failed; the stopped unit was restored, with autostart disabled.", "Inspect the user manager before retrying uninstall.")
	}
	r.OK, r.State = true, "uninstalled"
	r.Service.Installed, r.Service.Managed, r.Service.IdentityMatches = false, false, false
	r.Service.LoadState, r.Service.ActiveState, r.Service.SubState = "not-found", "inactive", "dead"
	setAutostart(&r, "")
	r.Checks = append(r.Checks, Check{"data_retained", "pass", true, "Only the managed unit and its autostart links were removed; configuration, secrets and database were retained.", ""})
	return r
}
