package creation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
)

var (
	appFourHexBucket = regexp.MustCompile(`(?i)^[0-9a-f]{4}$`)
	appNamedBucket   = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	appTimestampName = regexp.MustCompile(`(?i)^\d{6}-\d{4}-[0-9a-f]{8}$`)
)

type appScanRecord struct {
	Path, OwnerID        string
	Pinned, NativeActive bool
	GitLocked            bool
	Birthtime            *time.Time
	UpdatedAt            time.Time
}

func appRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func appDirectory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func appNamedBucketValid(name string) bool {
	if len(name) == 0 || len(name) > 64 || !appNamedBucket.MatchString(name) {
		return false
	}
	if map[string]bool{"con": true, "prn": true, "aux": true, "nul": true, "com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true, "lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true}[name] {
		return false
	}
	return len(name) < 4 || strings.Trim(name, "0123456789abcdef") != ""
}

func appRealGitDirReadable(path string) bool {
	if !appRegularFile(filepath.Join(path, ".git")) {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil || !strings.HasPrefix(string(raw), "gitdir: ") {
		return false
	}
	target := strings.TrimSpace(strings.TrimPrefix(string(raw), "gitdir: "))
	if target == "" || !filepath.IsAbs(target) {
		target = filepath.Join(path, target)
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = os.ReadDir(real)
	return err == nil
}

// scanAppWorktrees models the verified desktop scanner against real temporary
// directories. The owner callback is deliberately fresh for every scan so a
// cleaner never relies on a cached path or thread identity.
func scanAppWorktrees(root string, owner func(string) string, state func(string) (bool, bool)) []appScanRecord {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var records []appScanRecord
	for _, bucket := range entries {
		bucketPath := filepath.Join(root, bucket.Name())
		if !appDirectory(bucketPath) {
			continue
		}
		fourHex := appFourHexBucket.MatchString(bucket.Name())
		named := appNamedBucketValid(bucket.Name())
		marker := named && appRegularFile(filepath.Join(bucketPath, ".codex-worktree-name"))
		children, err := os.ReadDir(bucketPath)
		if err != nil {
			continue
		}
		for _, child := range children {
			childPath := filepath.Join(bucketPath, child.Name())
			if !appDirectory(childPath) {
				continue
			}
			candidate := fourHex || appTimestampName.MatchString(child.Name())
			if !candidate && named && marker {
				candidate = appRealGitDirReadable(childPath)
			}
			if !candidate {
				continue
			}
			ownerID := ""
			if owner != nil {
				ownerID = owner(childPath)
			}
			active, pinned := false, false
			if state != nil {
				active, pinned = state(childPath)
			}
			records = append(records, appScanRecord{Path: childPath, OwnerID: ownerID, NativeActive: active, Pinned: pinned})
		}
	}
	return records
}

func appCleanupCandidates(records []appScanRecord, now time.Time, keepCount int) []appScanRecord {
	if len(records) <= keepCount {
		return nil
	}
	eligible := make([]appScanRecord, 0, len(records))
	for _, record := range records {
		if record.OwnerID != "" && (record.Pinned || record.NativeActive) {
			continue
		}
		if record.OwnerID == "" && record.Birthtime != nil && now.Sub(*record.Birthtime) < time.Hour {
			continue
		}
		eligible = append(eligible, record)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		left, right := eligible[i], eligible[j]
		if (left.OwnerID == "") != (right.OwnerID == "") {
			return left.OwnerID == ""
		}
		if left.OwnerID == "" {
			if (left.Birthtime == nil) != (right.Birthtime == nil) {
				return left.Birthtime != nil
			}
			return left.Birthtime != nil && right.Birthtime != nil && left.Birthtime.Before(*right.Birthtime)
		}
		return left.UpdatedAt.Before(right.UpdatedAt)
	})
	count := len(eligible) - keepCount
	if count <= 0 {
		return nil
	}
	return eligible[:count]
}

type appCleanerState struct {
	mu     sync.RWMutex
	active map[string]bool
	pinned map[string]bool
}

func (s *appCleanerState) set(path string, active, pinned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[path], s.pinned[path] = active, pinned
}

func (s *appCleanerState) get(path string) (bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active[path], s.pinned[path]
}

type appCleaner struct {
	root    string
	owner   func(string) string
	state   *appCleanerState
	mu      sync.Mutex
	records []appScanRecord
	stop    chan struct{}
	done    chan struct{}
	target  string
	deleted chan appScanRecord
}

func newAppCleaner(root string, owner func(string) string, state *appCleanerState, target string) *appCleaner {
	return &appCleaner{root: root, owner: owner, state: state, target: target, stop: make(chan struct{}), done: make(chan struct{}), deleted: make(chan appScanRecord, 1)}
}

func (c *appCleaner) scan() {
	records := scanAppWorktrees(c.root, c.owner, c.state.get)
	c.mu.Lock()
	c.records = records
	c.mu.Unlock()
	for _, candidate := range appCleanupCandidates(records, time.Now(), 0) {
		// Limit force-removal to the one disposable checkout owned by this
		// fixture. This runs concurrently with creation and ignores Git locks,
		// matching the inspected desktop's recursive filesystem fallback.
		if candidate.Path == c.target && os.RemoveAll(candidate.Path) == nil {
			c.deleted <- candidate
		}
	}
}

func (c *appCleaner) start() {
	go func() {
		defer close(c.done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			c.scan()
			select {
			case <-ticker.C:
			case <-c.stop:
				return
			}
		}
	}()
}

func (c *appCleaner) close() {
	close(c.stop)
	<-c.done
}

func (c *appCleaner) snapshot() []appScanRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]appScanRecord(nil), c.records...)
}

