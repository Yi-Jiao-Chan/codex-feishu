package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"time"

	"codex-feishu/compat"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
	messagesync "codex-feishu/internal/sync"
)

var ErrMigrationPending = errors.New("M0 migration needs recovery; preserve its plan and backup")
var ErrMigrationLocked = errors.New("migration deployment is busy; stop only its bridge before applying")

// Only native reads and Feishu GETs are available to migration planning.
type MigrationNative interface {
	Metadata(context.Context, string) (codexclient.Thread, error)
	Turns(context.Context, string) ([]codexclient.Turn, error)
	Queue(context.Context, string) ([]codexclient.Queued, error)
	Close()
}
type MigrationGateway interface {
	Bot(context.Context) (string, error)
	BotInChat(context.Context, string) (bool, error)
	MemberIDs(context.Context, string) ([]string, error)
}
type MigrationPlanner struct {
	Probe      func(context.Context, string) (Environment, error)
	Connect    func(context.Context, string) (MigrationNative, error)
	Gateway    func(config.Config, string) MigrationGateway
	Now        func() time.Time
	checkpoint func(string) error // Fault injection only; CLI never sets it.
}

// CLI output is a summary. Neither raw histories nor adoption seeds leave this
// module. The private journal contains hashes, not credential or message bodies.
type MigrationPlan struct {
	ID           string `json:"plan_id"`
	ConfigFile   string `json:"config_file"`
	DatabaseFile string `json:"database_file"`
	BackupFile   string `json:"backup_file"`
	CanApply     bool   `json:"can_apply"`
	FromSchema   int    `json:"from_schema"`
	ToSchema     int    `json:"to_schema"`
	Bindings     int    `json:"bindings"`
	Receipts     int    `json:"receipts"`
	CardUpdates  int    `json:"card_updates"`
	journal      migrationJournal
	adoption     store.LegacyAdoption
}
type MigrationResult struct {
	PlanID       string `json:"plan_id"`
	DatabaseFile string `json:"database_file"`
	BackupFile   string `json:"backup_file"`
	Changed      bool   `json:"changed"`
	State        string `json:"state"`
}
type migrationJournal struct {
	Version                                                        int
	PlanID, IdentityDigest, AdoptionID, SourceDigest, NativeDigest string
	ConfigFile, BinaryFile, DatabaseFile, BackupFile               string
	HostID, AppID                                                  string
	StartedAtMS                                                    int64
	TargetSchema                                                   int
	State                                                          string
}

