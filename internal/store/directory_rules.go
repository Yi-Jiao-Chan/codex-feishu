package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"

	"codex-feishu/internal/rules"
)

const DirectoryRuleSchema = `
CREATE TABLE directory_rules (
 host_id TEXT NOT NULL REFERENCES deployment(host_id), app_id TEXT NOT NULL,
 canonical_path TEXT NOT NULL, recursive INTEGER NOT NULL CHECK(recursive IN (0,1)),
 PRIMARY KEY(host_id,app_id,canonical_path,recursive)
);
`

// DirectoryRules lists one deployment's persisted rules. A missing directory is
// retained in the list: removing it from disk must not erase its mute setting.
func (s *Store) DirectoryRules(ctx context.Context, host, app string) ([]rules.DirectoryRule, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := directoryDeployment(ctx, tx, host, app); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT canonical_path,recursive FROM directory_rules WHERE host_id=? AND app_id=? ORDER BY canonical_path,recursive`, host, app)
	if err != nil {
		return nil, err
	}
	result := []rules.DirectoryRule{}
	for rows.Next() {
		var rule rules.DirectoryRule
		var recursive int
		if err := rows.Scan(&rule.Path, &recursive); err != nil {
			rows.Close()
			return nil, err
		}
		if !filepath.IsAbs(rule.Path) || filepath.Clean(rule.Path) != rule.Path || recursive != 0 && recursive != 1 {
			rows.Close()
			return nil, errors.New("invalid stored directory rule")
		}
		rule.Recursive = recursive == 1
		result = append(result, rule)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// ApplyDirectoryCommand is idempotent for mute/unmute. The caller owns operator
// authorization and allowed-root checks; storage never widens the supplied path.
func (s *Store) ApplyDirectoryCommand(ctx context.Context, host, app string, cmd rules.DirectoryCommand) error {
	switch cmd.Action {
	case rules.DirectoryMute, rules.DirectoryUnmute:
		canonical, err := rules.CanonicalDirectory(cmd.Rule.Path)
		if err != nil {
			return err
		}
		if canonical != cmd.Rule.Path {
			return errors.New("directory command must retain its validated canonical path")
		}
	case rules.DirectoryList:
		if cmd.Rule != (rules.DirectoryRule{}) {
			return errors.New("directory list does not accept a path")
		}
	default:
		return errors.New("unknown directory command")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := directoryDeployment(ctx, tx, host, app); err != nil {
		return err
	}
	switch cmd.Action {
	case rules.DirectoryMute:
		_, err = tx.ExecContext(ctx, `INSERT INTO directory_rules(host_id,app_id,canonical_path,recursive) VALUES(?,?,?,?) ON CONFLICT(host_id,app_id,canonical_path,recursive) DO NOTHING`, host, app, cmd.Rule.Path, cmd.Rule.Recursive)
	case rules.DirectoryUnmute:
		_, err = tx.ExecContext(ctx, `DELETE FROM directory_rules WHERE host_id=? AND app_id=? AND canonical_path=? AND recursive=?`, host, app, cmd.Rule.Path, cmd.Rule.Recursive)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func directoryDeployment(ctx context.Context, tx *sql.Tx, host, app string) error {
	if strings.TrimSpace(host) == "" || strings.TrimSpace(app) == "" {
		return errors.New("directory host and app must be nonempty")
	}
	var storedApp string
	err := tx.QueryRowContext(ctx, `SELECT app_id FROM deployment WHERE host_id=?`, host).Scan(&storedApp)
	if errors.Is(err, sql.ErrNoRows) || err == nil && storedApp != app {
		return ErrConflict
	}
	return err
}