func waitAppCleaner(t *testing.T, c *appCleaner, check func([]appScanRecord) bool) []appScanRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		records := c.snapshot()
		if check(records) {
			return records
		}
		time.Sleep(time.Millisecond)
	}
	records := c.snapshot()
	t.Fatalf("cleaner did not reach expected filesystem snapshot: %+v", records)
	return nil
}

func worktreeLifecycleTurn(client string, status string) codexclient.Turn {
	part, _ := json.Marshal(codexclient.TextInput("synthetic first input")[0])
	return codexclient.Turn{
		ID:        "turn-" + strings.TrimPrefix(client, "cfb-"),
		Status:    status,
		ItemsView: "full",
		Items:     []codexclient.Item{{ID: "user-" + strings.TrimPrefix(client, "cfb-"), Type: "userMessage", ClientID: &client, Content: []json.RawMessage{part}}},
	}
}

func TestManagedWorktreeLifecyclePublishesOnlyAfterVisibleInput(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "managed-lifecycle")
	i := claimWorktreeTestIntent(t, h, d, nil)
	expected, err := managedWorktreePath(h.codexHome, h.root, d.ID)
	if err != nil || i.WorktreePath != expected {
		t.Fatalf("managed destination=%q want %q err=%v", i.WorktreePath, expected, err)
	}
	if filepath.Dir(filepath.Dir(i.WorktreePath)) != filepath.Join(h.codexHome, "worktrees") {
		t.Fatalf("managed destination escaped daemon worktree root: %s", i.WorktreePath)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(i.WorktreePath), ".codex-worktree-name")); !os.IsNotExist(err) {
		t.Fatalf("publication marker appeared before creation: %v", err)
	}

	h.tick()
	i = h.intent(d)
	if i.State != "confirmed" {
		t.Fatalf("managed workflow did not complete: state=%s error=%s", i.State, i.ErrorCode)
	}
	if len(h.n.starts) != 1 || len(h.n.adds) != 1 || h.g.groups != 1 || h.g.roots != 1 {
		t.Fatalf("workflow repeated or skipped a durable step: starts=%d adds=%d groups=%d roots=%d", len(h.n.starts), len(h.n.adds), h.g.groups, h.g.roots)
	}
	marker := filepath.Join(filepath.Dir(i.WorktreePath), ".codex-worktree-name")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("published marker missing: %v", err)
	}
	identity := filepath.Join(filepath.Dir(i.WorktreePath), ".codex-feishu-"+d.ID+".identity")
	identityRaw, err := os.ReadFile(identity)
	if err != nil {
		t.Fatalf("private birth receipt missing: %v", err)
	}
	worktreeAttempt, err := h.s.CreationAttempt(context.Background(), d.ID, "worktree")
	if err != nil || worktreeAttempt.State != "confirmed" || worktreeAttempt.Result == "{}" {
		t.Fatalf("worktree generation was not frozen: %+v err=%v", worktreeAttempt, err)
	}
	if worktreeAttempt.Result != string(identityRaw) {
		t.Fatalf("worktree journal and private birth receipt diverged: result=%s file=%s", worktreeAttempt.Result, identityRaw)
	}
	ownerAttempt, err := h.s.CreationAttempt(context.Background(), d.ID, "worktree-owner")
	if err != nil || ownerAttempt.State != "confirmed" || ownerAttempt.RemoteID != i.NativeThreadID || !strings.Contains(ownerAttempt.Params, i.NativeThreadID) {
		t.Fatalf("owner binding was not durable: %+v err=%v", ownerAttempt, err)
	}
	publishAttempt, err := h.s.CreationAttempt(context.Background(), d.ID, "worktree-publish")
	if err != nil || publishAttempt.State != "confirmed" || publishAttempt.RemoteID != i.NativeThreadID {
		t.Fatalf("publication was not durable: %+v err=%v", publishAttempt, err)
	}
	h.tick()
	if len(h.n.starts) != 1 || len(h.n.adds) != 1 || h.g.groups != 1 || h.g.roots != 1 {
		t.Fatal("replaying a completed worktree created a duplicate remote resource")
	}
}