func (j migrationJournal) id() string {
	return digest(encoded([]any{j.Version, j.IdentityDigest, j.AdoptionID, j.TargetSchema}))
}
func (p MigrationPlanner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func privateMigrationFile(path string) (string, error) {
	physical, err := canonicalTarget(path)
	if err != nil {
		return "", errors.New("private migration file unavailable")
	}
	for _, item := range []struct {
		path      string
		directory bool
	}{{filepath.Dir(physical), true}, {physical, false}} {
		info, err := os.Lstat(item.path)
		if err != nil || item.directory && (!info.IsDir() || info.Mode().Perm() != 0700) || !item.directory && (!info.Mode().IsRegular() || info.Mode().Perm() != 0600) {
			return "", errors.New("migration needs private owned directories and regular files")
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Geteuid()) {
			return "", errors.New("migration file owner differs")
		}
	}
	return physical, nil
}

func migrationConfig(path string) (config.Config, string, error) {
	physical, err := privateMigrationFile(path)
	if err != nil {
		return config.Config{}, "", err
	}
	c, err := config.Load(physical)
	if err != nil {
		return c, physical, errors.New("migration configuration invalid")
	}
	db, err := privateMigrationFile(c.Storage.DatabaseFile)
	if err != nil {
		return c, physical, err
	}
	c.Storage.DatabaseFile = db
	return c, physical, nil
}

func (p MigrationPlanner) identity(ctx context.Context, configFile, binary string) (config.Config, Environment, string, error) {
	c, physical, err := migrationConfig(configFile)
	if err != nil {
		return c, Environment{}, "", err
	}
	if err = checkConfigurationApplyReady(physical); err != nil {
		return c, Environment{}, "", err
	}
	configBytes, err := regularFile(physical, false)
	if err != nil {
		return c, Environment{}, "", err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return c, Environment{}, "", errors.New("migration artifact unavailable")
	}
	artifact, err := regularFile(binary, true)
	if err != nil {
		return c, Environment{}, "", err
	}
	secret, err := c.Secret()
	if err != nil {
		return c, Environment{}, "", err
	}
	if _, err = regularFile(c.Feishu.AppSecretFile, false); err != nil {
		return c, Environment{}, "", err
	}
	probe := p.Probe
	if probe == nil {
		probe = Probe
	}
	env, err := probe(ctx, binary)
	if err != nil {
		return c, env, "", errors.New("migration artifact or existing daemon unavailable")
	}
	if env.Daemon.Status != "running" || !compat.VerifiedCodexVersion(env.Daemon.AppServerVersion) || env.Artifact.CodexProtocol != "0.162.0" || env.Artifact.ConfigSchema != 1 || !has(env.Artifact.Commands, "serve") || !filepath.IsAbs(env.CLIPath) {
		return c, env, "", errors.New("migration compatibility is unverified")
	}
	if c.Codex.CLIPath != "auto" {
		configured, err := filepath.EvalSymlinks(c.Codex.CLIPath)
		actual, actualErr := filepath.EvalSymlinks(env.CLIPath)
		if err != nil || actualErr != nil || configured != actual {
			return c, env, "", errors.New("migration daemon differs from configuration")
		}
	}
	stable := env
	stable.Daemon.SocketPath = "" // Same verified daemon may restart on another socket.
	return c, env, digest(encoded([]any{physical, c.Storage.DatabaseFile, binary, digest(configBytes), digest(artifact), digest([]byte(secret)), stable})), nil
}

func (p MigrationPlanner) build(ctx context.Context, configFile, binary string, startedMS int64) (MigrationPlan, error) {
	var plan MigrationPlan
	c, env, identity, err := p.identity(ctx, configFile, binary)
	if err != nil {
		return plan, err
	}
	configFile, _ = canonicalTarget(configFile)
	binary, _ = filepath.EvalSymlinks(binary)
	if startedMS <= 0 || startedMS > p.now().UnixMilli() {
		return plan, ErrStalePlan
	}
	s, version, err := store.OpenReadOnly(ctx, c.Storage.DatabaseFile)
	if err != nil {
		return plan, errors.New("M0 inventory unavailable")
	}
	defer s.Close()
	if version != 1 {
		return plan, errors.New("migration preview requires schema 1; nothing upgraded")
	}
	i, err := s.LegacyInventory(ctx)
	if err != nil {
		return plan, err
	}
	if err = i.Quiescent(c.Feishu.AppID); err != nil {
		return plan, err
	}
	if len(i.Bindings) == 0 {
		return plan, errors.New("M0 has no existing bindings to adopt")
	}
	connect := p.Connect
	if connect == nil {
		connect = func(ctx context.Context, socket string) (MigrationNative, error) {
			return codexclient.Connect(ctx, socket)
		}
	}
	native, err := connect(ctx, env.Daemon.SocketPath)
	if err != nil {
		return plan, errors.New("migration native reader unavailable")
	}
	defer native.Close()
	secret, err := c.Secret()
	if err != nil {
		return plan, err
	}
	factory := p.Gateway
	if factory == nil {
		factory = func(c config.Config, secret string) MigrationGateway { return feishu.New(c.Feishu.AppID, secret) }
	}
	g := factory(c, secret)
	bot, err := g.Bot(ctx)
	if err != nil || !config.ValidOpenID(bot) {
		return plan, errors.New("migration bot identity unverified")
	}
	adoption := store.LegacyAdoption{HostID: c.Host.ID, AppID: c.Feishu.AppID, OwnerID: c.Access.OwnerOpenID, SourceDigest: i.Digest(), StartedAtMS: startedMS}
	var snapshots []any
	for _, binding := range i.Bindings {
		t, err := native.Metadata(ctx, binding.ThreadID)
		if err != nil || t.ID != binding.ThreadID || !t.InteractiveMain() || t.ForkedFromID != nil || t.CreatedAt <= 0 || t.CreatedAt > startedMS/1000 || t.Status.Type != "idle" && t.Status.Type != "notLoaded" {
			return plan, store.ErrLegacyPending
		}
		scope, err := rules.ResolveScope(ctx, t.CWD, c.Discovery.AllowedRoots)
		if err != nil || !scope.Allowed {
			return plan, errors.New("M0 directory is outside verified allowed roots")
		}
		queue, err := native.Queue(ctx, t.ID)
		if err != nil || len(queue) != 0 {
			return plan, store.ErrLegacyPending
		}
		turns, err := native.Turns(ctx, t.ID)
		if err != nil {
			return plan, errors.New("migration needs complete native history")
		}
		first := ""
		for _, turn := range turns {
			for _, item := range turn.Items {
				if first == "" && item.Type == "userMessage" {
					first = item.ID
				}
			}
		}
		seed, err := messagesync.PlanLegacyThread(c.Host.ID, store.LegacyThreadSeed{ThreadID: t.ID, ChatID: binding.ChatID, CWD: scope.CWD, ProjectRoot: scope.ProjectRoot, FirstUserItemID: first, CreatedAt: t.CreatedAt}, turns, i, startedMS, 0)
		if err != nil {
			return plan, err
		}
		present, err := g.BotInChat(ctx, binding.ChatID)
		if err != nil || !present {
			return plan, errors.New("M0 bot group membership unverified")
		}
		members, err := g.MemberIDs(ctx, binding.ChatID)
		if err != nil || !has(members, c.Access.OwnerOpenID) {
			return plan, errors.New("M0 owner group membership unverified")
		}
		members = unique(members)
		sort.Strings(members)
		later, err := native.Metadata(ctx, t.ID)
		if err != nil || !reflect.DeepEqual(t, later) {
			return plan, ErrStalePlan
		}
		queue, err = native.Queue(ctx, t.ID)
		if err != nil || len(queue) != 0 {
			return plan, store.ErrLegacyPending
		}
		adoption.Threads = append(adoption.Threads, seed)
		snapshots = append(snapshots, []any{t, turns, scope, bot, members})
		plan.CardUpdates += len(seed.Mirrors)
	}
	adoption.NativeDigest = digest(encoded(snapshots))
	current, err := s.LegacyInventory(ctx)
	if err != nil || current.Digest() != i.Digest() {
		return plan, ErrStalePlan
	}
	_, _, latest, err := p.identity(ctx, configFile, binary)
	if err != nil || latest != identity {
		return plan, ErrStalePlan
	}
	j := migrationJournal{Version: 1, IdentityDigest: identity, AdoptionID: adoption.ID(), SourceDigest: i.Digest(), NativeDigest: adoption.NativeDigest, ConfigFile: configFile, BinaryFile: binary, DatabaseFile: c.Storage.DatabaseFile, HostID: c.Host.ID, AppID: c.Feishu.AppID, StartedAtMS: startedMS, TargetSchema: store.SchemaVersion, State: "planned"}
	j.PlanID = j.id()
	j.BackupFile = j.DatabaseFile + ".m0-backup." + j.PlanID[:16] + ".db"
	plan.ID, plan.ConfigFile, plan.DatabaseFile, plan.BackupFile, plan.CanApply = j.PlanID, configFile, j.DatabaseFile, j.BackupFile, env.Artifact.Released
	plan.FromSchema, plan.ToSchema, plan.Bindings, plan.Receipts = 1, store.SchemaVersion, len(i.Bindings), len(i.Receipts)
	plan.journal, plan.adoption = j, adoption
	return plan, nil
}

func migrationConfigLock(target string) (*os.File, error) {
	return migrationLock(target + ".setup.lock")
}

// Shared across copied configurations that name the same physical database.
// This lock reserves only the plan artifact, so preview can coexist with M0.
func migrationLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, ErrMigrationLocked
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, ErrMigrationLocked
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || owner.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, ErrMigrationLocked
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrMigrationLocked
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		f.Close()
		return nil, ErrMigrationLocked
	}
	return f, nil
}

