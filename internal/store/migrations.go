package store

import (
	"database/sql"
	"errors"
)

const SchemaVersion = 35

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

	if version < 14 {
		if _, err = tx.Exec(CommandRetrySchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=14"); err != nil {
			return err
		}
	}

	if version < 15 {
		if _, err = tx.Exec(ReminderRetrySchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=15"); err != nil {
			return err
		}
	}

	if version < 16 {
		if _, err = tx.Exec(MirrorReadbackSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=16"); err != nil {
			return err
		}
	}

	if version < 17 {
		if _, err = tx.Exec(CardReadbackSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=17"); err != nil {
			return err
		}
	}

	if version < 18 {
		if _, err = tx.Exec(VerificationSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=18"); err != nil {
			return err
		}
	}

	if version < 19 {
		if _, err = tx.Exec(VerificationGroupSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=19"); err != nil {
			return err
		}
	}

	if version < 20 {
		if _, err = tx.Exec(VerificationNativeSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=20"); err != nil {
			return err
		}
	}

	if version < 21 {
		if _, err = tx.Exec(VerificationBindingSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=21"); err != nil {
			return err
		}
	}

	if version < 22 {
		if _, err = tx.Exec(`ALTER TABLE mirrors ADD COLUMN progress_observed_at_json TEXT NOT NULL DEFAULT 'null'`); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=22"); err != nil {
			return err
		}
	}

	if version < 23 {
		// Requests already attempted retain their original description and UUID.
		if _, err = tx.Exec(`ALTER TABLE create_intents ADD COLUMN description TEXT NOT NULL DEFAULT '跟进同一个 Codex 会话的任务与公开回复';
UPDATE create_intents SET description='' WHERE state='ready' AND NOT EXISTS(SELECT 1 FROM group_attempts a WHERE a.intent_id=create_intents.intent_id);
PRAGMA user_version=23;`); err != nil {
			return err
		}
	}

	if version < 24 {
		if _, err = tx.Exec(InteractionsSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=24"); err != nil {
			return err
		}
	}
	if version < 25 {
		if _, err = tx.Exec(InteractionAnswersSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=25"); err != nil {
			return err
		}
	}
	if version < 26 {
		var payloadColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('m0_receipts') WHERE name='input_json'`).Scan(&payloadColumn); err != nil {
			return err
		}
		if payloadColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE m0_receipts ADD COLUMN input_json TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(AttachmentsSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=26"); err != nil {
			return err
		}
	}

	if version < 27 {
		var targetColumn int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('bindings') WHERE name='follow_mode'`).Scan(&targetColumn); err != nil {
			return err
		}
		if targetColumn == 0 {
			if _, err = tx.Exec(FollowTargetsSchema); err != nil {
				return err
			}
			rows, checkErr := tx.Query("PRAGMA foreign_key_check")
			if checkErr != nil {
				return checkErr
			}
			bad := rows.Next()
			checkErr = rows.Err()
			rows.Close()
			if checkErr != nil {
				return checkErr
			}
			if bad {
				return errors.New("follow target migration violates foreign keys")
			}
			// DROP/rename leaves obsolete deferred parent counters even though all
			// current references are valid. Clear them only after the full check.
			if _, err = tx.Exec(`PRAGMA defer_foreign_keys=OFF`); err != nil {
				return err
			}
		}
		var outboundTargetColumn int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('outbound_attempts') WHERE name='follow_mode'`).Scan(&outboundTargetColumn); err != nil {
			return err
		}
		if outboundTargetColumn == 0 {
			if _, err = tx.Exec(`ALTER TABLE outbound_attempts ADD COLUMN follow_mode TEXT NOT NULL DEFAULT 'group';
ALTER TABLE outbound_attempts ADD COLUMN root_message_id TEXT NOT NULL DEFAULT '';
ALTER TABLE outbound_attempts ADD COLUMN platform_thread_id TEXT NOT NULL DEFAULT '';`); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`PRAGMA user_version=27`); err != nil {
			return err
		}
	}

	if version < 28 {
		var replyColumn int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('m0_receipts') WHERE name='reply_message_id'`).Scan(&replyColumn); err != nil {
			return err
		}
		if replyColumn == 0 {
			if _, err = tx.Exec(`ALTER TABLE m0_receipts ADD COLUMN reply_message_id TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(CreationSchema); err != nil {
			return err
		}
		if _, err = tx.Exec(FollowAttemptSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=28"); err != nil {
			return err
		}
	}

	if version < 29 {
		if err = migrateCreationWorktrees(tx); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=29"); err != nil {
			return err
		}
	}
	if version < 30 {
		var present int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('creation_drafts') WHERE name='attachments_json'`).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			if _, err = tx.Exec(`ALTER TABLE creation_drafts ADD COLUMN attachments_json TEXT NOT NULL DEFAULT '[]'`); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("PRAGMA user_version=30"); err != nil {
			return err
		}
	}

	if version < 31 {
		if _, err = tx.Exec(COTSchema); err != nil {
			return err
		}
		for _, column := range []struct{ name, definition string }{
			{"cot_step", "TEXT NOT NULL DEFAULT ''"},
			{"cot_id", "TEXT NOT NULL DEFAULT ''"},
			{"cot_events_json", "TEXT NOT NULL DEFAULT '[]'"},
			{"cot_state_json", "TEXT NOT NULL DEFAULT '{}'"},
			{"cot_state_before", "TEXT NOT NULL DEFAULT ''"},
			{"cot_caught_up", "INTEGER NOT NULL DEFAULT 0"},
		} {
			var present int
			if err = tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('outbound_attempts') WHERE name=?`, column.name).Scan(&present); err != nil {
				return err
			}
			if present == 0 {
				if _, err = tx.Exec(`ALTER TABLE outbound_attempts ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
					return err
				}
			}
		}
		if _, err = tx.Exec("PRAGMA user_version=31"); err != nil {
			return err
		}
	}
	if version < 32 {
		if _, err = tx.Exec(SessionSettingsSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=32"); err != nil {
			return err
		}
	}
	if version < 33 {
		if _, err = tx.Exec(SessionSettingsFormsSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=33"); err != nil {
			return err
		}
	}
	if version < 34 {
		if _, err = tx.Exec(WorktreeRestoreSchema); err != nil {
			return err
		}
		if _, err = tx.Exec("PRAGMA user_version=34"); err != nil {
			return err
		}
	}
	if version < 35 {
		for _, column := range []struct {
			table, name, definition string
		}{
			{"outbound", "retry_of_attempt_id", "TEXT NOT NULL DEFAULT ''"},
			{"outbound_attempts", "retry_of_attempt_id", "TEXT NOT NULL DEFAULT ''"},
			{"outbound_attempts", "rejection_code", "INTEGER NOT NULL DEFAULT 0"},
			{"outbound_attempts", "rejection_http_status", "INTEGER NOT NULL DEFAULT 0"},
			{"outbound_attempts", "rejection_request_id", "TEXT NOT NULL DEFAULT ''"},
		} {
			var present int
			if err = tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, column.table, column.name).Scan(&present); err != nil {
				return err
			}
			if present == 0 {
				if _, err = tx.Exec(`ALTER TABLE ` + column.table + ` ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
					return err
				}
			}
		}
		if _, err = tx.Exec("PRAGMA user_version=35"); err != nil {
			return err
		}
	}
	return nil
}