func TestManagedWorktreeQueuedInputStaysHiddenUntilVisible(t *testing.T) {
	h := newManagerHarness(t)
	h.n.autoConsumeWorktree = false
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "managed-queued")
	i := claimWorktreeTestIntent(t, h, d, nil)
	h.tick()
	if got := h.intent(d); got.State == "confirmed" || len(h.n.adds) != 1 || len(h.n.queue) != 1 || len(h.n.turns) != 0 {
		t.Fatalf("queued first input was exposed: state=%s adds=%d queue=%d turns=%d", h.intent(d).State, len(h.n.adds), len(h.n.queue), len(h.n.turns))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(i.WorktreePath), ".codex-worktree-name")); !os.IsNotExist(err) {
		t.Fatalf("queued input published marker: %v", err)
	}
	client := h.n.adds[0].client
	thread := h.n.threads[i.NativeThreadID]
	thread.Status.Type = "active"
	h.n.threads[i.NativeThreadID] = thread
	h.n.turns = []codexclient.Turn{worktreeLifecycleTurn(client, "inProgress")}
	h.n.queue = nil
	h.tick()
	if got := h.intent(d); got.State != "confirmed" || len(h.n.starts) != 1 || len(h.n.adds) != 1 {
		t.Fatalf("visible first input did not publish exactly once: state=%s starts=%d adds=%d", h.intent(d).State, len(h.n.starts), len(h.n.adds))
	}
}

func TestManagedWorktreeOwnerConfirmationUnknownRecoversWithoutRestartingNative(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "managed-owner-recovery")
	claimWorktreeTestIntent(t, h, d, nil)
	_, err := h.s.DB.Exec(`CREATE TRIGGER fail_worktree_owner_confirm BEFORE UPDATE OF state ON creation_attempts WHEN OLD.step='worktree-owner' AND NEW.state='confirmed' BEGIN SELECT RAISE(FAIL,'fixture owner journal failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	a, err := h.s.CreationAttempt(context.Background(), d.ID, "worktree-owner")
	if err != nil || a.State != "dispatching" || len(h.n.starts) != 1 || len(h.n.adds) != 0 {
		t.Fatalf("owner save failure did not fence the dispatch: %+v starts=%d adds=%d err=%v", a, len(h.n.starts), len(h.n.adds), err)
	}
	if _, err := h.s.DB.Exec(`DROP TRIGGER fail_worktree_owner_confirm`); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.tick()
	if len(h.n.starts) != 1 || len(h.n.adds) != 1 || h.g.groups != 1 || h.g.roots != 1 {
		t.Fatalf("owner recovery repeated a remote action: starts=%d adds=%d groups=%d roots=%d", len(h.n.starts), len(h.n.adds), h.g.groups, h.g.roots)
	}
	a, err = h.s.CreationAttempt(context.Background(), d.ID, "worktree-owner")
	if err != nil || a.State != "confirmed" || a.RemoteID != h.intent(d).NativeThreadID {
		t.Fatalf("owner recovery did not confirm the original owner: %+v err=%v", a, err)
	}
}

func TestManagedWorktreePublishConfirmationUnknownRecoversWithoutDuplicateAdd(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "managed-publish-recovery")
	claimWorktreeTestIntent(t, h, d, nil)
	_, err := h.s.DB.Exec(`CREATE TRIGGER fail_worktree_publish_confirm BEFORE UPDATE OF state ON creation_attempts WHEN OLD.step='worktree-publish' AND NEW.state='confirmed' BEGIN SELECT RAISE(FAIL,'fixture publish journal failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	a, err := h.s.CreationAttempt(context.Background(), d.ID, "worktree-publish")
	if err != nil || a.State != "dispatching" || len(h.n.starts) != 1 || len(h.n.adds) != 1 {
		t.Fatalf("publish save failure did not fence the dispatch: %+v starts=%d adds=%d err=%v", a, len(h.n.starts), len(h.n.adds), err)
	}
	i := h.intent(d)
	marker := filepath.Join(filepath.Dir(i.WorktreePath), ".codex-worktree-name")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("publish marker was not written before journal failure: %v", err)
	}
	if _, err := h.s.DB.Exec(`DROP TRIGGER fail_worktree_publish_confirm`); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.tick()
	if got := h.intent(d); got.State != "confirmed" || len(h.n.starts) != 1 || len(h.n.adds) != 1 {
		t.Fatalf("publish recovery duplicated work: state=%s starts=%d adds=%d", h.intent(d).State, len(h.n.starts), len(h.n.adds))
	}
	a, err = h.s.CreationAttempt(context.Background(), d.ID, "worktree-publish")
	if err != nil || a.State != "confirmed" || a.RemoteID != i.NativeThreadID {
		t.Fatalf("publish recovery did not confirm marker ownership: %+v err=%v", a, err)
	}
}

