package setup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"github.com/pelletier/go-toml/v2"
)

type migrationNativeFixture struct {
	metadata      codexclient.Thread
	turns         []codexclient.Turn
	queue         []codexclient.Queued
	closed, reads int
	metadataReads int
	onMetadata    func(int)
}

func (n *migrationNativeFixture) Metadata(context.Context, string) (codexclient.Thread, error) {
	n.reads++
	n.metadataReads++
	if n.onMetadata != nil {
		n.onMetadata(n.metadataReads)
	}
	return n.metadata, nil
}
func (n *migrationNativeFixture) Turns(context.Context, string) ([]codexclient.Turn, error) {
	n.reads++
	return n.turns, nil
}
func (n *migrationNativeFixture) Queue(context.Context, string) ([]codexclient.Queued, error) {
	n.reads++
	return n.queue, nil
}
func (n *migrationNativeFixture) Close() { n.closed++ }

type migrationGatewayFixture struct {
	owner, bot string
	present    bool
	members    []string
}

func (g *migrationGatewayFixture) Bot(context.Context) (string, error) { return g.bot, nil }
func (g *migrationGatewayFixture) BotInChat(context.Context, string) (bool, error) {
	return g.present, nil
}
func (g *migrationGatewayFixture) MemberIDs(context.Context, string) ([]string, error) {
	return append([]string{}, g.members...), nil
}