// Preview saves only a private review artifact. It does not acquire a receiver,
// stop a bridge, create a backup, migrate SQLite or submit native input.
func (p MigrationPlanner) Preview(ctx context.Context, configFile, binary string) (MigrationPlan, error) {
	c, target, err := migrationConfig(configFile)
	if err != nil {
		return MigrationPlan{}, err
	}
	lock, err := migrationConfigLock(target)
	if err != nil {
		return MigrationPlan{}, err
	}
	defer lock.Close()
	c, currentTarget, err := migrationConfig(target)
	if err != nil || currentTarget != target {
		return MigrationPlan{}, ErrStalePlan
	}
	artifactLock, err := migrationLock(c.Storage.DatabaseFile + ".migration.lock")
	if err != nil {
		return MigrationPlan{}, err
	}
	defer artifactLock.Close()
	old, found, err := readMigrationJournal(c.Storage.DatabaseFile)
	if err != nil {
		return MigrationPlan{}, err
	}
	if found && old.State != "planned" {
		return MigrationPlan{}, ErrMigrationPending
	}
	started := p.now().UnixMilli()
	// Repeated preview of identical evidence retains its original plan ID.
	if found && started-old.StartedAtMS <= int64((15*time.Minute)/time.Millisecond) {
		started = old.StartedAtMS
	}
	plan, err := p.build(ctx, target, binary, started)
	if err != nil {
		return plan, err
	}
	if plan.DatabaseFile != c.Storage.DatabaseFile {
		return MigrationPlan{}, ErrStalePlan
	}
	if err := atomicFile(plan.DatabaseFile+".migration-state.json", encoded(plan.journal)); err != nil {
		return plan, err
	}
	return plan, nil
}

