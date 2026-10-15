package creation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

// Opt-in only: one journaled workflow on the existing daemon, an explicitly
// selected registered project, a private checkout/SQLite, and synthetic Feishu
// gateway. It proves native cwd/project continuity, not Feishu or App UI.
func TestNativeWorktreeCreation(t *testing.T) {
	probe := os.Getenv("CODEX_FEISHU_WORKTREE_PROBE_DIR")
	source := os.Getenv("CODEX_FEISHU_WORKTREE_PROBE_SOURCE")
	if probe == "" || source == "" {
		t.Skip("requires explicit private probe directory and registered source project")
	}
	if !filepath.IsAbs(probe) || !filepath.IsAbs(source) {
		t.Fatal("probe paths must be absolute")
	}
	if err := os.Mkdir(probe, 0700); err != nil {
		t.Fatal("probe directory already exists or is unavailable; never redispatch")
	}
	probe, err := rules.CanonicalDirectory(probe)
	if err != nil {
		t.Fatal("probe physical directory unavailable")
	}
	journal := filepath.Join(probe, "probe.json")
	record := map[string]any{"state": "prepared"}
	save := func() {
		t.Helper()
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal("probe evidence encoding failed")
		}
		f, err := os.OpenFile(journal+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal("probe evidence unavailable")
		}
		_, err = f.Write(raw)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil || os.Rename(journal+".tmp", journal) != nil {
			t.Fatal("probe evidence persistence failed")
		}
	}
	save()
	createProject := os.Getenv("CODEX_FEISHU_WORKTREE_PROBE_CREATE_PROJECT") == "1"
	if createProject {
		if source != filepath.Join(probe, "source") || os.Mkdir(source, 0700) != nil {
			t.Fatal("fixture project must be a new directory inside the private probe")
		}
		initWorktreeTestRepo(t, source)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
	defer cancel()
	source, err = rules.CanonicalDirectory(source)
	if err != nil {
		t.Fatal("source project unavailable")
	}
	before, err := worktreeGit(ctx, source, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		t.Fatal("source status unavailable")
	}
	daemon, err := codexclient.Discover(ctx, "auto")
	if err != nil || daemon.AppServerVersion != "0.162.1" {
		t.Fatal("verified existing daemon unavailable")
	}
	native, err := codexclient.Connect(ctx, daemon.SocketPath)
	if err != nil {
		t.Fatal("existing daemon connection unavailable")
	}
	defer native.Close()
	go func() {
		for range native.Events() {
		}
	}()
	projects, err := native.ListProjects(ctx)
	if err != nil {
		t.Fatal("native project catalog unavailable")
	}
	projectID, _ := projectForPath(projects, source, "")
	if projectID == "" && createProject {
		params := map[string]any{"idempotencyKey": "cfb-worktree-probe-" + store.ID(), "name": "Feishu Worktree Verification", "roots": []codexclient.ProjectRoot{{Path: source}}}
		record["project_params"], record["state"] = params, "project_dispatching"
		save()
		var result struct {
			Project codexclient.Project `json:"project"`
		}
		if err := native.Call(ctx, "project/create", params, &result); err != nil {
			record["state"] = "project_unknown"
			save()
			t.Fatal("fixture project creation unconfirmed; never redispatch")
		}
		record["project"], record["state"] = result.Project, "project_confirmed"
		save()
		projects, err = native.ListProjects(ctx)
		if err != nil {
			t.Fatal("fixture project catalog unavailable")
		}
		projectID, _ = projectForPath(projects, source, "")
	}
	if projectID == "" {
		t.Fatal("probe source must be an actual registered project")
	}
	h := newManagerHarness(t)
	h.s.Close()
	h.root, h.db, h.clock = source, filepath.Join(probe, "probe.db"), time.Now()
	h.s, err = store.Open(h.db)
	if err != nil {
		t.Fatal("private probe store unavailable")
	}
	if _, err := h.s.BeginBaseline(ctx, "host", "app", h.clock.Unix()); err != nil {
		t.Fatal("probe baseline unavailable")
	}
	h.m.Store = h.s
	h.m.Config.Storage.DatabaseFile = h.db
	h.m.Config.Creation.AllowedRoots, h.m.Config.Creation.CommonDirectories = []string{source}, []string{source}
	h.m.Native = func() Native { return native }
	handled, err := h.m.Receive(ctx, Inbound{MessageID: "worktree-probe", ChatID: "oc_private", ChatType: "p2p", UserID: "ou_creator", Text: "/new 这是飞书工作树创建能力的独立验收。请只回复 FEISHU_WORKTREE_READY，不调用工具、不修改文件。"})
	if err != nil || !handled || h.m.tickUI(ctx) != nil {
		t.Fatal("probe draft unavailable")
	}
	d := h.draft("worktree-probe")
	if err := h.action(d, "creation_worktree", nil); err != nil || h.m.tickUI(ctx) != nil {
		t.Fatal("worktree mode unavailable")
	}
	d = h.draft("worktree-probe")
	if err := h.action(d, "creation_submit", nil); err != nil || h.m.tickUI(ctx) != nil {
		t.Fatal("probe intent unavailable")
	}
	i := h.intent(d)
	if i.ProjectID != projectID || i.WorkspaceMode != "worktree" {
		t.Fatal("native project identity was not frozen")
	}
	h.g.expectedDirectory = i.ExecutionDirectory()
	record["intent"], record["state"] = i, "workflow_claimed"
	save()
	if err := h.m.tickWorkflows(ctx); err != nil {
		t.Fatal("probe workflow pending; inspect private journal, never redispatch")
	}
	i = h.intent(d)
	record["intent"], record["state"] = i, i.State
	save()
	if i.State != "confirmed" || i.NativeThreadID == "" {
		t.Fatal("native worktree creation is not confirmed; private workflow retained")
	}
	other, err := codexclient.Connect(ctx, daemon.SocketPath)
	if err != nil {
		t.Fatal("second daemon connection unavailable")
	}
	defer other.Close()
	go func() {
		for range other.Events() {
		}
	}()
	// A fresh thread need not have a readable rollout immediately after add.
	// Acceptance is not persistence; wait for its first actual history item.
	for {
		turns, err := other.Turns(ctx, i.NativeThreadID)
		if err == nil && len(turns) != 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("first input history is not persisted yet; no redispatch")
		case <-time.After(250 * time.Millisecond):
		}
	}
	proof, err := other.ReadCreation(ctx, codexclient.CreationSpec{IntentID: i.ID, CWD: i.ExecutionDirectory(), ProjectID: projectID}, i.NativeThreadID)
	if err != nil {
		t.Fatal("worktree thread cannot resume with exact cwd and project identity")
	}
	after, err := worktreeGit(ctx, source, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || before != after {
		t.Fatal("live worktree creation changed the original checkout")
	}
	record["native_proof"], record["state"], record["source_status_digest"] = proof, "native_verified", sha256.Sum256([]byte(after))
	save()
}

