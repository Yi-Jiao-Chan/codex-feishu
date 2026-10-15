package creation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type directoryFeedbackGateway struct {
	*managerGateway
	onUpdate func(string) error
	sent     chan string
}

func (g *directoryFeedbackGateway) UpdateCard(ctx context.Context, id, card string) error {
	if err := g.managerGateway.UpdateCard(ctx, id, card); err != nil {
		return err
	}
	if g.sent != nil {
		g.sent <- card
	}
	if g.onUpdate != nil {
		return g.onUpdate(card)
	}
	return nil
}

func (g *directoryFeedbackGateway) ReplyCard(ctx context.Context, parent, card, uuid string) (string, error) {
	id, err := g.managerGateway.ReplyCard(ctx, parent, card, uuid)
	if g.sent != nil {
		g.sent <- card
	}
	return id, err
}

type directoryFeedbackNative struct {
	*managerNative
	beforeProjects func(context.Context)
	beforeStart    func(context.Context)
}

func (n *directoryFeedbackNative) ListProjects(ctx context.Context) ([]codexclient.Project, error) {
	if n.beforeProjects != nil {
		n.beforeProjects(ctx)
	}
	return n.managerNative.ListProjects(ctx)
}

func (n *directoryFeedbackNative) StartCreation(ctx context.Context, s codexclient.CreationSpec) (codexclient.CreationResult, error) {
	if n.beforeStart != nil {
		n.beforeStart(ctx)
	}
	return n.managerNative.StartCreation(ctx, s)
}