func readMigrationJournal(database string) (migrationJournal, bool, error) {
	var j migrationJournal
	path := database + ".migration-state.json"
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return j, false, nil
	}
	if _, err := privateMigrationFile(path); err != nil {
		return j, true, ErrMigrationPending
	}
	body, err := regularFile(path, false)
	if err != nil {
		return j, true, ErrMigrationPending
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	var extra any
	if d.Decode(&j) != nil || !errors.Is(d.Decode(&extra), io.EOF) || j.Version != 1 || !validDigest(j.PlanID) || !validDigest(j.IdentityDigest) || !validDigest(j.AdoptionID) || !validDigest(j.SourceDigest) || !validDigest(j.NativeDigest) || j.PlanID != j.id() || j.TargetSchema != store.SchemaVersion || j.StartedAtMS <= 0 || j.HostID == "" || !config.ValidAppID(j.AppID) || j.DatabaseFile != database || !filepath.IsAbs(j.ConfigFile) || !filepath.IsAbs(j.BinaryFile) || j.BackupFile != database+".m0-backup."+j.PlanID[:16]+".db" || j.State != "planned" && j.State != "prepared" && j.State != "applied" {
		return j, true, ErrMigrationPending
	}
	return j, true, nil
}

func sameMigrationPlan(a, b migrationJournal) bool {
	a.State, b.State = "", ""
	return a == b
}

func migrationBackupReady(ctx context.Context, j migrationJournal) error {
	if _, err := privateMigrationFile(j.BackupFile); err != nil {
		return ErrMigrationPending
	}
	s, version, err := store.OpenReadOnly(ctx, j.BackupFile)
	if err != nil {
		return ErrMigrationPending
	}
	defer s.Close()
	i, err := s.LegacyInventory(ctx)
	if err != nil || version != 1 || i.Digest() != j.SourceDigest {
		return ErrMigrationPending
	}
	return nil
}

func checkMigrationReady(database, host, app string) error {
	physical, err := deploymentDatabasePath(database)
	if err != nil {
		return ErrMigrationPending
	}
	j, found, err := readMigrationJournal(physical)
	if err != nil {
		return err
	}
	if !found || j.State == "planned" {
		return nil
	}
	if j.State != "applied" || j.HostID != host || j.AppID != app {
		return ErrMigrationPending
	}
	s, _, err := store.OpenReadOnly(context.Background(), physical)
	if err != nil {
		return ErrMigrationPending
	}
	defer s.Close()
	if ok, err := s.LegacyAdoptionCommitted(context.Background(), j.AdoptionID, host, app, j.SourceDigest, j.NativeDigest, j.StartedAtMS); err != nil || !ok {
		return ErrMigrationPending
	}
	return nil
}