func TestManagedWorktreeUnknownWithoutBirthReceiptStaysUnknown(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "managed-no-birth-receipt")
	i := claimWorktreeTestIntent(t, h, d, nil)
	p := frozenWorktree(i)
	raw, _ := json.Marshal(p)
	a, err := h.s.PrepareCreationAttempt(context.Background(), d.ID, "worktree", string(raw), h.clock.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.s.DispatchCreationAttempt(context.Background(), a); err != nil || !ok {
		t.Fatalf("claim worktree dispatch: ok=%v err=%v", ok, err)
	}
	if err := worktreeMarker(p, true); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, h.root, "worktree", "add", "--detach", "--", p.Destination, p.Commit)
	h.restart()
	h.tick()
	a, err = h.s.CreationAttempt(context.Background(), d.ID, "worktree")
	if err != nil || a.State != "unknown" || len(h.n.starts) != 0 || len(h.n.adds) != 0 {
		t.Fatalf("checkout without private birth receipt was accepted or retried: %+v starts=%d adds=%d err=%v", a, len(h.n.starts), len(h.n.adds), err)
	}
}

func TestManagedWorktreeUnsupportedNativeVersionCreatesNoRemoteResources(t *testing.T) {
	h := newManagerHarness(t)
	h.n.appServerVersion = "0.163.0"
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "managed-version-gate")
	if err := h.action(d, "creation_submit", map[string]any{"creation_use_worktree": true}); err != nil {
		t.Fatal(err)
	}
	if err := h.m.tickUI(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.tick()
	got := h.draft(d.SourceMessageID)
	if got.ErrorCode != "worktree-protocol" || len(h.n.starts) != 0 || len(h.n.adds) != 0 || h.g.groups != 0 || h.g.roots != 0 {
		t.Fatalf("unsupported native version advanced creation: error=%s starts=%d adds=%d groups=%d roots=%d", got.ErrorCode, len(h.n.starts), len(h.n.adds), h.g.groups, h.g.roots)
	}
}

