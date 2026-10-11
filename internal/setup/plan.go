package setup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"codex-feishu/internal/config"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
	"github.com/pelletier/go-toml/v2"
)

var (
	ErrNeedsIdentity       = errors.New("owner identity is required; use pairing first")
	ErrStalePlan           = errors.New("setup plan is stale; preview again")
	ErrUnreleased          = errors.New("artifact has not completed phase1 release acceptance")
	ErrPolicyUpdatePending = errors.New("existing notification changes require the pending managed-policy apply path")
)

type Planner struct {
	Probe func(context.Context, string) (Environment, error)
}

type PolicyView struct {
	OpenID string       `json:"open_id"`
	Policy rules.Policy `json:"policy"`
}
type PolicyChange struct {
	OpenID        string `json:"open_id"`
	Before, After rules.Policy
}
type Plan struct {
	ID                           string         `json:"plan_id"`
	Paths                        Paths          `json:"paths"`
	Environment                  Environment    `json:"environment"`
	Config                       config.Config  `json:"config"`
	Changed                      bool           `json:"changed"`
	CanApply                     bool           `json:"can_apply"`
	DatabaseSchema               int            `json:"database_schema"`
	ExistingPolicies             []PolicyView   `json:"existing_notification_policies"`
	PolicyChanges                []PolicyChange `json:"notification_changes"`
	ServiceMode                  string         `json:"service_mode"`
	Verification                 any            `json:"verification"`
	NextSteps                    []string       `json:"next_steps"`
	content                      []byte
	previous                     []byte
	previousDigest, sourceDigest string
}

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func encoded(v any) []byte   { b, _ := json.Marshal(v); return b }

func regularFile(path string, executable bool) ([]byte, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\n\r") {
		return nil, errors.New("file path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || executable && info.Mode().Perm()&0111 == 0 {
		return nil, errors.New("required regular file unavailable")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || !executable && int(stat.Uid) != os.Geteuid() {
		return nil, errors.New("file must belong to the current system user")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("file unavailable")
	}
	defer f.Close()
	limit := int64(1 << 20)
	if executable {
		limit = 256 << 20
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("file unavailable or too large")
	}
	return b, nil
}

// Resolve existing ancestors without creating directories. The final config
// file must be regular, not a symlink; aliases of its parent share one plan.
func canonicalTarget(path string) (string, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\n\r") {
		return "", errors.New("target path must be absolute")
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return "", errors.New("config target must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("config target unavailable")
	}
	parent, parts := filepath.Dir(filepath.Clean(path)), []string{filepath.Base(path)}
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			for i := len(parts) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, parts[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) || parent == filepath.Dir(parent) {
			return "", errors.New("target parent unavailable")
		}
		parts = append(parts, filepath.Base(parent))
		parent = filepath.Dir(parent)
	}
}