// Apply never starts a bridge or writes to either remote system. It reserves
// the configuration and both receiver locks before backup or schema writes.
// A crash after SQLite commit is recovered by its immutable adoption record;
// no regenerated seeds may overwrite post-migration activity on that path.
func (p MigrationPlanner) Apply(ctx context.Context, configFile, binary, planID string) (MigrationResult, error) {
	var result MigrationResult
	if !validDigest(planID) {
		return result, ErrStalePlan
	}
	c, target, err := migrationConfig(configFile)
	if err != nil {
		return result, err
	}
	configLock, err := migrationConfigLock(target)
	if err != nil {
		return result, err
	}
	defer configLock.Close()
	c, currentTarget, err := migrationConfig(target)
	if err != nil || currentTarget != target {
		return result, ErrStalePlan
	}
	artifactLock, err := migrationLock(c.Storage.DatabaseFile + ".migration.lock")
	if err != nil {
		return result, err
	}
	defer artifactLock.Close()
	lockedDatabase, lockedApp := c.Storage.DatabaseFile, c.Feishu.AppID
	appLock, err := store.AcquireApplication(c.Feishu.AppID)
	if err != nil {
		return result, ErrMigrationLocked
	}
	defer appLock.Close()
	dbLock, err := store.AcquireListener(c.Storage.DatabaseFile)
	if err != nil {
		return result, ErrMigrationLocked
	}
	defer dbLock.Close()
	j, found, err := readMigrationJournal(c.Storage.DatabaseFile)
	if err != nil {
		return result, err
	}
	if !found || j.PlanID != planID {
		return result, ErrStalePlan
	}
	c, env, identity, err := p.identity(ctx, target, binary)
	if err != nil {
		return result, err
	}
	if !env.Artifact.Released {
		return result, ErrUnreleased
	}
	if c.Storage.DatabaseFile != lockedDatabase || c.Feishu.AppID != lockedApp || j.IdentityDigest != identity || j.ConfigFile != target || j.DatabaseFile != c.Storage.DatabaseFile || j.HostID != c.Host.ID || j.AppID != c.Feishu.AppID {
		return result, ErrStalePlan
	}
	result = MigrationResult{PlanID: planID, DatabaseFile: j.DatabaseFile, BackupFile: j.BackupFile}
	s, schema, err := store.OpenReadOnly(ctx, j.DatabaseFile)
	if err != nil {
		return result, ErrMigrationPending
	}
	committed, err := s.LegacyAdoptionCommitted(ctx, j.AdoptionID, j.HostID, j.AppID, j.SourceDigest, j.NativeDigest, j.StartedAtMS)
	s.Close()
	if err != nil {
		return result, ErrMigrationPending
	}
	if committed {
		if err := migrationBackupReady(ctx, j); err != nil {
			return result, err
		}
		if j.State != "applied" {
			j.State = "applied"
			if err := atomicFile(j.DatabaseFile+".migration-state.json", encoded(j)); err != nil {
				return result, ErrMigrationPending
			}
		}
		result.State = "migration-unchanged"
		return result, nil
	}
	if schema != 1 || j.State == "applied" {
		return result, ErrMigrationPending
	}
	if j.State == "planned" && p.now().UnixMilli()-j.StartedAtMS > int64((15*time.Minute)/time.Millisecond) {
		return result, ErrStalePlan
	}
	plan, err := p.build(ctx, target, binary, j.StartedAtMS)
	if errors.Is(err, ErrStalePlan) || err == nil && !sameMigrationPlan(plan.journal, j) {
		if j.State == "prepared" {
			j.State = "planned"
			if err := atomicFile(j.DatabaseFile+".migration-state.json", encoded(j)); err != nil {
				return result, ErrMigrationPending
			}
		}
		return result, ErrStalePlan
	}
	if err != nil {
		return result, err
	}
	j.State = "prepared"
	if err := atomicFile(j.DatabaseFile+".migration-state.json", encoded(j)); err != nil {
		return result, ErrMigrationPending
	}
	if p.checkpoint != nil && p.checkpoint("prepared") != nil {
		return result, ErrMigrationPending
	}
	db, err := store.OpenExisting(ctx, j.DatabaseFile, 1)
	if err != nil {
		return result, ErrMigrationPending
	}
	defer db.Close()
	if err := db.BackupLegacy(ctx, j.BackupFile, j.SourceDigest); err != nil {
		return result, err
	}
	if p.checkpoint != nil && p.checkpoint("backup") != nil {
		return result, ErrMigrationPending
	}
	// A backup is not authority to apply an earlier remote snapshot. Re-read
	// everything immediately before the atomic local adoption transaction.
	latest, err := p.build(ctx, target, binary, j.StartedAtMS)
	if err != nil || !sameMigrationPlan(latest.journal, j) {
		// No adoption was attempted. The schema is still 1, so a new preview may
		// replace this stale plan while retaining the already-created backup.
		j.State = "planned"
		if saveErr := atomicFile(j.DatabaseFile+".migration-state.json", encoded(j)); saveErr != nil {
			return result, ErrMigrationPending
		}
		return result, ErrStalePlan
	}
	changed, err := db.AdoptLegacy(ctx, latest.adoption)
	if err != nil {
		return result, err
	}
	result.Changed = changed
	if p.checkpoint != nil && p.checkpoint("committed") != nil {
		return result, ErrMigrationPending
	}
	j.State = "applied"
	if err := atomicFile(j.DatabaseFile+".migration-state.json", encoded(j)); err != nil {
		return result, ErrMigrationPending
	}
	result.State = "migration-applied"
	return result, nil
}