func TestManagedWorktreeOwnerIdentityRejectsForeignOwnerAfterRestart(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "managed-foreign-owner")
	i := claimWorktreeTestIntent(t, h, d, nil)
	_, err := h.s.DB.Exec(`CREATE TRIGGER fail_worktree_owner_confirm BEFORE UPDATE OF state ON creation_attempts WHEN OLD.step='worktree-owner' AND NEW.state='confirmed' BEGIN SELECT RAISE(FAIL,'fixture owner journal failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	if _, err := h.s.DB.Exec(`DROP TRIGGER fail_worktree_owner_confirm`); err != nil {
		t.Fatal(err)
	}
	ownerPath := worktreeTestGit(t, i.WorktreePath, "rev-parse", "--path-format=absolute", "--git-path", worktreeOwnerName)
	if err := os.WriteFile(ownerPath, []byte(`{"version":1,"ownerThreadId":"foreign-thread"}`), 0600); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.tick()
	a, err := h.s.CreationAttempt(context.Background(), d.ID, "worktree-owner")
	if err != nil || a.State != "unknown" || len(h.n.starts) != 1 || len(h.n.adds) != 0 || h.intent(d).State == "confirmed" {
		t.Fatalf("foreign owner was accepted or retried: %+v starts=%d adds=%d intent=%s err=%v", a, len(h.n.starts), len(h.n.adds), h.intent(d).State, err)
	}
}

func TestManagedWorktreeOwnerMissingAfterRestartStaysUnknown(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "managed-missing-owner")
	i := claimWorktreeTestIntent(t, h, d, nil)
	_, err := h.s.DB.Exec(`CREATE TRIGGER fail_worktree_owner_confirm BEFORE UPDATE OF state ON creation_attempts WHEN OLD.step='worktree-owner' AND NEW.state='confirmed' BEGIN SELECT RAISE(FAIL,'fixture owner journal failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	if _, err := h.s.DB.Exec(`DROP TRIGGER fail_worktree_owner_confirm`); err != nil {
		t.Fatal(err)
	}
	ownerPath := worktreeTestGit(t, i.WorktreePath, "rev-parse", "--path-format=absolute", "--git-path", worktreeOwnerName)
	if err := os.Remove(ownerPath); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.tick()
	a, err := h.s.CreationAttempt(context.Background(), d.ID, "worktree-owner")
	if err != nil || a.State != "unknown" || len(h.n.starts) != 1 || len(h.n.adds) != 0 || h.intent(d).State == "confirmed" {
		t.Fatalf("missing owner was accepted or retried: %+v starts=%d adds=%d intent=%s err=%v", a, len(h.n.starts), len(h.n.adds), h.intent(d).State, err)
	}
}

func TestManagedWorktreeIdentityRejectsSamePathRebuild(t *testing.T) {
	h := newManagerHarness(t)
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "managed-rebuild")
	i := claimWorktreeTestIntent(t, h, d, nil)
	// Leave a complete worktree journal, then replace the checkout at the same
	// path. The immutable birth receipt must fence the new Git generation.
	p := frozenWorktree(i)
	raw, _ := json.Marshal(p)
	a, err := h.s.PrepareCreationAttempt(context.Background(), d.ID, "worktree", string(raw), h.clock.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.s.DispatchCreationAttempt(context.Background(), a); err != nil || !ok {
		t.Fatal(err)
	}
	if err := worktreeMarker(p, true); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, h.root, "worktree", "add", "--detach", "--", p.Destination, p.Commit)
	if _, err := creationWorktreeResult(context.Background(), p, true); err != nil {
		t.Fatal(err)
	}
	if err := h.s.FinishCreationAttempt(context.Background(), a, "confirmed", p.Destination, mustJSONBirth(t, p)); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, h.root, "worktree", "remove", "--force", "--", p.Destination)
	worktreeTestGit(t, h.root, "worktree", "add", "--detach", "--", p.Destination, p.Commit)
	h.restart()
	h.tick()
	got, err := h.s.CreationAttempt(context.Background(), d.ID, "worktree")
	if err != nil || got.State != "confirmed" || len(h.n.starts) != 0 || h.intent(d).State == "confirmed" {
		t.Fatalf("same-path rebuild passed an old generation: attempt=%+v starts=%d state=%s err=%v", got, len(h.n.starts), h.intent(d).State, err)
	}
}