func TestBrowseFirstOptionLookupAndLargeLayer(t *testing.T) {
	h := newManagerHarness(t)
	ctx := context.Background()
	for i := 0; i < 85; i++ {
		selectionMkdir(t, h.root, fmt.Sprintf("child-%02d", i))
	}
	target := filepath.Join(h.root, "child-07")
	n := &directoryFeedbackNative{managerNative: h.n, beforeProjects: func(context.Context) { t.Fatal("browser queried native project catalog") }}
	h.m.Native = func() Native { return n }
	h.m.Config.Creation.WorkgroupChatIDs = []string{"oc_unrelated"}
	d := h.receive("browse-first", "/new original task")
	before := h.g.reads
	if err := h.action(d, "creation_panel", map[string]any{"creation_body": "edited task"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	page, err := h.m.directories(nil).List(ctx, d.BrowsePath, "", 100)
	if err != nil || len(page.Choices) != 85 || page.Next != "" || page.Total != 85 || page.Offset != 0 {
		t.Fatal("small layer artificially paginated", page.Total, len(page.Choices), err)
	}
	if h.g.reads != before {
		t.Fatal("browser fetched unrelated workgroup metadata")
	}
	if err := h.m.Action(ctx, d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_browse", map[string]any{"option": target, "path": h.root}, map[string]any{"creation_entry": h.root, "creation_directory": target}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.BrowsePath != target || d.Directory != h.root || d.View != "directories" || d.Body != "edited task" {
		t.Fatal("dropdown option did not enter without selecting", d.View, d.BrowsePath)
	}
	for _, query := range []string{h.root, "CHILD-0", ""} {
		if err := h.action(d, "creation_lookup", map[string]any{"creation_query": query}); err != nil {
			t.Fatal(err)
		}
		h.tick()
		d = h.draft(d.SourceMessageID)
		filter, _, err := parseDirectoryCursor(d.BrowseCursor)
		wantFilter := query
		if filepath.IsAbs(query) {
			wantFilter = ""
		}
		if err != nil || d.BrowsePath != h.root || d.Directory != h.root || d.View != "directories" || filter != wantFilter {
			t.Fatal("lookup lost navigation/filter contract", err)
		}
	}
	if err := h.action(d, "creation_lookup", map[string]any{"creation_query": t.TempDir(), "creation_body": "keep failed edits"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.BrowsePath != h.root || d.Directory != h.root || d.Body != "keep failed edits" || d.ErrorCode != "directory-scope" {
		t.Fatal("failed lookup erased task or location")
	}
	if err := h.action(d, "creation_submit", nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.State != "draft" || d.ErrorCode != "directory-selection" || h.g.groups != 0 {
		t.Fatal("browser submitted stale selected directory")
	}
	// Real selection is a separate action and rechecks project identity.
	h.m.Native = func() Native { return h.n }
	if err := h.m.Action(ctx, d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_pick", map[string]any{"option": target}, nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.Directory != target || d.View != "main" || d.Body != "keep failed edits" {
		t.Fatal("option selection did not return preserved draft")
	}
}

func TestDirectoryBusyIsConfirmedBeforeReadAndResult(t *testing.T) {
	h := newManagerHarness(t)
	d := h.receive("busy-order", "/new task")
	var published []string
	g := &directoryFeedbackGateway{managerGateway: h.g, onUpdate: func(card string) error { published = append(published, card); return nil }}
	h.m.Gateway = g
	n := &directoryFeedbackNative{managerNative: h.n, beforeProjects: func(ctx context.Context) {
		var state string
		if err := h.s.DB.QueryRowContext(ctx, `SELECT state FROM creation_attempts WHERE draft_id=? AND step LIKE 'entry:busy:%' ORDER BY rowid DESC LIMIT 1`, d.ID).Scan(&state); err != nil || state != "confirmed" {
			t.Fatal("native read preceded durable busy confirmation", state, err)
		}
	}}
	h.m.Native = func() Native { return n }
	if err := h.action(d, "creation_projects", map[string]any{"creation_body": "edited while opening projects"}); err != nil {
		t.Fatal(err)
	}
	if len(published) != 0 {
		t.Fatal("callback performed a remote card update")
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if len(published) != 2 || !strings.Contains(published[0], "正在查找目录") || !strings.Contains(published[0], "creation_body") || !strings.Contains(published[0], "edited while opening projects") || strings.Contains(published[1], "正在查找目录") || d.View != "search" || d.CardRevision != d.Revision {
		t.Fatal("busy/result were not one ordered journaled UI flow", len(published))
	}
}

func TestDirectoryUnknownBusyBlocksReadAndResultAcrossRestart(t *testing.T) {
	h := newManagerHarness(t)
	d := h.receive("busy-unknown", "/new task")
	updates := 0
	g := &directoryFeedbackGateway{managerGateway: h.g, onUpdate: func(string) error {
		updates++
		if updates == 1 {
			return errors.New("transport response lost")
		}
		return nil
	}}
	h.m.Gateway = g
	if err := h.action(d, "creation_projects", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		h.tick()
	}
	if current := h.draft(d.SourceMessageID); current.Revision != d.Revision || current.View != "main" || updates != 1 || h.n.reads != 0 {
		t.Fatal("unknown busy was resent, overwritten or allowed its native read")
	}
	h.restart()
	h.m.Gateway = g
	h.tick()
	if updates != 1 || h.n.reads != 0 {
		t.Fatal("restart reopened unknown busy")
	}
	h.g.readback = true
	h.tick()
	current := h.draft(d.SourceMessageID)
	if current.View != "search" || current.CardRevision != current.Revision || updates != 2 || h.n.reads != 1 {
		t.Fatal("exact busy readback did not resume once to final result", updates, h.n.reads)
	}
}

func TestDirectoryBusyRemoteSuccessLocalCommitFailureRecoversWithoutResend(t *testing.T) {
	h := newManagerHarness(t)
	d := h.receive("busy-local-commit", "/new task")
	updates := 0
	g := &directoryFeedbackGateway{managerGateway: h.g, onUpdate: func(string) error { updates++; return nil }}
	h.m.Gateway = g
	if _, err := h.s.DB.Exec(`CREATE TRIGGER fail_busy_confirmation BEFORE UPDATE OF state ON creation_attempts WHEN OLD.step LIKE 'entry:busy:%' AND NEW.state='confirmed' BEGIN SELECT RAISE(FAIL,'fixture local commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := h.action(d, "creation_projects", nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if current := h.draft(d.SourceMessageID); current.Revision != d.Revision || h.n.reads != 0 || updates != 1 {
		t.Fatal("failed busy commit allowed action or duplicate write")
	}
	if _, err := h.s.DB.Exec(`DROP TRIGGER fail_busy_confirmation`); err != nil {
		t.Fatal(err)
	}
	h.g.readback = true
	h.tick()
	if current := h.draft(d.SourceMessageID); current.View != "search" || current.CardRevision != current.Revision || updates != 2 || h.n.reads != 1 {
		t.Fatal("lost confirmation did not recover exactly once in the same manager")
	}
}

func TestCreationNoopOrStaleActionRestoresFinalViewAfterBusy(t *testing.T) {
	h := newManagerHarness(t)
	d := h.receive("stale-mode", "/new task")
	if err := h.action(d, "creation_panel", map[string]any{"creation_mode": "invalid"}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.Revision != 2 || d.CardRevision != d.Revision || strings.Contains(h.g.cards[d.CardID].Content, "正在打开目录") {
		t.Fatal("stale local action left a permanently disabled busy card")
	}
	h.submit(d)
	d = h.draft(d.SourceMessageID)
	if d.State != "created" {
		t.Fatal("fixture failed to create")
	}
	previous := d.Revision
	if err := h.action(d, "creation_submit", nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.State != "created" || d.Revision != previous+1 || d.CardRevision != d.Revision || len(h.n.starts) != 1 || strings.Contains(h.g.cards[d.CardID].Content, "正在核验创建信息") {
		t.Fatal("duplicate submit left busy or repeated remote creation")
	}
}

func TestDirectoryBusyFallbackHasBoundedDisplayPath(t *testing.T) {
	card := busyFallback("host", strings.Repeat("中文\"\\", 10000), "正在打开目录…")
	if !json.Valid([]byte(card)) || len(card) > 4096 || !strings.Contains(card, "…") {
		t.Fatal("fallback exposed an unbounded path or invalid card", len(card))
	}
}

func TestDirectoryUIProceedsWhileNativeCreationIsInFlight(t *testing.T) {
	h := newManagerHarness(t)
	creating := h.receive("slow-native", "/new task")
	browsing := h.receive("independent-directory", "/new another task")
	if err := h.action(creating, "creation_submit", nil); err != nil {
		t.Fatal(err)
	}
	if err := h.m.tickUI(context.Background()); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	n := &directoryFeedbackNative{managerNative: h.n, beforeStart: func(ctx context.Context) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}}
	h.m.Native = func() Native { return n }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.m.tickWorkflows(ctx) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("native creation did not enter")
	}
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	if err := h.action(browsing, "creation_panel", nil); err != nil {
		t.Fatal(err)
	}
	if err := h.m.tickUI(ctx); err != nil {
		t.Fatal(err)
	}
	if h.draft(browsing.SourceMessageID).View != "directories" {
		t.Fatal("slow native creation blocked independent directory interaction")
	}
	a, err := h.s.CreationAttempt(ctx, creating.ID, "native")
	if err != nil || a.State != "dispatching" {
		t.Fatal("UI recovery stole an active native dispatch", a.State, err)
	}
}

func TestDirectoryActionWakesRunWithoutWaitingForTicker(t *testing.T) {
	h := newManagerHarness(t)
	g := &directoryFeedbackGateway{managerGateway: h.g, sent: make(chan string, 16)}
	h.m.Gateway = g
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.m.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	if _, err := h.m.Receive(ctx, Inbound{MessageID: "wake", ChatID: "oc_private", ChatType: "p2p", UserID: "ou_creator", Text: "task"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-g.sent:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("receive was left waiting for the one-second ticker")
	}
	var d store.CreationDraft
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		d = h.draft("wake")
		if d.CardID != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if d.CardID == "" {
		t.Fatal("initial card confirmation did not commit")
	}
	if err := h.action(d, "creation_panel", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case card := <-g.sent:
		if !strings.Contains(card, "正在打开目录") {
			t.Fatal("action wake did not publish busy first")
		}
	case <-time.After(750 * time.Millisecond):
		t.Fatal("action was left waiting for the one-second ticker")
	}
}
