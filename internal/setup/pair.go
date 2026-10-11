package setup

import (
	"errors"
	"os"
	"path/filepath"

	"codex-feishu/internal/config"
)

var (
	// ErrPairIdentityConflict means that pairing would change the identity or
	// credential reference of an existing deployment.
	ErrPairIdentityConflict = errors.New("pair input conflicts with existing configuration identity")

	errPairInputSchema    = errors.New("invalid pair input schema")
	errPairInputPhase     = errors.New("unsupported pair input phase")
	errPairAppID          = errors.New("valid feishu app id required")
	errPairOwner          = errors.New("owner must be a valid feishu open id")
	errPairSecret         = errors.New("app secret file is unavailable or invalid")
	errPairConfigTarget   = errors.New("configuration target is unavailable")
	errPairExistingConfig = errors.New("existing configuration invalid; preserve and repair it first")
)

// PairPreparation contains the local state needed by the pairing command.
// Secret is intentionally excluded from JSON output because it is only needed
// by the in-process pairing flow.
type PairPreparation struct {
	AppID         string         `json:"app_id"`
	Secret        string         `json:"-"`
	ConfigPath    string         `json:"config_path"`
	Existing      *config.Config `json:"existing,omitempty"`
	ExpectedOwner string         `json:"expected_owner,omitempty"`
}

// PreparePair validates the partial setup input used by pairing. It performs
// no writes and does not require the artifact, discovery roots, or daemon to
// be available.
func PreparePair(in Input, configPath string) (PairPreparation, error) {
	var prepared PairPreparation

	if in.SchemaVersion != 1 {
		return prepared, errPairInputSchema
	}
	if in.Features.Creation {
		return prepared, errPairInputPhase
	}
	if in.Features.Phase != "" && in.Features.Phase != "phase1" {
		return prepared, errPairInputPhase
	}
	if !config.ValidAppID(in.Feishu.AppID) {
		return prepared, errPairAppID
	}
	if in.Identity.OwnerOpenID != nil && !config.ValidOpenID(*in.Identity.OwnerOpenID) {
		return prepared, errPairOwner
	}

	secretPath := in.Feishu.AppSecretFile
	if !filepath.IsAbs(secretPath) {
		return prepared, errPairSecret
	}
	if _, err := regularFile(secretPath, false); err != nil {
		return prepared, errPairSecret
	}
	secretConfig := config.Config{}
	secretConfig.Feishu.AppSecretFile = secretPath
	secret, err := secretConfig.Secret()
	if err != nil {
		return prepared, errPairSecret
	}

	target, err := canonicalTarget(configPath)
	if err != nil {
		return prepared, errPairConfigTarget
	}
	prepared = PairPreparation{
		AppID:      in.Feishu.AppID,
		Secret:     secret,
		ConfigPath: target,
	}
	if in.Identity.OwnerOpenID != nil {
		prepared.ExpectedOwner = *in.Identity.OwnerOpenID
	}

	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return prepared, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return PairPreparation{}, errPairConfigTarget
	}
	if _, err := regularFile(target, false); err != nil {
		return PairPreparation{}, errPairExistingConfig
	}

	existing, err := config.Load(target)
	if err != nil {
		return PairPreparation{}, errPairExistingConfig
	}
	if existing.Feishu.AppID != in.Feishu.AppID {
		return PairPreparation{}, ErrPairIdentityConflict
	}
	if in.Identity.OwnerOpenID != nil && existing.Access.OwnerOpenID != *in.Identity.OwnerOpenID {
		return PairPreparation{}, ErrPairIdentityConflict
	}

	// Validate the existing credential reference before resolving it. This
	// keeps final-file symlinks rejected even when their target is otherwise
	// usable, while allowing aliases through symlinked parent directories.
	if _, err := regularFile(existing.Feishu.AppSecretFile, false); err != nil {
		return PairPreparation{}, errPairExistingConfig
	}
	existingSecretConfig := existing
	if _, err := existingSecretConfig.Secret(); err != nil {
		return PairPreparation{}, errPairExistingConfig
	}
	inputSecretPath, err := filepath.EvalSymlinks(secretPath)
	if err != nil {
		return PairPreparation{}, errPairSecret
	}
	existingSecretPath, err := filepath.EvalSymlinks(existing.Feishu.AppSecretFile)
	if err != nil {
		return PairPreparation{}, errPairExistingConfig
	}
	if inputSecretPath != existingSecretPath {
		return PairPreparation{}, ErrPairIdentityConflict
	}

	prepared.Existing = &existing
	prepared.ExpectedOwner = existing.Access.OwnerOpenID
	return prepared, nil
}
