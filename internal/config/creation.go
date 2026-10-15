package config

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	"codex-feishu/internal/rules"
	"golang.org/x/sys/unix"
)

// CreationConfig controls the optional flow that starts a new Codex session
// from Feishu. It is deliberately independent from DiscoveryConfig: creation
// scopes are an explicit user choice and are never inferred from discovery.
type CreationConfig struct {
	Enabled           bool     `toml:"enabled" json:"enabled"`
	DefaultFollowMode string   `toml:"default_follow_mode" json:"default_follow_mode"`
	EntryChatIDs      []string `toml:"entry_chat_ids" json:"entry_chat_ids"`
	WorkgroupChatIDs  []string `toml:"workgroup_chat_ids" json:"workgroup_chat_ids"`
	AllowedRoots      []string `toml:"allowed_roots" json:"allowed_roots"`
	CommonDirectories []string `toml:"common_directories" json:"common_directories"`
	TopicsEnabled     bool     `toml:"topics_enabled" json:"topics_enabled"`
	DraftTTLSeconds   int      `toml:"draft_ttl_seconds" json:"draft_ttl_seconds"`
}

const defaultCreationTTLSeconds = 1800

// ValidateCreation validates only the local creation policy. In particular,
// it does not consult discovery roots or call any remote API.
func (c Config) ValidateCreation() error {
	creation := c.Creation
	followMode := creation.DefaultFollowMode
	if followMode == "" {
		followMode = "group"
	}
	if followMode != "group" && followMode != "topic" {
		return errors.New("creation.default_follow_mode must be group or topic")
	}
	if creation.DraftTTLSeconds < 0 || creation.DraftTTLSeconds != 0 && (creation.DraftTTLSeconds < 300 || creation.DraftTTLSeconds > 86400) {
		return errors.New("creation.draft_ttl_seconds must be 300–86400 seconds, or 0 for default")
	}
	if err := validateCreationChatIDs(creation.EntryChatIDs); err != nil {
		return errors.New("invalid or duplicate creation.entry_chat_ids")
	}
	if err := validateCreationChatIDs(creation.WorkgroupChatIDs); err != nil {
		return errors.New("invalid or duplicate creation.workgroup_chat_ids")
	}
	if creation.TopicsEnabled && len(creation.WorkgroupChatIDs) == 0 {
		return errors.New("creation.topics_enabled requires at least one workgroup_chat_id")
	}
	if followMode == "topic" && (!creation.TopicsEnabled || len(creation.WorkgroupChatIDs) == 0) {
		return errors.New("topic follow mode requires topics_enabled and at least one workgroup_chat_id")
	}
	if creation.Enabled && len(creation.AllowedRoots) == 0 {
		return errors.New("enabled creation requires explicit allowed_roots")
	}

	roots, err := canonicalCreationDirectories(creation.AllowedRoots, creation.Enabled)
	if err != nil {
		return err
	}
	for _, directory := range creation.CommonDirectories {
		if !filepath.IsAbs(directory) {
			return errors.New("creation.common_directories must be absolute")
		}
		canonical, err := rules.CanonicalDirectory(directory)
		if err != nil {
			return errors.New("creation common directory unavailable")
		}
		inside := false
		for _, root := range roots {
			if rules.Inside(root, canonical) {
				inside = true
				break
			}
		}
		if !inside {
			return errors.New("creation common directory is outside allowed_roots")
		}
	}
	return nil
}

func canonicalCreationDirectories(directories []string, requireBrowseAccess bool) ([]string, error) {
	canonical := make([]string, 0, len(directories))
	for _, directory := range directories {
		if !filepath.IsAbs(directory) {
			return nil, errors.New("creation.allowed_roots must be absolute")
		}
		real, err := rules.CanonicalDirectory(directory)
		if err != nil {
			return nil, errors.New("creation allowed directory unavailable")
		}
		// Scope roots are navigation boundaries, not execution directories.
		// A read-only parent may contain writable descendants selected later.
		if requireBrowseAccess && unix.Access(real, unix.R_OK|unix.X_OK) != nil {
			return nil, errors.New("creation allowed directory must be readable and searchable")
		}
		canonical = append(canonical, real)
	}
	return canonical, nil
}

func validateCreationChatIDs(ids []string) error {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !validCreationChatID(id) {
			return errors.New("invalid chat id")
		}
		if _, ok := seen[id]; ok {
			return errors.New("duplicate chat id")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func validCreationChatID(id string) bool {
	if !strings.HasPrefix(id, "oc_") || len(id) <= len("oc_") {
		return false
	}
	for _, r := range id[len("oc_"):] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// CreationAllows reports whether path resolves to a directory inside one of
// the explicitly configured creation roots. Symlinks are resolved before the
// boundary check, so an alias cannot escape its configured physical root.
func (c Config) CreationAllows(path string) bool {
	if !c.Creation.Enabled {
		return false
	}
	actual, err := rules.CanonicalDirectory(path)
	if err != nil {
		return false
	}
	for _, configured := range c.Creation.AllowedRoots {
		root, err := rules.CanonicalDirectory(configured)
		if err == nil && rules.Inside(root, actual) {
			return true
		}
	}
	return false
}

// CreationTTL returns the configured draft lifetime, applying the documented
// default for a zero value. Invalid nonzero values are rejected by Validate.
func (c Config) CreationTTL() time.Duration {
	seconds := c.Creation.DraftTTLSeconds
	if seconds == 0 {
		seconds = defaultCreationTTLSeconds
	}
	return time.Duration(seconds) * time.Second
}