func migrationFixture(t *testing.T) (MigrationPlanner, string, string, *migrationNativeFixture, *migrationGatewayFixture) {
	t.Helper()
	base, in, paths := fixture(t)
	plan, err := base.Build(context.Background(), in, paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(paths.ConfigFile), filepath.Dir(paths.DatabaseFile)} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	body, err := toml.Marshal(plan.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths.ConfigFile, body, 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`
CREATE TABLE m0_operations(key TEXT PRIMARY KEY,kind TEXT NOT NULL,state TEXT NOT NULL,remote_id TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL);
CREATE TABLE m0_bindings(thread_id TEXT PRIMARY KEY,chat_id TEXT UNIQUE NOT NULL);
CREATE TABLE m0_receipts(id TEXT PRIMARY KEY,app_id TEXT NOT NULL,message_id TEXT NOT NULL,thread_id TEXT NOT NULL,chat_id TEXT NOT NULL,body TEXT NOT NULL,client_id TEXT NOT NULL UNIQUE,queue_id TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,card_id TEXT NOT NULL DEFAULT '',action_token TEXT NOT NULL UNIQUE,UNIQUE(app_id,message_id));
CREATE TABLE m0_actions(receipt_id TEXT PRIMARY KEY,kind TEXT NOT NULL,state TEXT NOT NULL);
CREATE TABLE m0_reactions(receipt_id TEXT PRIMARY KEY,emoji TEXT NOT NULL DEFAULT '',reaction_id TEXT NOT NULL DEFAULT '',revision INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL DEFAULT 'confirmed');
INSERT INTO m0_bindings VALUES('thread','existing-group');
INSERT INTO m0_operations VALUES('m0.mirror.thread.u.0','feishu.mirror','confirmed','old-human',1);
INSERT INTO m0_operations VALUES('m0.final.thread.turn.0','feishu.final','confirmed','old-final',1);
PRAGMA user_version=1;
`)
	closeErr := raw.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if err = os.Chmod(paths.DatabaseFile, 0600); err != nil {
		t.Fatal(err)
	}
	n := &migrationNativeFixture{metadata: codexclient.Thread{ID: "thread", CWD: in.Discovery.AllowedRoots[0], CreatedAt: 1, Source: json.RawMessage(`"vscode"`), Preview: "private-preview"}}
	n.metadata.Status.Type = "idle"
	phase := "final_answer"
	n.turns = []codexclient.Turn{{ID: "turn", Status: "completed", ItemsView: "full", Items: []codexclient.Item{{ID: "u", Type: "userMessage", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"private-question"}`)}}, {ID: "a", Type: "agentMessage", Text: "private-answer", Phase: &phase}}}}
	g := &migrationGatewayFixture{owner: plan.Config.Access.OwnerOpenID, bot: "ou_migration_bot", present: true, members: []string{plan.Config.Access.OwnerOpenID}}
	p := MigrationPlanner{Probe: base.Probe, Connect: func(context.Context, string) (MigrationNative, error) { return n, nil }, Gateway: func(config.Config, string) MigrationGateway { return g }, Now: func() time.Time { return time.UnixMilli(1000000) }}
	return p, paths.ConfigFile, in.Artifact.BinaryPath, n, g
}

func migrationSchema(t *testing.T, database string) int {
	t.Helper()
	s, version, err := store.OpenReadOnly(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	return version
}

func TestMigrationPreviewStablePrivateAndApplyReusesRoute(t *testing.T) {
	p, cfg, binary, native, _ := migrationFixture(t)
	ctx := context.Background()
	a, err := p.Preview(ctx, cfg, binary)
	if err != nil {
		t.Fatal(err)
	}
	p.Now = func() time.Time { return time.UnixMilli(1000100) }
	b, err := p.Preview(ctx, cfg, binary)
	if err != nil || a.ID != b.ID || a.CardUpdates != 2 || a.Bindings != 1 || !a.CanApply || native.closed != 2 {
		t.Fatal("preview unstable or reader leaked", err)
	}
	public, err := json.Marshal(a)
	journal, journalErr := os.ReadFile(a.DatabaseFile + ".migration-state.json")
	if err != nil || journalErr != nil {
		t.Fatal(err, journalErr)
	}
	for _, sensitive := range []string{"private-question", "private-answer", "private-preview", "fake-secret-for-local-tests"} {
		if strings.Contains(string(public), sensitive) || strings.Contains(string(journal), sensitive) {
			t.Fatal("plan exposed private data")
		}
	}
	if migrationSchema(t, a.DatabaseFile) != 1 {
		t.Fatal("preview migrated schema")
	}
	if _, err := os.Stat(a.BackupFile); !os.IsNotExist(err) {
		t.Fatal("preview created backup", err)
	}
	if err := CheckApplyReady(cfg); err != nil {
		t.Fatal("planned artifact blocks unchanged M0 config", err)
	}
	result, err := p.Apply(ctx, cfg, binary, a.ID)
	if err != nil || !result.Changed || result.State != "migration-applied" {
		t.Fatal("apply failed", err)
	}
	if migrationSchema(t, a.DatabaseFile) != store.SchemaVersion || migrationSchema(t, a.BackupFile) != 1 {
		t.Fatal("backup or migration schema wrong")
	}
	if err := CheckApplyReady(cfg); err != nil {
		t.Fatal("completed migration blocks startup", err)
	}
	s, _, err := store.OpenReadOnly(ctx, a.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	binding, err := s.Binding(ctx, a.adoption.HostID, "thread")
	if err != nil || binding.ChatID != "existing-group" {
		t.Fatal("original group lost", err)
	}
	mirrors, err := s.Mirrors(ctx, a.adoption.HostID, "thread")
	if err != nil || len(mirrors) != 2 {
		t.Fatal("history card identities lost", err)
	}
	for _, m := range mirrors {
		if m.RemoteID == "" || m.State != "ready" || !m.Replay {
			t.Fatal("historical send could be created")
		}
	}
	reads := native.reads
	p.Connect = func(context.Context, string) (MigrationNative, error) {
		t.Fatal("committed replay reread native or rebuilt seeds")
		return nil, nil
	}
	result, err = p.Apply(ctx, cfg, binary, a.ID)
	if err != nil || result.Changed || result.State != "migration-unchanged" || native.reads != reads {
		t.Fatal("replay not idempotent", err)
	}
}

func TestMigrationFaultRecoveryBeforeAndAfterCommit(t *testing.T) {
	for _, stage := range []string{"prepared", "backup", "committed"} {
		t.Run(stage, func(t *testing.T) {
			p, cfg, binary, _, _ := migrationFixture(t)
			ctx := context.Background()
			plan, err := p.Preview(ctx, cfg, binary)
			if err != nil {
				t.Fatal(err)
			}
			p.checkpoint = func(at string) error {
				if at == stage {
					return errors.New("fixture interruption")
				}
				return nil
			}
			if _, err = p.Apply(ctx, cfg, binary, plan.ID); !errors.Is(err, ErrMigrationPending) {
				t.Fatal("interruption not recoverable", err)
			}
			if err := CheckApplyReady(cfg); !errors.Is(err, ErrMigrationPending) {
				t.Fatal("unfinished migration permits service start", err)
			}
			copyCfg := filepath.Join(filepath.Dir(cfg), "copied.toml")
			body, err := os.ReadFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(copyCfg, body, 0600); err != nil {
				t.Fatal(err)
			}
			if err := CheckApplyReady(copyCfg); !errors.Is(err, ErrMigrationPending) {
				t.Fatal("copied config bypasses database migration marker", err)
			}
			p.checkpoint = nil
			if stage == "committed" {
				p.Connect = func(context.Context, string) (MigrationNative, error) {
					t.Fatal("post-commit recovery may not replay adoption")
					return nil, nil
				}
				db, err := store.OpenExisting(ctx, plan.DatabaseFile, store.SchemaVersion)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.DB.Exec(`UPDATE mirrors SET card_json='{"body":"later live card"}',revision=revision+1; UPDATE outbound SET state='confirmed';`); err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			result, err := p.Apply(ctx, cfg, binary, plan.ID)
			if err != nil || stage == "committed" && result.Changed {
				t.Fatal("recovery failed or repeated commit", err)
			}
			if err := CheckApplyReady(cfg); err != nil {
				t.Fatal("recovery did not release startup fence", err)
			}
			if stage == "committed" {
				db, _, err := store.OpenReadOnly(ctx, plan.DatabaseFile)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				mirrors, err := db.Mirrors(ctx, plan.adoption.HostID, "thread")
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range mirrors {
					if m.Revision != 2 || !strings.Contains(m.Card, "later live card") {
						t.Fatal("recovery overwrote post-commit activity")
					}
				}
			}
		})
	}
}

func TestMigrationRejectsLocksAndUnreleasedArtifactBeforeBackup(t *testing.T) {
	for _, kind := range []string{"application", "database", "unreleased"} {
		t.Run(kind, func(t *testing.T) {
			p, cfg, binary, native, _ := migrationFixture(t)
			if kind == "unreleased" {
				probe := p.Probe
				p.Probe = func(ctx context.Context, path string) (Environment, error) {
					env, err := probe(ctx, path)
					env.Artifact.Released = false
					return env, err
				}
			}
			plan, err := p.Preview(context.Background(), cfg, binary)
			if err != nil {
				t.Fatal(err)
			}
			want := ErrMigrationLocked
			if kind == "unreleased" {
				want = ErrUnreleased
			}
			if kind == "application" {
				lock, err := store.AcquireApplication(plan.adoption.AppID)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			if kind == "database" {
				lock, err := store.AcquireListener(plan.DatabaseFile)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			reads := native.reads
			if _, err := p.Apply(context.Background(), cfg, binary, plan.ID); !errors.Is(err, want) {
				t.Fatal("unsafe apply permitted", err)
			}
			if native.reads != reads || migrationSchema(t, plan.DatabaseFile) != 1 {
				t.Fatal("rejected apply read native or migrated")
			}
			if _, err := os.Stat(plan.BackupFile); !os.IsNotExist(err) {
				t.Fatal("rejected apply created backup", err)
			}
		})
	}
}

func TestMigrationFreshNativeProofAndIdentityInvalidateOldPlan(t *testing.T) {
	for _, cause := range []string{"native", "after backup", "config", "artifact", "credential", "membership", "expired", "journal tamper"} {
		t.Run(cause, func(t *testing.T) {
			p, cfg, binary, n, g := migrationFixture(t)
			plan, err := p.Preview(context.Background(), cfg, binary)
			if err != nil {
				t.Fatal(err)
			}
			switch cause {
			case "native":
				n.turns[0].Items[1].Text = "new native answer"
			case "after backup":
				p.checkpoint = func(at string) error {
					if at == "backup" {
						n.turns[0].Items[1].Text = "changed during backup"
					}
					return nil
				}
			case "config":
				f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				f.WriteString("\n# modified\n")
				f.Close()
			case "artifact":
				os.WriteFile(binary, []byte("modified executable fixture"), 0700)
			case "credential":
				c, err := config.Load(cfg)
				if err != nil {
					t.Fatal(err)
				}
				os.WriteFile(c.Feishu.AppSecretFile, []byte("other-test-secret"), 0600)
			case "membership":
				g.members = append(g.members, "ou_other_member")
			case "expired":
				p.Now = func() time.Time { return time.UnixMilli(2000000) }
			case "journal tamper":
				j := plan.journal
				j.SourceDigest = strings.Repeat("b", 64)
				if err := atomicFile(plan.DatabaseFile+".migration-state.json", encoded(j)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := p.Apply(context.Background(), cfg, binary, plan.ID); !errors.Is(err, ErrStalePlan) {
				t.Fatal("changed proof accepted", err)
			}
			if migrationSchema(t, plan.DatabaseFile) != 1 {
				t.Fatal("stale plan migrated")
			}
			if cause == "after backup" {
				j, found, err := readMigrationJournal(plan.DatabaseFile)
				if err != nil || !found || j.State != "planned" {
					t.Fatal("known uncommitted stale plan could not be replaced", err)
				}
				p.checkpoint = nil
				if next, err := p.Preview(context.Background(), cfg, binary); err != nil || next.ID == plan.ID {
					t.Fatal("new preview failed", err)
				}
			}
		})
	}
}

func TestMigrationPreviewRefusesUnknownActiveOrUnauthorizedTargets(t *testing.T) {
	for _, cause := range []string{"queue", "active", "child", "fork", "outside", "no bot", "no owner", "metadata race"} {
		t.Run(cause, func(t *testing.T) {
			p, cfg, binary, n, g := migrationFixture(t)
			switch cause {
			case "queue":
				n.queue = []codexclient.Queued{{ID: "pending"}}
			case "active":
				n.metadata.Status.Type = "active"
			case "child":
				parent := "parent"
				n.metadata.ParentThreadID = &parent
			case "fork":
				parent := "parent"
				n.metadata.ForkedFromID = &parent
			case "outside":
				n.metadata.CWD = t.TempDir()
			case "no bot":
				g.present = false
			case "no owner":
				g.members = nil
			case "metadata race":
				n.onMetadata = func(count int) {
					if count == 2 {
						n.metadata.UpdatedAt++
					}
				}
			}
			if _, err := p.Preview(context.Background(), cfg, binary); err == nil {
				t.Fatal("unsafe migration plan generated")
			}
			c, err := config.Load(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if migrationSchema(t, c.Storage.DatabaseFile) != 1 {
				t.Fatal("preview changed schema")
			}
			if _, err := os.Stat(c.Storage.DatabaseFile + ".migration-state.json"); !os.IsNotExist(err) {
				t.Fatal("failed preview published a plan", err)
			}
		})
	}
}

func TestMigrationArtifactLockCoversCopiedConfigurations(t *testing.T) {
	p, cfg, binary, _, _ := migrationFixture(t)
	ctx := context.Background()
	plan, err := p.Preview(ctx, cfg, binary)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	copyCfg := cfg + ".copy"
	if err := os.WriteFile(copyCfg, body, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := migrationLock(plan.DatabaseFile + ".migration.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := p.Preview(ctx, copyCfg, binary); !errors.Is(err, ErrMigrationLocked) {
		t.Fatal("copied preview bypassed shared lock", err)
	}
	if _, err := p.Apply(ctx, copyCfg, binary, plan.ID); !errors.Is(err, ErrMigrationLocked) {
		t.Fatal("copied apply bypassed shared lock", err)
	}
	if _, err := p.Apply(ctx, cfg, binary, plan.ID); !errors.Is(err, ErrMigrationLocked) {
		t.Fatal("apply bypassed artifact lock", err)
	}
	j, _, err := readMigrationJournal(plan.DatabaseFile)
	if err != nil || j.PlanID != plan.ID || j.State != "planned" || migrationSchema(t, plan.DatabaseFile) != 1 {
		t.Fatal("blocked operation changed evidence", err)
	}
}

func TestMigrationApplyKeepsArtifactLockUntilCommitMarker(t *testing.T) {
	p, cfg, binary, _, _ := migrationFixture(t)
	ctx := context.Background()
	plan, err := p.Preview(ctx, cfg, binary)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	copyCfg := cfg + ".copy"
	if err := os.WriteFile(copyCfg, body, 0600); err != nil {
		t.Fatal(err)
	}
	var stages []string
	other := p
	p.checkpoint = func(stage string) error {
		stages = append(stages, stage)
		if _, err := other.Preview(ctx, copyCfg, binary); !errors.Is(err, ErrMigrationLocked) {
			t.Fatal("preview overwrote in-flight plan at", stage, err)
		}
		return nil
	}
	if _, err := p.Apply(ctx, cfg, binary, plan.ID); err != nil {
		t.Fatal(err)
	}
	if strings.Join(stages, ",") != "prepared,backup,committed" {
		t.Fatal("missing fault checkpoints", stages)
	}
	j, _, err := readMigrationJournal(plan.DatabaseFile)
	if err != nil || j.PlanID != plan.ID || j.State != "applied" {
		t.Fatal("committed artifact lost", err)
	}
}
