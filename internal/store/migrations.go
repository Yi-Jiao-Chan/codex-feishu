package store

import (
	"database/sql"
	"errors"
)

const SchemaVersion = 13

const LegacyAdoptionSchema = `
CREATE TABLE legacy_adoptions (
 plan_id TEXT PRIMARY KEY, host_id TEXT NOT NULL UNIQUE REFERENCES deployment(host_id),
 app_id TEXT NOT NULL, source_digest TEXT NOT NULL, native_digest TEXT NOT NULL,
 adopted_at_ms INTEGER NOT NULL CHECK(adopted_at_ms>0)
);
`

// Preserve the M0 journals while adding production discovery and outbox slices.
// Never reset user_version on open or accept a newer database accidentally.
func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == 1 {
		return ErrLegacyAdoptionRequired
	}
	if err = migrateSchema(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateSchema is also used inside the reviewed legacy adoption transaction.
// Its caller must validate the old journals before entering this path.
func migrateSchema(tx *sql.Tx) error {
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	var err error
	if version > SchemaVersion {
		return errors.New("database schema is newer than this binary")
	}
	if version < 2 {
		_, err = tx.Exec(`
CREATE TABLE IF NOT EXISTS m0_operations(key TEXT PRIMARY KEY, kind TEXT NOT NULL, state TEXT NOT NULL, remote_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS m0_receipts(id TEXT PRIMARY KEY, app_id TEXT NOT NULL,message_id TEXT NOT NULL,thread_id TEXT NOT NULL,chat_id TEXT NOT NULL,body TEXT NOT NULL,client_id TEXT NOT NULL UNIQUE,queue_id TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,card_id TEXT NOT NULL DEFAULT '',action_token TEXT NOT NULL UNIQUE,UNIQUE(app_id,message_id));
CREATE TABLE IF NOT EXISTS m0_bindings(thread_id TEXT PRIMARY KEY,chat_id TEXT UNIQUE NOT NULL);
CREATE TABLE IF NOT EXISTS m0_actions(receipt_id TEXT PRIMARY KEY,kind TEXT NOT NULL,state TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS m0_reactions(receipt_id TEXT PRIMARY KEY,emoji TEXT NOT NULL DEFAULT '',reaction_id TEXT NOT NULL DEFAULT '',revision INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL DEFAULT 'confirmed');
CREATE TABLE IF NOT EXISTS deployment (
 host_id TEXT PRIMARY KEY, app_id TEXT NOT NULL,
 baseline_started_at_ms INTEGER NOT NULL, baseline_completed_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS thread_observations (
 host_id TEXT NOT NULL REFERENCES deployment(host_id), thread_id TEXT NOT NULL,
 origin TEXT NOT NULL CHECK(origin IN ('baseline','new','unknown')),
 proof TEXT NOT NULL DEFAULT 'baseline' CHECK(proof IN ('baseline','birth','draft','v7','ambiguous')),
 state TEXT NOT NULL CHECK(state IN ('pending','ignored','eligible','excluded')),
 cwd TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0,
 first_user_item_id TEXT NOT NULL DEFAULT '', project_root TEXT NOT NULL DEFAULT '',
 exclusion_reason TEXT NOT NULL DEFAULT '', PRIMARY KEY(host_id,thread_id)
);
CREATE TABLE IF NOT EXISTS create_intents (
 intent_id TEXT PRIMARY KEY, host_id TEXT NOT NULL, thread_id TEXT NOT NULL,
 owner_id TEXT NOT NULL, name TEXT NOT NULL, members_json TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('ready','dispatching','unknown','rejected','confirmed')),
 chat_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
 UNIQUE(host_id,thread_id), FOREIGN KEY(host_id,thread_id) REFERENCES thread_observations(host_id,thread_id)
);
CREATE TABLE IF NOT EXISTS bindings (
 host_id TEXT NOT NULL, thread_id TEXT NOT NULL, intent_id TEXT NOT NULL UNIQUE REFERENCES create_intents(intent_id),
 chat_id TEXT NOT NULL, PRIMARY KEY(host_id,thread_id), UNIQUE(host_id,chat_id)
);
PRAGMA user_version=2;
`)
		if err != nil {
			return err
		}
	}
	if version < 3 {
		_, err = tx.Exec(`
CREATE TABLE IF NOT EXISTS mirrors (
 host_id TEXT NOT NULL, thread_id TEXT NOT NULL, mirror_key TEXT NOT NULL,
 turn_id TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('human','progress','final')),
 card_json TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 1,
 terminal INTEGER NOT NULL DEFAULT 0, replay INTEGER NOT NULL DEFAULT 0,
 parent_key TEXT NOT NULL DEFAULT '', parent_message_id TEXT NOT NULL DEFAULT '',
 remote_id TEXT NOT NULL DEFAULT '', delivered_revision INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(host_id,thread_id,mirror_key),
 FOREIGN KEY(host_id,thread_id) REFERENCES thread_observations(host_id,thread_id)
);
CREATE TABLE IF NOT EXISTS outbound (
 host_id TEXT NOT NULL, thread_id TEXT NOT NULL, mirror_key TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('ready','dispatching','unknown','rejected','confirmed')),
 PRIMARY KEY(host_id,thread_id,mirror_key),
 FOREIGN KEY(host_id,thread_id,mirror_key) REFERENCES mirrors(host_id,thread_id,mirror_key)
);
CREATE TABLE IF NOT EXISTS outbound_attempts (
 attempt_id TEXT PRIMARY KEY, host_id TEXT NOT NULL, thread_id TEXT NOT NULL, mirror_key TEXT NOT NULL,
 revision INTEGER NOT NULL, operation TEXT NOT NULL CHECK(operation IN ('send','reply','update')),
 chat_id TEXT NOT NULL, parent_message_id TEXT NOT NULL, target_message_id TEXT NOT NULL,
 card_json TEXT NOT NULL, created_at_ms INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('dispatching','unknown','rejected','confirmed')),
 remote_id TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(host_id,thread_id,mirror_key) REFERENCES mirrors(host_id,thread_id,mirror_key)
);
CREATE INDEX IF NOT EXISTS outbound_attempts_mirror ON outbound_attempts(host_id,thread_id,mirror_key,created_at_ms);
CREATE TABLE IF NOT EXISTS history_watermarks (
 host_id TEXT NOT NULL, thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, item_id TEXT NOT NULL,
 observed_at_ms INTEGER NOT NULL,
 PRIMARY KEY(host_id,thread_id),
 FOREIGN KEY(host_id,thread_id) REFERENCES thread_observations(host_id,thread_id)
);
PRAGMA user_version=3;
`)
		if err != nil {
			return err
		}
	}
	if version < 4 {
		_, err = tx.Exec(`
ALTER TABLE mirrors ADD COLUMN retired INTEGER NOT NULL DEFAULT 0;
ALTER TABLE mirrors ADD COLUMN event_type TEXT NOT NULL DEFAULT '';
ALTER TABLE mirrors ADD COLUMN event_at INTEGER NOT NULL DEFAULT 0;
CREATE TABLE input_history_links (
 host_id TEXT NOT NULL, thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, item_id TEXT NOT NULL,
 receipt_id TEXT NOT NULL UNIQUE REFERENCES m0_receipts(id),
 PRIMARY KEY(host_id,thread_id,turn_id,item_id),
 FOREIGN KEY(host_id,thread_id) REFERENCES thread_observations(host_id,thread_id)
);
CREATE TABLE notification_rules (
 app_id TEXT NOT NULL, open_id TEXT NOT NULL, enabled INTEGER NOT NULL,
 snooze_until TEXT NOT NULL, quiet_start TEXT NOT NULL, quiet_end TEXT NOT NULL,
 timezone TEXT NOT NULL, revision INTEGER NOT NULL,
 PRIMARY KEY(app_id,open_id)
);
PRAGMA user_version=4;
`)
		if err != nil {
			return err
		}
	}
	if version < 5 {
		if _, err = tx.Exec(SubmissionSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=5"); err != nil {
			return err
		}
	}
	if version < 6 {
		if _, err = tx.Exec(DirectoryRuleSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=6"); err != nil {
			return err
		}
	}
	if version < 7 {
		if _, err = tx.Exec(MentionsSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=7"); err != nil {
			return err
		}
	}
	if version < 8 {
		if _, err = tx.Exec(CommandSchema); err != nil {
			return err
		}
		if _, err = tx.Exec(TaskControlSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=8"); err != nil {
			return err
		}
	}
	if version < 9 {
		if _, err = tx.Exec(PendingRequestsSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=9"); err != nil {
			return err
		}
	}
	if version < 10 {
		if _, err = tx.Exec(MirrorRetrySchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=10"); err != nil {
			return err
		}
	}
	if version < 11 {
		if _, err = tx.Exec(LegacyAdoptionSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=11"); err != nil {
			return err
		}
	}
	if version < 12 {
		if _, err = tx.Exec(ReactionRetrySchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=12"); err != nil {
			return err
		}
	}

	if version < 13 {
		if _, err = tx.Exec(GroupRetrySchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=13"); err != nil {
			return err
		}
	}

	return nil
}