func has(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func unique(values []string) []string {
	out := []string{}
	for _, v := range values {
		if !has(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func (p Planner) Build(ctx context.Context, in Input, paths Paths) (Plan, error) {
	var plan Plan
	if in.SchemaVersion != 1 || in.Features.Creation || in.Features.Phase != "" && in.Features.Phase != "phase1" {
		return plan, errors.New("only phase1 setup is supported")
	}
	if in.Service.Mode != "" && in.Service.Mode != "systemd-user" && in.Service.Mode != "foreground" {
		return plan, errors.New("unsupported service mode")
	}
	var err error
	paths.ConfigFile, err = canonicalTarget(paths.ConfigFile)
	if err != nil {
		return plan, err
	}
	if !filepath.IsAbs(paths.DatabaseFile) {
		return plan, errors.New("database path must be absolute")
	}
	plan.Paths, plan.ServiceMode = paths, in.Service.Mode
	if plan.ServiceMode == "" {
		plan.ServiceMode = "systemd-user"
	}
	oldBytes, err := regularFile(paths.ConfigFile, false)
	var c config.Config
	if err == nil {
		c, err = config.Load(paths.ConfigFile)
		if err != nil {
			return plan, errors.New("existing configuration invalid; preserve and repair it first")
		}
		plan.previous, plan.previousDigest = oldBytes, digest(oldBytes)
		plan.Paths.DatabaseFile = c.Storage.DatabaseFile
	} else if _, statErr := os.Lstat(paths.ConfigFile); !errors.Is(statErr, os.ErrNotExist) {
		return plan, err
	} else {
		plan.previousDigest = "missing"
		host, hostErr := os.Hostname()
		if hostErr != nil {
			return plan, errors.New("host identity unavailable")
		}
		c.SchemaVersion = 1
		c.Host.ID = "host-" + digest(encoded([]any{host, os.Geteuid(), paths.ConfigFile}))[:32]
		c.Storage.DatabaseFile = paths.DatabaseFile
		c.Discovery.FirstRun, c.Discovery.ScanIntervalSeconds = "baseline", 15
		c.Discovery.Enabled = true
		c.Notifications.Timezone = "Asia/Shanghai"
		c.Notifications.MentionOn = []string{"completed", "failed", "needs_user"}
		c.Sync.CardUpdateMinIntervalMS = 800
		c.Creation.DefaultFollowMode = "group"
	}
	if c.Feishu.AppID != "" && c.Feishu.AppID != in.Feishu.AppID {
		return plan, errors.New("application change requires a separate deployment; existing data preserved")
	}
	if in.Identity.OwnerOpenID == nil {
		if in.Identity.PairOwner || c.Access.OwnerOpenID == "" {
			return plan, ErrNeedsIdentity
		}
	} else {
		if *in.Identity.OwnerOpenID == "" {
			return plan, ErrNeedsIdentity
		}
		c.Access.OwnerOpenID = *in.Identity.OwnerOpenID
	}
	c.Access.OperatorOpenIDs = unique(append(append([]string{}, in.Identity.OperatorOpenIDs...), c.Access.OwnerOpenID))
	c.Notifications.TargetOpenIDs = unique(in.Identity.NotificationOpenIDs)
	if len(c.Notifications.TargetOpenIDs) == 0 {
		c.Notifications.TargetOpenIDs = []string{c.Access.OwnerOpenID}
	}
	c.Groups.AdditionalMemberOpenIDs = unique(in.Identity.GroupMemberOpenIDs)
	c.Feishu.AppID, c.Feishu.AppSecretFile = in.Feishu.AppID, in.Feishu.AppSecretFile
	c.Discovery.AllowedRoots = []string{}
	for _, root := range in.Discovery.AllowedRoots {
		root, err = rules.CanonicalDirectory(root)
		if err != nil {
			return plan, errors.New("authorized directory unavailable")
		}
		if !has(c.Discovery.AllowedRoots, root) {
			c.Discovery.AllowedRoots = append(c.Discovery.AllowedRoots, root)
		}
	}
	if in.Discovery.AutoGroup != nil {
		c.Discovery.Enabled = *in.Discovery.AutoGroup
	}
	if in.Notifications.Timezone != "" {
		c.Notifications.Timezone = in.Notifications.Timezone
	}
	if in.Notifications.QuietHours != nil {
		c.Notifications.QuietHours = *in.Notifications.QuietHours
	}
	artifactPath, err := filepath.EvalSymlinks(in.Artifact.BinaryPath)
	if err != nil || !filepath.IsAbs(artifactPath) {
		return plan, errors.New("artifact path unavailable")
	}
	artifactBytes, err := regularFile(artifactPath, true)
	if err != nil {
		return plan, err
	}
	probe := p.Probe
	if probe == nil {
		probe = Probe
	}
	plan.Environment, err = probe(ctx, artifactPath)
	if err != nil {
		return plan, err
	}
	if plan.Environment.Daemon.Status != "running" || plan.Environment.Daemon.AppServerVersion != "0.162.0" || plan.Environment.Artifact.CodexProtocol != "0.162.0" || plan.Environment.Artifact.ConfigSchema != 1 || !filepath.IsAbs(plan.Environment.CLIPath) {
		return plan, errors.New("existing daemon or artifact compatibility is unverified")
	}
	for _, command := range []string{"version", "doctor", "setup", "serve", "service", "status", "verify"} {
		if !has(plan.Environment.Artifact.Commands, command) {
			return plan, errors.New("artifact is missing a required phase1 command")
		}
	}
	c.Codex.CLIPath, c.Codex.Endpoint = plan.Environment.CLIPath, "auto"
	if err := c.Validate(); err != nil {
		return plan, errors.New("configuration values are invalid")
	}
	secret, err := c.Secret()
	if err != nil {
		return plan, err
	}
	if _, err := regularFile(c.Feishu.AppSecretFile, false); err != nil {
		return plan, err
	}
	plan.Config = c
	if err := plan.readPolicies(ctx, in); err != nil {
		return plan, err
	}
	plan.content, err = toml.Marshal(c)
	if err != nil {
		return plan, errors.New("configuration serialization failed")
	}
	// Input authorization and the inspected artifact/credentials are included.
	// No secret value or fingerprint is exposed in the returned JSON plan.
	plan.sourceDigest = digest(encoded([]any{in, digest(artifactBytes), digest([]byte(secret)), plan.Environment, plan.Paths, plan.ExistingPolicies}))
	plan.ID = digest(encoded([]any{plan.sourceDigest, plan.previousDigest, digest(plan.content), plan.PolicyChanges}))
	plan.Changed = !reflect.DeepEqual(plan.previous, plan.content)
	plan.CanApply = plan.Environment.Artifact.Released && len(plan.PolicyChanges) == 0
	if !plan.Environment.Artifact.Released {
		plan.NextSteps = append(plan.NextSteps, "phase1 acceptance is pending; this is a development preview")
	}
	if len(plan.PolicyChanges) > 0 {
		plan.NextSteps = append(plan.NextSteps, "explicit existing policy changes need managed policy apply; no defaults overwrite them")
	}
	plan.NextSteps = append(plan.NextSteps, "apply configuration only; service installation/start and authorized verification are separate")
	plan.Verification = in.Verification
	return plan, nil
}

func (p *Plan) readPolicies(ctx context.Context, in Input) error {
	path := p.Config.Storage.DatabaseFile
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("existing database unavailable")
	}
	s, version, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return err
	}
	defer s.Close()
	p.DatabaseSchema = version
	if version < 5 {
		return errors.New("legacy database needs reviewed migration and route adoption before setup")
	}
	var app, host string
	err = s.DB.QueryRowContext(ctx, "SELECT host_id,app_id FROM deployment LIMIT 1").Scan(&host, &app)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return errors.New("deployment metadata unavailable")
	}
	if err == nil && (app != p.Config.Feishu.AppID || host != p.Config.Host.ID) {
		return errors.New("configuration and existing database identity differ")
	}
	for _, user := range p.Config.Notifications.TargetOpenIDs {
		policy, err := s.NotificationPolicy(ctx, p.Config.Feishu.AppID, user)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return errors.New("existing notification policy unavailable")
		}
		p.ExistingPolicies = append(p.ExistingPolicies, PolicyView{OpenID: user, Policy: policy})
		if user != p.Config.Access.OwnerOpenID || in.Notifications.QuietHours == nil && in.Notifications.Timezone == "" {
			continue
		}
		next := policy
		if in.Notifications.QuietHours != nil {
			next.QuietStart, next.QuietEnd, _ = strings.Cut(*in.Notifications.QuietHours, "-")
		}
		if in.Notifications.Timezone != "" {
			next.Timezone = in.Notifications.Timezone
		}
		if next == policy {
			continue
		}
		if policy.Revision == 1<<63-1 {
			return errors.New("notification revision overflow")
		}
		next.Revision++
		if err := next.Validate(); err != nil {
			return errors.New("notification changes are invalid")
		}
		p.PolicyChanges = append(p.PolicyChanges, PolicyChange{OpenID: user, Before: policy, After: next})
	}
	return nil
}