// Readback of a previously journaled live probe never creates a project,
// checkout, thread, or input. It is safe to use after the creation test loses
// its connection before a rollout becomes visible to another client.
func TestNativeWorktreeReadback(t *testing.T) {
	probe := os.Getenv("CODEX_FEISHU_WORKTREE_READBACK_DIR")
	if probe == "" {
		t.Skip("requires an existing private worktree probe journal")
	}
	if !filepath.IsAbs(probe) || privateWorktreeDirectory(probe, false) != nil {
		t.Fatal("private probe directory unavailable")
	}
	path := filepath.Join(probe, "probe.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("private probe journal unavailable")
	}
	var record map[string]json.RawMessage
	var evidence struct {
		Intent store.CreationIntent `json:"intent"`
	}
	if json.Unmarshal(raw, &record) != nil || json.Unmarshal(raw, &evidence) != nil || evidence.Intent.NativeThreadID == "" || evidence.Intent.State != "confirmed" {
		t.Fatal("probe has no confirmed thread to verify")
	}
	i := evidence.Intent
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := verifyWorktree(ctx, frozenWorktree(i), false); err != nil {
		t.Fatal("probe checkout identity is not intact")
	}
	daemon, err := codexclient.Discover(ctx, "auto")
	if err != nil || daemon.AppServerVersion != "0.162.1" {
		t.Fatal("verified daemon unavailable")
	}
	native, err := codexclient.Connect(ctx, daemon.SocketPath)
	if err != nil {
		t.Fatal("existing daemon connection unavailable")
	}
	defer native.Close()
	go func() {
		for range native.Events() {
		}
	}()
	proof, err := native.ReadCreation(ctx, codexclient.CreationSpec{IntentID: i.ID, CWD: i.ExecutionDirectory(), ProjectID: i.ProjectID}, i.NativeThreadID)
	if err != nil {
		t.Fatal("same-thread worktree readback failed")
	}
	turns, err := native.Turns(ctx, i.NativeThreadID)
	if err != nil || len(turns) == 0 {
		t.Fatal("accepted worktree input has no persistent history")
	}
	record["native_proof"], _ = json.Marshal(proof)
	record["state"] = json.RawMessage(`"native_verified"`)
	raw, _ = json.Marshal(record)
	f, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal("private proof cannot be saved")
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil || os.Rename(path+".tmp", path) != nil {
		t.Fatal("private proof persistence failed")
	}
}