func mustJSONBirth(t *testing.T, p worktreeRequest) string {
	t.Helper()
	identity, err := worktreeOwnerIdentity(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(worktreeBirth{Version: 1, Identity: identity})
	return string(raw)
}

func appRecord(records []appScanRecord, path string) (appScanRecord, bool) {
	for _, record := range records {
		if record.Path == path {
			return record, true
		}
	}
	return appScanRecord{}, false
}

func appRecordIn(records []appScanRecord, path string) bool {
	_, ok := appRecord(records, path)
	return ok
}

func appCandidateIn(records []appScanRecord, path string) bool {
	for _, record := range records {
		if record.Path == path {
			return true
		}
	}
	return false
}

func TestAppCleanupCandidatesUseKeepCountAndOwnership(t *testing.T) {
	now := time.Unix(1800000000, 0)
	old := now.Add(-2 * time.Hour)
	lockedBirth := old.Add(time.Minute)
	recent := now.Add(-30 * time.Minute)
	records := []appScanRecord{
		{Path: "ownerless-old", Birthtime: &old},
		{Path: "ownerless-null-birthtime", Birthtime: nil},
		{Path: "ownerless-recent-birthtime", Birthtime: &recent},
		{Path: "owned-active", OwnerID: "thread-active", NativeActive: true, UpdatedAt: old},
		{Path: "owned-pinned", OwnerID: "thread-pinned", Pinned: true, UpdatedAt: old},
		{Path: "owned-idle", OwnerID: "thread-idle", UpdatedAt: old},
		{Path: "git-locked-ownerless", GitLocked: true, Birthtime: &lockedBirth},
	}
	if got := appCleanupCandidates(records, now, len(records)); got != nil {
		t.Fatalf("records within keepCount produced candidates: %+v", got)
	}
	got := appCleanupCandidates(records, now, 0)
	if appCandidateIn(got, "ownerless-recent-birthtime") || appCandidateIn(got, "owned-active") || appCandidateIn(got, "owned-pinned") {
		t.Fatalf("protected records were selected: %+v", got)
	}
	if !appCandidateIn(got, "git-locked-ownerless") || len(got) != 4 {
		t.Fatalf("candidate selection ignored nullable birthtime or Git lock semantics: %+v", got)
	}
	if got[0].Path != "ownerless-old" || got[1].Path != "git-locked-ownerless" || got[2].Path != "ownerless-null-birthtime" || got[3].Path != "owned-idle" {
		t.Fatalf("candidate order did not keep ownerless records first: %+v", got)
	}
	if got = appCleanupCandidates(records, now, 2); len(got) != 2 || got[0].Path != "ownerless-old" || got[1].Path != "git-locked-ownerless" {
		t.Fatalf("keepCount pressure selected the wrong records: %+v", got)
	}
}

func TestManagedWorktreeCleanerScansRealLifecycleBoundaries(t *testing.T) {
	h := newManagerHarness(t)
	h.n.autoConsumeWorktree = false
	initWorktreeTestRepo(t, h.root)
	d := worktreeDraft(t, h, "managed-cleaner-boundaries")
	i := claimWorktreeTestIntent(t, h, d, nil)
	worktreesRoot := filepath.Join(h.codexHome, "worktrees")
	legacyBucket := filepath.Join(worktreesRoot, "a1B2")
	legacyChild := filepath.Join(legacyBucket, "legacy-project")
	if err := os.MkdirAll(legacyChild, 0700); err != nil {
		t.Fatal(err)
	}
	symlinkChild := filepath.Join(legacyBucket, "symlink-project")
	if err := os.Symlink(legacyChild, symlinkChild); err != nil {
		t.Fatal(err)
	}
	timestampChild := filepath.Join(worktreesRoot, "foreign-bucket", "261016-6234-ABCDEF12")
	if err := os.MkdirAll(timestampChild, 0700); err != nil {
		t.Fatal(err)
	}
	withoutMarker := filepath.Join(worktreesRoot, "named-bucket", "ordinary-project")
	if err := os.MkdirAll(withoutMarker, 0700); err != nil {
		t.Fatal(err)
	}
	markedWithoutGit := filepath.Join(worktreesRoot, "marked-bucket", "ordinary-project")
	if err := os.MkdirAll(markedWithoutGit, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(markedWithoutGit), ".codex-worktree-name"), nil, 0600); err != nil {
		t.Fatal(err)
	}

	state := &appCleanerState{active: map[string]bool{}, pinned: map[string]bool{}}
	p := frozenWorktree(i)
	ownerRequests := map[string]worktreeRequest{i.WorktreePath: p}
	owner := func(path string) string {
		request, ok := ownerRequests[path]
		if !ok {
			return ""
		}
		id, err := readWorktreeOwner(context.Background(), request)
		if err != nil {
			return ""
		}
		return id
	}
	cleaner := newAppCleaner(worktreesRoot, owner, state, i.WorktreePath)
	cleaner.start()
	defer cleaner.close()

	started := make(chan string, 1)
	releaseStart := make(chan struct{})
	h.n.afterStart = func(id string) {
		started <- id
		<-releaseStart
	}
	addStarted := make(chan struct{})
	releaseAdd := make(chan struct{})
	ownerSeen := make(chan string, 1)
	h.n.beforeAdd = func(_, _, _ string) {
		ownerID, err := readWorktreeOwner(context.Background(), p)
		if err != nil {
			ownerID = "error: " + err.Error()
		}
		ownerSeen <- ownerID
		close(addStarted)
		<-releaseAdd
	}
	tickDone := make(chan error, 1)
	go func() { tickDone <- h.m.Tick(context.Background()) }()
	var nativeID string
	select {
	case nativeID = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("native start barrier was not reached")
	}
	if _, err := os.Stat(i.WorktreePath); err != nil {
		t.Fatalf("worktree was not present at start barrier: %v", err)
	}
	records := waitAppCleaner(t, cleaner, func(records []appScanRecord) bool {
		return appRecordIn(records, legacyChild) && appRecordIn(records, timestampChild) && len(records) >= 2
	})
	if appRecordIn(records, i.WorktreePath) || appRecordIn(records, symlinkChild) || appRecordIn(records, withoutMarker) || appRecordIn(records, markedWithoutGit) {
		t.Fatalf("hidden or invalid named worktree was exposed during start: %+v", records)
	}
	close(releaseStart)
	select {
	case <-addStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("owner/add barrier was not reached")
	}
	select {
	case ownerID := <-ownerSeen:
		if ownerID != nativeID {
			t.Fatalf("owner file did not contain the native thread at queued boundary: got=%q want=%q", ownerID, nativeID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("owner was not observed before queued Add")
	}
	records = waitAppCleaner(t, cleaner, func(records []appScanRecord) bool { return len(records) >= 2 })
	if appRecordIn(records, i.WorktreePath) {
		t.Fatalf("owner-written but unpublished worktree was exposed: %+v", records)
	}
	close(releaseAdd)
	if err := <-tickDone; err != nil {
		t.Fatal(err)
	}
	if got := h.intent(d); got.State == "confirmed" || len(h.n.queue) != 1 {
		t.Fatalf("queued first input crossed the publication boundary: state=%s queue=%d", h.intent(d).State, len(h.n.queue))
	}

	client := h.n.adds[0].client
	thread := h.n.threads[nativeID]
	thread.Status.Type = "active"
	h.n.threads[nativeID] = thread
	h.n.turns = []codexclient.Turn{worktreeLifecycleTurn(client, "inProgress")}
	h.n.queue = nil
	state.set(i.WorktreePath, true, false)
	h.tick()
	i = h.intent(d)
	marker := filepath.Join(filepath.Dir(i.WorktreePath), ".codex-worktree-name")
	if i.State != "confirmed" {
		t.Fatalf("first visible turn did not complete creation: %+v", i)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("publication marker missing after first visible turn: %v", err)
	}
	records = waitAppCleaner(t, cleaner, func(records []appScanRecord) bool {
		record, ok := appRecord(records, i.WorktreePath)
		return ok && record.OwnerID == nativeID && record.NativeActive && !record.Pinned
	})
	record, _ := appRecord(records, i.WorktreePath)
	if record.OwnerID != nativeID {
		t.Fatalf("published scanner record used the wrong owner: %+v", record)
	}
	if candidates := appCleanupCandidates(records, time.Now(), 0); appCandidateIn(candidates, i.WorktreePath) {
		t.Fatalf("active published owner was selected: %+v", candidates)
	}

	state.set(i.WorktreePath, false, true)
	records = waitAppCleaner(t, cleaner, func(records []appScanRecord) bool {
		record, ok := appRecord(records, i.WorktreePath)
		return ok && record.Pinned && !record.NativeActive
	})
	if candidates := appCleanupCandidates(records, time.Now(), 0); appCandidateIn(candidates, i.WorktreePath) {
		t.Fatalf("pinned published owner was selected: %+v", candidates)
	}
	worktreeTestGit(t, h.root, "worktree", "lock", "--reason", "synthetic lock", "--", i.WorktreePath)
	state.set(i.WorktreePath, false, false)
	select {
	case deleted := <-cleaner.deleted:
		if deleted.OwnerID != nativeID || deleted.Pinned || deleted.NativeActive || deleted.Birthtime != nil {
			t.Fatalf("cleaner removed a protected or unowned checkout: %+v", deleted)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle published owner was not deleted under capacity pressure")
	}
	waitAppCleaner(t, cleaner, func(records []appScanRecord) bool { return !appRecordIn(records, i.WorktreePath) })
}
