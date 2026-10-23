package creation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
	messages "codex-feishu/internal/sync"
)

type creationAddCall struct{ thread, body, client string }
type managerNative struct {
	h                      *managerHarness
	starts                 []codexclient.CreationSpec
	adds                   []creationAddCall
	startErrors, addErrors []error
	threads                map[string]codexclient.Thread
	listed, archived       []codexclient.Thread
	loaded                 []string
	queue                  []codexclient.Queued
	turns                  []codexclient.Turn
	reads                  int
	projects               []codexclient.Project
	metadataErr            error
	archiveErr             error
	creationReadErr        error
	creationReads          int
	afterStart             func(string)
}

func (n *managerNative) ListProjects(context.Context) ([]codexclient.Project, error) {
	n.reads++
	return n.projects, nil
}

func (n *managerNative) CodexHome() string { return n.h.codexHome }
func (n *managerNative) StartCreation(ctx context.Context, s codexclient.CreationSpec) (codexclient.CreationResult, error) {
	n.h.requireDispatch(ctx, "native")
	params, err := codexclient.CreationRequestJSON(s)
	if err != nil {
		n.h.t.Fatal(err)
	}
	var frozen string
	if err = n.h.s.DB.QueryRowContext(ctx, `SELECT params_json FROM creation_attempts WHERE step='native' ORDER BY rowid DESC LIMIT 1`).Scan(&frozen); err != nil || frozen != params {
		n.h.t.Fatal("native params were not journaled accurately", err)
	}
	n.starts = append(n.starts, s)
	n.h.order = append(n.h.order, "native")
	var remoteErr error
	if len(n.startErrors) > 0 {
		remoteErr = n.startErrors[0]
		n.startErrors = n.startErrors[1:]
	}
	var rejected *codexclient.RPCError
	if errors.As(remoteErr, &rejected) {
		return codexclient.CreationResult{}, remoteErr
	}
	source, _ := codexclient.CreationThreadSource(s.IntentID)
	label, _ := json.Marshal(source)
	thread := codexclient.Thread{ID: fmt.Sprintf("native-%d", len(n.starts)), CWD: s.CWD, Ephemeral: false, ThreadSource: label}
	if s.ProjectID != "" {
		id := s.ProjectID
		thread.ProjectID = &id
	}
	n.threads[thread.ID] = thread
	if n.afterStart != nil {
		n.afterStart(thread.ID)
	}
	return codexclient.CreationResult{Thread: thread, CWD: s.CWD, Model: "inherited", ModelProvider: "native-default", ApprovalPolicy: json.RawMessage(`"on-request"`), ApprovalsReviewer: "user", Sandbox: json.RawMessage(`{"type":"workspaceWrite","networkAccess":false}`), ActivePermissionProfile: json.RawMessage(`{"id":":workspace","extends":null}`)}, remoteErr
}
func (n *managerNative) ReadCreation(_ context.Context, s codexclient.CreationSpec, id string) (codexclient.CreationResult, error) {
	n.reads++
	n.creationReads++
	if len(n.starts) == 0 || !reflect.DeepEqual(s, n.starts[0]) {
		n.h.t.Fatal("creation proof used another frozen specification")
	}
	if n.creationReadErr != nil {
		return codexclient.CreationResult{}, n.creationReadErr
	}
	thread, ok := n.threads[id]
	if !ok {
		return codexclient.CreationResult{}, &codexclient.UnknownError{Method: "thread/resume", Cause: errors.New("unavailable")}
	}
	var actual string
	expected, _ := codexclient.CreationThreadSource(s.IntentID)
	if json.Unmarshal(thread.ThreadSource, &actual) != nil || actual != expected || thread.ID != id || thread.CWD != s.CWD || thread.Ephemeral || thread.ParentThreadID != nil || thread.ForkedFromID != nil || s.ProjectID != "" && (thread.ProjectID == nil || *thread.ProjectID != s.ProjectID) {
		return codexclient.CreationResult{}, &codexclient.UnknownError{Method: "thread/resume", Cause: errors.New("identity proof incomplete")}
	}
	return codexclient.CreationResult{Thread: thread, CWD: s.CWD, Model: "inherited", ModelProvider: "native-default", ApprovalPolicy: json.RawMessage(`"on-request"`), ApprovalsReviewer: "user", Sandbox: json.RawMessage(`{"type":"workspaceWrite","networkAccess":false}`), ActivePermissionProfile: json.RawMessage(`{"id":":workspace","extends":null}`)}, nil
}
func (n *managerNative) Add(ctx context.Context, thread, body, client string) (codexclient.Queued, error) {
	var state string
	if err := n.h.s.DB.QueryRowContext(ctx, `SELECT state FROM submission_attempts WHERE client_id=? ORDER BY rowid DESC LIMIT 1`, client).Scan(&state); err != nil || state != "dispatching" {
		n.h.t.Fatal("input was not journaled before dispatch", state, err)
	}
	n.adds = append(n.adds, creationAddCall{thread, body, client})
	n.h.order = append(n.h.order, "add")
	if len(n.addErrors) > 0 {
		err := n.addErrors[0]
		n.addErrors = n.addErrors[1:]
		if err != nil {
			return codexclient.Queued{}, err
		}
	}
	input, _ := json.Marshal(codexclient.TextInput(body))
	q := codexclient.Queued{ID: "q-" + store.ID(), ClientUserMessageID: &client, Input: input}
	n.queue = append(n.queue, q)
	return q, nil
}
func (n *managerNative) Metadata(_ context.Context, id string) (codexclient.Thread, error) {
	n.reads++
	if n.metadataErr != nil {
		return codexclient.Thread{}, n.metadataErr
	}
	t, ok := n.threads[id]
	if !ok {
		return codexclient.Thread{}, errors.New("fixture metadata unavailable")
	}
	return t, nil
}
func (n *managerNative) ListArchive(_ context.Context, archived bool) ([]codexclient.Thread, error) {
	n.reads++
	if n.archiveErr != nil {
		return nil, n.archiveErr
	}
	if archived {
		return n.archived, nil
	}
	return n.listed, nil
}
func (n *managerNative) LoadedIDs(context.Context) ([]string, error) { n.reads++; return n.loaded, nil }
func (n *managerNative) Queue(context.Context, string) ([]codexclient.Queued, error) {
	n.reads++
	return n.queue, nil
}
func (n *managerNative) Turns(context.Context, string) ([]codexclient.Turn, error) {
	n.reads++
	return n.turns, nil
}
func (n *managerNative) Delete(context.Context, string, string) (bool, error) {
	n.h.t.Fatal("creation deleted native input")
	return false, nil
}
func (n *managerNative) Steer(context.Context, string, string, string, string) error {
	n.h.t.Fatal("creation steered input")
	return nil
}
func (n *managerNative) Interrupt(context.Context, string, string) error {
	n.h.t.Fatal("creation interrupted native turn")
	return nil
}
func (n *managerNative) StartQueue(context.Context, string) error {
	n.h.t.Fatal("creation started native queue explicitly")
	return nil
}

type managerGateway struct {
	h                                      *managerHarness
	groups, roots, replies, updates, reads int
	groupErr                               error
	rootErr, replyErr                      error
	readback                               bool
	cards                                  map[string]feishu.CardMessage
	workgroups                             map[string]string
	afterRoot                              func(string)
	getCalls                               int
	expectedDirectory                      string
}

func (g *managerGateway) CreateGroupWithDescription(ctx context.Context, name, owner string, members []string, uuid, description string) (string, error) {
	g.h.requireDispatch(ctx, "group")
	g.groups++
	g.h.order = append(g.h.order, "group")
	expected := g.expectedDirectory
	if expected == "" {
		expected = g.h.root
	}
	if owner != "ou_owner" || uuid == "" || !contains(members, "ou_creator") || !strings.Contains(description, expected) {
		g.h.t.Fatal("group request lost frozen identity/location")
	}
	if g.groupErr != nil {
		return "", g.groupErr
	}
	return "oc_task", nil
}
func (g *managerGateway) SendCard(ctx context.Context, typ, chat, card, uuid string) (string, error) {
	g.h.requireDispatch(ctx, "root")
	g.roots++
	g.h.order = append(g.h.order, "root")
	if typ != "chat_id" || chat != "oc_task" && g.workgroups[chat] == "" || uuid == "" || !json.Valid([]byte(card)) {
		g.h.t.Fatal("invalid root request")
	}
	id := "om_root"
	if g.roots > 1 {
		id = fmt.Sprintf("om_root_%d", g.roots)
	}
	g.saveCard(id, chat, "", card)
	if g.afterRoot != nil {
		g.afterRoot(id)
	}
	if g.rootErr != nil {
		err := g.rootErr
		g.rootErr = nil
		return "", err
	}
	return id, nil
}
func (g *managerGateway) ReplyCard(ctx context.Context, parent, card, _ string) (string, error) {
	g.replies++
	id := fmt.Sprintf("om_entry_%d", g.replies)
	var chat string
	if err := g.h.s.DB.QueryRowContext(ctx, `SELECT source_chat_id FROM creation_drafts WHERE source_message_id=?`, parent).Scan(&chat); err != nil {
		g.h.t.Fatal(err)
	}
	g.saveCard(id, chat, parent, card)
	if g.replyErr != nil {
		err := g.replyErr
		g.replyErr = nil
		return "", err
	}
	return id, nil
}
func (g *managerGateway) UpdateCard(_ context.Context, id, card string) error {
	g.updates++
	if message, ok := g.cards[id]; ok {
		message.Content = card
		g.cards[id] = message
	}
	return nil
}
func (g *managerGateway) MemberIDs(context.Context, string) ([]string, error) {
	g.reads++
	return []string{"ou_owner", "ou_creator", "ou_other"}, nil
}
func (g *managerGateway) BotInChat(context.Context, string) (bool, error) {
	g.reads++
	return true, nil
}
func (g *managerGateway) GetGroupInfo(_ context.Context, chat string) (feishu.GroupInfo, error) {
	g.reads++
	if name := g.workgroups[chat]; name != "" {
		return feishu.GroupInfo{Name: name, OwnerID: "workgroup-owner", ChatMode: "group"}, nil
	}
	return feishu.GroupInfo{OwnerID: "ou_owner", ChatMode: "group"}, nil
}
func (g *managerGateway) ReadCardMessage(_ context.Context, id string) (feishu.CardMessage, error) {
	g.reads++
	g.getCalls++
	if g.readback {
		if message, ok := g.cards[id]; ok {
			return message, nil
		}
	}
	return feishu.CardMessage{}, messages.ErrReadbackIncomplete
}
func (g *managerGateway) ListCardMessages(_ context.Context, _ string, chat, _ string) (feishu.CardMessagePage, error) {
	g.reads++
	if g.readback {
		page := feishu.CardMessagePage{}
		for _, message := range g.cards {
			if message.ChatID == chat {
				page.Items = append(page.Items, message)
			}
		}
		return page, nil
	}
	return feishu.CardMessagePage{}, nil
}

func (g *managerGateway) saveCard(id, chat, parent, card string) {
	if g.cards == nil {
		g.cards = map[string]feishu.CardMessage{}
	}
	deleted := false
	message := feishu.CardMessage{ID: id, ChatID: chat, Type: "interactive", SenderID: "app", SenderIDType: "app_id", SenderType: "app", Deleted: &deleted, Content: card}
	if parent != "" {
		message.ParentID = &parent
	}
	g.cards[id] = message
}

var _ Native = (*managerNative)(nil)
var _ Gateway = (*managerGateway)(nil)

type managerHarness struct {
	t                         *testing.T
	s                         *store.Store
	m                         *Manager
	n                         *managerNative
	g                         *managerGateway
	root, db, codexHome       string
	clock                     time.Time
	order, activated, notices []string
}

func newManagerHarness(t *testing.T) *managerHarness {
	t.Helper()
	h := &managerHarness{t: t, root: t.TempDir(), db: filepath.Join(t.TempDir(), "creation.db"), codexHome: t.TempDir(), clock: time.Unix(1800000000, 0)}
	var err error
	h.s, err = store.Open(h.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.s.Close() })
	if _, err = h.s.BeginBaseline(context.Background(), "host", "app", h.clock.Unix()); err != nil {
		t.Fatal(err)
	}
	h.n = &managerNative{h: h, threads: map[string]codexclient.Thread{}}
	h.g = &managerGateway{h: h}
	var cfg config.Config
	cfg.Host.ID = "host"
	cfg.Feishu.AppID = "app"
	cfg.Access.OwnerOpenID = "ou_owner"
	cfg.Access.OperatorOpenIDs = []string{"ou_creator", "ou_other"}
	cfg.Creation = config.CreationConfig{Enabled: true, AllowedRoots: []string{h.root}, CommonDirectories: []string{h.root}, EntryChatIDs: []string{"oc_entry"}}
	h.m = &Manager{Store: h.s, Config: cfg, Gateway: h.g, Native: func() Native { return h.n }, Now: func() time.Time { return h.clock }, Activate: func(id string) { h.activated = append(h.activated, id) }, Notice: func(code string) { h.notices = append(h.notices, code) }}
	return h
}
func (h *managerHarness) requireDispatch(ctx context.Context, step string) {
	h.t.Helper()
	var state string
	if err := h.s.DB.QueryRowContext(ctx, `SELECT state FROM creation_attempts WHERE step=? ORDER BY rowid DESC LIMIT 1`, step).Scan(&state); err != nil || state != "dispatching" {
		h.t.Fatal("remote action lacked durable dispatch claim", step, state, err)
	}
}
func (h *managerHarness) tick() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.m.Tick(ctx); err != nil {
		h.t.Fatal("manager tick failed", err)
	}
}
func (h *managerHarness) draft(source string) store.CreationDraft {
	h.t.Helper()
	var id string
	if err := h.s.DB.QueryRow(`SELECT draft_id FROM creation_drafts WHERE source_message_id=?`, source).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	d, err := h.s.CreationDraft(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}
func (h *managerHarness) receive(source, text string) store.CreationDraft {
	h.t.Helper()
	handled, err := h.m.Receive(context.Background(), Inbound{MessageID: source, ChatID: "oc_private", ChatType: "p2p", UserID: "ou_creator", Text: text})
	if err != nil || !handled {
		h.t.Fatal("inbound not accepted", handled, err)
	}
	h.tick()
	return h.draft(source)
}
func (h *managerHarness) action(d store.CreationDraft, kind string, form map[string]any) error {
	return h.m.Action(context.Background(), d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, kind, nil, form)
}

func localCreationForm(form map[string]any) map[string]any {
	local := make(map[string]any, len(form)+1)
	for key, value := range form {
		local[key] = value
	}
	local["creation_use_worktree"] = false
	return local
}

func (h *managerHarness) submitLocal(d store.CreationDraft) {
	h.t.Helper()
	if err := h.action(d, "creation_submit", localCreationForm(nil)); err != nil {
		h.t.Fatal(err)
	}
	h.tick()
}

func (h *managerHarness) submit(d store.CreationDraft) {
	h.t.Helper()
	if err := h.action(d, "creation_submit", nil); err != nil {
		h.t.Fatal(err)
	}
	h.tick()
}
func (h *managerHarness) intent(d store.CreationDraft) store.CreationIntent {
	h.t.Helper()
	i, err := h.s.CreationIntent(context.Background(), d.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return i
}
func (h *managerHarness) remoteCalls() int {
	return len(h.n.starts) + len(h.n.adds) + h.n.reads + h.g.groups + h.g.roots + h.g.replies + h.g.updates + h.g.reads
}
func (h *managerHarness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.s.DB.QueryRow(query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}
func (h *managerHarness) restart() {
	h.t.Helper()
	if err := h.s.Close(); err != nil {
		h.t.Fatal(err)
	}
	var err error
	h.s, err = store.Open(h.db)
	if err != nil {
		h.t.Fatal(err)
	}
	old := h.m
	h.m = &Manager{Store: h.s, Config: old.Config, Gateway: h.g, Native: old.Native, Now: old.Now, Activate: old.Activate, Notice: old.Notice}
	if err = h.s.RecoverCreations(context.Background(), "host", "app"); err != nil {
		h.t.Fatal(err)
	}
}

func TestManagerCreationIngressAndLocalCallbacks(t *testing.T) {
	h := newManagerHarness(t)
	for i, in := range []Inbound{
		{ChatID: "oc_private", ChatType: "p2p", UserID: "ou_creator", Text: "普通任务"},
		{ChatID: "oc_private", ChatType: "p2p", UserID: "ou_creator", Text: "/new 明确任务"},
		{ChatID: "oc_task", ChatType: "group", UserID: "ou_creator", Text: "继续已有任务", Bound: true},
		{ChatID: "oc_forbidden", ChatType: "group", UserID: "ou_creator", Text: "/new 不允许"},
		{ChatID: "oc_private", ChatType: "p2p", UserID: "ou_unauthorized", Text: "/new 不允许"},
		{ChatID: "oc_entry", ChatType: "group", UserID: "ou_creator", Text: "/notify status"},
	} {
		in.MessageID = fmt.Sprintf("source-%d", i)
		before := h.remoteCalls()
		handled, err := h.m.Receive(context.Background(), in)
		if err != nil || handled != (i < 2) || h.remoteCalls() != before {
			t.Fatal("incorrect ingress routing or remote call in ingress", i, handled, err)
		}
	}
	if h.count(`SELECT COUNT(*) FROM creation_drafts`) != 2 {
		t.Fatal("unauthorized/bound/management input created draft")
	}
	h.tick()
	d := h.draft("source-1")
	if d.Body != "明确任务" || h.draft("source-0").Body != "普通任务" {
		t.Fatal("prefill lost command parsing")
	}
	before := h.remoteCalls()
	for i := 0; i < 2; i++ {
		if err := h.action(d, "creation_submit", localCreationForm(nil)); err != nil {
			t.Fatal("duplicate callback failed", err)
		}
	}
	if h.remoteCalls() != before || h.count(`SELECT COUNT(*) FROM creation_actions WHERE draft_id=?`, d.ID) != 1 {
		t.Fatal("callback dispatched remote action or duplicate claim")
	}
	h.tick()
	if h.draft("source-1").State != "created" || h.g.groups != 1 || len(h.n.starts) != 1 || len(h.n.adds) != 1 {
		t.Fatal("claimed callback did not create exactly one task")
	}
}

func TestManagerCreationOrderedFlowAndMirrorSuppression(t *testing.T) {
	h := newManagerHarness(t)
	d := h.receive("source", "build the feature")
	h.submitLocal(d)
	if !reflect.DeepEqual(h.order, []string{"group", "root", "native", "add"}) {
		t.Fatal("creation order/connection changed", h.order)
	}
	i := h.intent(d)
	if i.State != "confirmed" || i.ReceiptID == "" || len(h.activated) != 1 || h.n.adds[0].thread != i.NativeThreadID || h.n.adds[0].body != d.Body || h.n.starts[0].CWD != h.root {
		t.Fatal("initial creation identity not preserved")
	}
	client := h.n.adds[0].client
	var source, reply, thread string
	if err := h.s.DB.QueryRow(`SELECT message_id,reply_message_id,thread_id FROM m0_receipts WHERE id=?`, i.ReceiptID).Scan(&source, &reply, &thread); err != nil || source != d.SourceMessageID || reply != "om_root" || thread != i.NativeThreadID {
		t.Fatal("receipt lost original source or local reply root", err)
	}
	part, _ := json.Marshal(codexclient.TextInput(d.Body)[0])
	turns := []codexclient.Turn{{ID: "turn", Status: "completed", Items: []codexclient.Item{{ID: "user", Type: "userMessage", ClientID: &client, Content: []json.RawMessage{part}}}}}
	engine := messages.Engine{Store: h.s, HostID: "host"}
	if err := engine.Apply(context.Background(), i.NativeThreadID, turns, false, h.clock.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if h.count(`SELECT COUNT(*) FROM mirrors WHERE kind='human' AND thread_id=?`, i.NativeThreadID) != 0 {
		t.Fatal("initial Feishu body was mirrored back as a second human message")
	}
	for j := 0; j < 3; j++ {
		h.tick()
	}
	if h.g.groups != 1 || h.g.roots != 1 || len(h.n.starts) != 1 || len(h.n.adds) != 1 {
		t.Fatal("repeat Tick duplicated remote object/input")
	}
	h.restart()
	h.tick()
	if h.g.groups != 1 || len(h.n.starts) != 1 || len(h.n.adds) != 1 {
		t.Fatal("restart recreated confirmed object")
	}
}

func TestManagerCreationCallbackAuthenticationAndStaleRevision(t *testing.T) {
	h := newManagerHarness(t)
	d := h.receive("source", "task")
	if err := h.action(d, "creation_browse", nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	d = h.draft("source")
	if d.Revision < 2 {
		t.Fatal("fixture did not produce a real older positive revision")
	}
	for _, variant := range []string{"other-creator", "wrong-chat", "wrong-card", "wrong-token", "old-revision", "expired"} {
		user, chat, card, token, rev := d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision
		switch variant {
		case "other-creator":
			user = "ou_other"
		case "wrong-chat":
			chat = "oc_other"
		case "wrong-card":
			card = "om_other"
		case "wrong-token":
			token = "forged"
		case "old-revision":
			rev--
		case "expired":
			h.clock = h.clock.Add(time.Hour)
		}
		before := h.remoteCalls()
		err := h.m.Action(context.Background(), user, chat, card, token, rev, "creation_submit", nil, nil)
		if !errors.Is(err, store.ErrSubmissionStale) || before != h.remoteCalls() {
			t.Fatal("invalid callback accepted/dispatched", variant, err)
		}
	}
	h.tick()
	if h.g.groups != 0 || len(h.n.starts) != 0 || h.count(`SELECT COUNT(*) FROM creation_workflows`) != 0 {
		t.Fatal("invalid callback created remote task")
	}
}

func TestManagerCreationNativeRejectExplicitRetryOnlyNative(t *testing.T) {
	h := newManagerHarness(t)
	h.n.startErrors = []error{&codexclient.RPCError{Code: -32602, Message: "PRIVATE-NATIVE-ERROR"}}
	d := h.receive("source", "PRIVATE-TASK-BODY")
	h.submitLocal(d)
	i := h.intent(d)
	if i.State != "rejected" || h.g.groups != 1 || h.g.roots != 1 || len(h.n.starts) != 1 || len(h.n.adds) != 0 {
		t.Fatal("native rejection lost confirmed target")
	}
	for j := 0; j < 2; j++ {
		h.tick()
	}
	if len(h.n.starts) != 1 {
		t.Fatal("native rejection retried without explicit action")
	}
	d = h.draft("source")
	if err := h.action(d, "creation_retry", nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if h.intent(d).State != "confirmed" || h.g.groups != 1 || h.g.roots != 1 || len(h.n.starts) != 2 || len(h.n.adds) != 1 {
		t.Fatal("explicit native retry recreated confirmed steps")
	}
	if !reflect.DeepEqual(h.n.starts[0], h.n.starts[1]) || h.count(`SELECT COUNT(*) FROM creation_attempts WHERE draft_id=? AND step='native'`, d.ID) != 2 {
		t.Fatal("native retry changed immutable params or overwrote attempt")
	}
	for _, notice := range h.notices {
		if strings.Contains(notice, "PRIVATE-") || strings.Contains(notice, h.root) {
			t.Fatal("private task/native detail leaked in diagnostic")
		}
	}
}

func TestManagerCreationInputRejectRetriesOriginalReceipt(t *testing.T) {
	h := newManagerHarness(t)
	h.n.addErrors = []error{&codexclient.RPCError{Code: -32602, Message: "PRIVATE-INPUT-ERROR"}}
	d := h.receive("source", "task")
	h.submitLocal(d)
	i := h.intent(d)
	if i.State != "rejected" || i.ErrorCode != "input-rejected" || i.ReceiptID == "" {
		t.Fatal("input rejection did not retain thread/receipt", i.State, i.ErrorCode)
	}
	for j := 0; j < 2; j++ {
		h.tick()
	}
	if len(h.n.adds) != 1 {
		t.Fatal("input rejection retried without explicit action")
	}
	d = h.draft("source")
	if err := h.action(d, "creation_retry", nil); err != nil {
		t.Fatal(err)
	}
	h.tick()
	got := h.intent(d)
	if got.State != "confirmed" || got.NativeThreadID != i.NativeThreadID || got.ReceiptID != i.ReceiptID || len(h.n.adds) != 2 || !reflect.DeepEqual(h.n.adds[0], h.n.adds[1]) || len(h.n.starts) != 1 || h.g.groups != 1 {
		t.Fatal("input retry changed original thread/receipt/client")
	}
	if h.count(`SELECT COUNT(*) FROM submission_attempts WHERE receipt_id=?`, i.ReceiptID) != 2 {
		t.Fatal("input retry did not keep immutable old attempt")
	}
}

func TestManagerCreationNativeUnknownExactSourceRecovery(t *testing.T) {
	for _, match := range []string{"none", "same-directory-wrong-source", "multiple", "incomplete", "unique-loaded", "unique-archived", "unique-listed-null-source", "unique-archived-null-source"} {
		t.Run(match, func(t *testing.T) {
			h := newManagerHarness(t)
			h.n.startErrors = []error{&codexclient.UnknownError{Method: "thread/start", Cause: errors.New("PRIVATE-TRANSPORT-ERROR")}}
			d := h.receive("source", "task")
			h.submitLocal(d)
			if h.intent(d).State != "unknown" || len(h.n.starts) != 1 || len(h.n.adds) != 0 {
				t.Fatal("ambiguous start was treated as definite result")
			}
			thread := h.n.threads["native-1"]
			switch match {
			case "same-directory-wrong-source":
				thread.ThreadSource = json.RawMessage(`"user"`)
				h.n.threads[thread.ID] = thread
				h.n.listed = []codexclient.Thread{thread}
			case "multiple":
				other := thread
				other.ID = "native-duplicate"
				h.n.threads[other.ID] = other
				h.n.listed = []codexclient.Thread{thread, other}
			case "incomplete":
				h.n.listed = []codexclient.Thread{thread, {ID: "metadata-unavailable"}}
			case "unique-loaded":
				h.n.loaded = []string{thread.ID}
			case "unique-archived":
				h.n.archived = []codexclient.Thread{thread}
			case "unique-listed-null-source":
				listed := thread
				listed.ThreadSource = json.RawMessage(`null`)
				h.n.listed = []codexclient.Thread{listed}
			case "unique-archived-null-source":
				listed := thread
				listed.ThreadSource = json.RawMessage(`null`)
				h.n.archived = []codexclient.Thread{listed}
			}
			h.restart()
			for j := 0; j < 3; j++ {
				h.tick()
			}
			resolved := strings.HasPrefix(match, "unique-")
			if resolved && (h.intent(d).State != "confirmed" || len(h.n.adds) != 1) {
				t.Fatal("unique exact source did not recover original thread")
			}
			if resolved && h.n.creationReads == 0 {
				t.Fatal("recovery confirmed metadata without verifying effective policies")
			}
			if !resolved && (h.intent(d).State != "unknown" || len(h.n.adds) != 0) {
				t.Fatal("inconclusive source scan created input/binding")
			}
			if len(h.n.starts) != 1 || h.g.groups != 1 || h.g.roots != 1 {
				t.Fatal("unknown native start was repeated")
			}
			if !resolved {
				before := h.remoteCalls()
				d = h.draft("source")
				if err := h.action(d, "creation_retry", nil); err != nil {
					t.Fatal(err)
				}
				if h.remoteCalls() != before {
					t.Fatal("unknown retry callback called remote")
				}
				h.tick()
				if len(h.n.starts) != 1 || h.intent(d).State != "unknown" {
					t.Fatal("retry action reopened unknown native attempt")
				}
			}
		})
	}
}

func TestManagerCreationUnknownInitialInputOnlyReadsEvidence(t *testing.T) {
	for _, proof := range []string{"none", "queue", "history"} {
		t.Run(proof, func(t *testing.T) {
			h := newManagerHarness(t)
			h.n.addErrors = []error{&codexclient.UnknownError{Method: "thread/queue/add", Cause: errors.New("lost")}}
			d := h.receive("source", "task")
			h.submitLocal(d)
			i := h.intent(d)
			if i.State != "unknown" || i.ErrorCode != "input-unknown" || len(h.n.adds) != 1 {
				t.Fatal("input unknown misclassified")
			}
			h.restart()
			h.tick()
			if len(h.n.adds) != 1 || h.intent(d).State != "unknown" {
				t.Fatal("restart replayed unknown initial input")
			}
			add := h.n.adds[0]
			switch proof {
			case "queue":
				input, _ := json.Marshal(codexclient.TextInput(add.body))
				h.n.queue = []codexclient.Queued{{ID: "retained", ClientUserMessageID: &add.client, Input: input}}
			case "history":
				part, _ := json.Marshal(codexclient.TextInput(add.body)[0])
				h.n.turns = []codexclient.Turn{{ID: "turn", Status: "completed", Items: []codexclient.Item{{ID: "user", Type: "userMessage", ClientID: &add.client, Content: []json.RawMessage{part}}}}}
			}
			for j := 0; j < 2; j++ {
				h.tick()
			}
			if len(h.n.adds) != 1 || len(h.n.starts) != 1 || h.g.groups != 1 {
				t.Fatal("unknown input was resent or created another thread")
			}
			if proof == "none" && h.intent(d).State != "unknown" || proof != "none" && h.intent(d).State != "confirmed" {
				t.Fatal("input evidence not reconciled", proof, h.intent(d).State)
			}
		})
	}
}

func TestManagerCreationEditedFormPersistsFrozenTask(t *testing.T) {
	h := newManagerHarness(t)
	d := h.receive("source", "original task")
	if err := h.action(d, "creation_submit", localCreationForm(map[string]any{"creation_body": "edited task", "creation_mode": "group"})); err != nil {
		t.Fatal(err)
	}
	h.tick()
	i := h.intent(d)
	updated := h.draft("source")
	if i.Body != "edited task" || updated.Body != i.Body || h.n.adds[0].body != i.Body {
		t.Fatal("submitted form/body differs across intent, input and persisted card")
	}
}

func TestManagerCreationCardUnknownStartupReadback(t *testing.T) {
	for _, step := range []string{"entry", "root"} {
		t.Run(step, func(t *testing.T) {
			h := newManagerHarness(t)
			if step == "entry" {
				h.g.replyErr = &feishu.Unknown{Operation: "reply"}
			}
			d := h.receive("source", "task")
			if step == "root" {
				h.g.rootErr = &feishu.Unknown{Operation: "send"}
				h.submitLocal(d)
			} else if d.CardID != "" {
				t.Fatal("lost entry reply was prematurely confirmed")
			}
			h.restart()
			for j := 0; j < 2; j++ {
				h.tick()
			}
			if h.g.replies != 1 || len(h.n.starts) != 0 || len(h.n.adds) != 0 || step == "root" && h.g.roots != 1 {
				t.Fatal("unknown card was resent or native creation advanced without root proof")
			}
			h.g.readback = true
			h.tick()
			if step == "entry" {
				d = h.draft("source")
				if d.CardID != "om_entry_1" {
					t.Fatal("exact entry marker not recovered")
				}
				h.submitLocal(d)
			}
			h.tick()
			if h.intent(d).State != "confirmed" || h.g.replies != 1 || h.g.groups != 1 || h.g.roots != 1 || len(h.n.starts) != 1 || len(h.n.adds) != 1 {
				t.Fatal("card readback failed to resume once through original workflow")
			}
		})
	}
}

func TestManagerCreationNativeConfirmationSaveFailureRecovery(t *testing.T) {
	h := newManagerHarness(t)
	d := h.receive("source", "task")
	_, err := h.s.DB.Exec(`CREATE TRIGGER fail_native_confirm BEFORE UPDATE OF state ON creation_attempts WHEN OLD.step='native' AND NEW.state='confirmed' BEGIN SELECT RAISE(FAIL,'fixture journal failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	h.submitLocal(d)
	a, err := h.s.CreationAttempt(context.Background(), d.ID, "native")
	if err != nil || a.State != "dispatching" || len(h.n.starts) != 1 || len(h.n.adds) != 0 {
		t.Fatal("fixture did not interrupt native confirmation persistence", err)
	}
	if _, err = h.s.DB.Exec(`DROP TRIGGER fail_native_confirm`); err != nil {
		t.Fatal(err)
	}
	h.restart()
	a, err = h.s.CreationAttempt(context.Background(), d.ID, "native")
	if err != nil || a.State != "unknown" {
		t.Fatal("startup did not fence interrupted dispatch", err)
	}
	h.tick()
	if len(h.n.starts) != 1 || len(h.n.adds) != 0 {
		t.Fatal("restart blindly repeated ambiguous native creation")
	}
	h.n.loaded = []string{"native-1"}
	h.tick()
	h.tick()
	if h.intent(d).State != "confirmed" || len(h.n.starts) != 1 || len(h.n.adds) != 1 || h.g.groups != 1 {
		t.Fatal("exact source did not recover confirmation-save failure")
	}
}

func TestManagerCreationNativeRecoveryRejectsMismatchedMetadata(t *testing.T) {
	for _, variant := range []string{"source", "cwd", "project", "ephemeral", "parent", "fork", "id"} {
		t.Run(variant, func(t *testing.T) {
			h := newManagerHarness(t)
			h.n.projects = []codexclient.Project{{ID: "p", Roots: []codexclient.ProjectRoot{{Path: h.root}}}}
			d := h.receive("source", "task")
			if err := h.m.Action(context.Background(), d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_pick", map[string]any{"path": h.root, "project_id": "p"}, nil); err != nil {
				t.Fatal(err)
			}
			h.tick()
			d = h.draft("source")
			h.n.startErrors = []error{&codexclient.UnknownError{Method: "thread/start", Cause: errors.New("lost")}}
			h.submitLocal(d)
			original := h.n.threads["native-1"]
			h.n.listed = []codexclient.Thread{original}
			modified := original
			switch variant {
			case "source":
				modified.ThreadSource = json.RawMessage(`"user"`)
			case "cwd":
				modified.CWD = h.root + "-other"
			case "project":
				id := "foreign"
				modified.ProjectID = &id
			case "ephemeral":
				modified.Ephemeral = true
			case "parent":
				id := "parent"
				modified.ParentThreadID = &id
			case "fork":
				id := "parent"
				modified.ForkedFromID = &id
			case "id":
				modified.ID = "other"
			}
			h.n.threads["native-1"] = modified
			for j := 0; j < 2; j++ {
				h.tick()
			}
			if h.intent(d).State != "unknown" || len(h.n.starts) != 1 || len(h.n.adds) != 0 || h.count(`SELECT COUNT(*) FROM bindings`) != 0 {
				t.Fatal("mismatched metadata advanced native recovery")
			}
		})
	}
}

func TestManagerCreationDiagnosticsOmitPrivateEvidence(t *testing.T) {
	h := newManagerHarness(t)
	h.n.startErrors = []error{&codexclient.UnknownError{Method: "thread/start", Cause: errors.New("PRIVATE-RESPONSE")}}
	d := h.receive("source", "PRIVATE-TASK-BODY")
	h.submitLocal(d)
	h.n.listed = []codexclient.Thread{h.n.threads["native-1"]}
	h.n.archiveErr = errors.New("PRIVATE-NATIVE-ERROR " + h.root)
	h.tick()
	if len(h.notices) == 0 {
		t.Fatal("fixture did not exercise diagnostic error path")
	}
	for _, notice := range h.notices {
		if strings.Contains(notice, "PRIVATE-") || strings.Contains(notice, h.root) {
			t.Fatal("private native/body/path evidence leaked through notice")
		}
	}
}

func TestManagerCreationFollowUpAfterNativeConnectionChange(t *testing.T) {
	h := newManagerHarness(t)
	d := h.receive("source", "initial task")
	h.submitLocal(d)
	i := h.intent(d)
	next := &managerNative{h: h, threads: h.n.threads, queue: h.n.queue}
	h.m.Native = func() Native { return next }
	h.tick()
	r, fresh, err := h.s.Claim(context.Background(), store.Receipt{AppID: "app", MessageID: "follow-source", ThreadID: i.NativeThreadID, ChatID: i.ChatID, Body: "follow-up", ActionToken: store.ID()})
	if err != nil || !fresh {
		t.Fatal("follow-up receipt rejected", err)
	}
	c := &submissions.Controller{Store: h.s, Native: next, HostID: "host", AppID: "app", ThreadID: i.NativeThreadID}
	if err = c.Tick(context.Background()); err != nil {
		t.Fatal("follow-up on replacement connection failed", err)
	}
	if len(next.starts) != 0 || len(next.adds) != 1 || next.adds[0].thread != i.NativeThreadID || next.adds[0].client != r.ClientID || h.g.groups != 1 || h.g.roots != 1 {
		t.Fatal("replacement connection did not continue original thread")
	}
}

func TestManagerCreationNativeRecoveryWaitsForEffectivePolicyProof(t *testing.T) {
	h := newManagerHarness(t)
	h.n.startErrors = []error{&codexclient.UnknownError{Method: "thread/start", Cause: errors.New("lost policy response")}}
	d := h.receive("source", "task")
	h.submitLocal(d)
	h.n.listed = []codexclient.Thread{h.n.threads["native-1"]}
	h.n.creationReadErr = &codexclient.UnknownError{Method: "thread/resume", Cause: errors.New("effective policy missing")}
	for j := 0; j < 2; j++ {
		h.tick()
	}
	if h.n.creationReads == 0 || h.intent(d).State != "unknown" || len(h.n.starts) != 1 || len(h.n.adds) != 0 || h.count(`SELECT COUNT(*) FROM bindings`) != 0 {
		t.Fatal("native metadata bypassed effective policy proof")
	}
	h.n.creationReadErr = nil
	h.tick()
	h.tick()
	if h.intent(d).State != "confirmed" || len(h.n.starts) != 1 || len(h.n.adds) != 1 {
		t.Fatal("verified native policy did not allow original thread recovery")
	}
}

func TestManagerCreationRepeatedValidationErrorsKeepDraftEditable(t *testing.T) {
	for _, finish := range []string{"submit", "cancel"} {
		t.Run(finish, func(t *testing.T) {
			h := newManagerHarness(t)
			d := h.receive("source", "task")
			for j := 0; j < 2; j++ {
				previous := d.Revision
				if err := h.action(d, "creation_submit", localCreationForm(map[string]any{"creation_body": ""})); err != nil {
					t.Fatal("current error card could not claim another action", err)
				}
				h.tick()
				d = h.draft("source")
				if d.State != "draft" || d.ErrorCode != "validation" || d.Revision <= previous || d.CardRevision != d.Revision {
					t.Fatal("repeated identical validation error left a consumed card revision")
				}
			}
			selected := directory(t, h.root, "selected")
			if err := h.m.Action(context.Background(), d.CreatorID, d.SourceChatID, d.CardID, d.Token, d.Revision, "creation_pick", map[string]any{"path": selected}, nil); err != nil {
				t.Fatal("error card locked directory selection", err)
			}
			h.tick()
			d = h.draft("source")
			if d.Directory != selected || d.ErrorCode != "" {
				t.Fatal("corrected directory was not saved")
			}
			if finish == "submit" {
				if err := h.action(d, "creation_submit", localCreationForm(map[string]any{"creation_body": "corrected task"})); err != nil {
					t.Fatal("error card could not accept corrected task text", err)
				}
				h.tick()
				if h.draft("source").State != "created" || len(h.n.starts) != 1 || h.n.starts[0].CWD != selected {
					t.Fatal("corrected draft could not create selected task")
				}
			} else {
				if err := h.action(d, "creation_cancel", nil); err != nil {
					t.Fatal("corrected draft could not cancel", err)
				}
				h.tick()
				if h.draft("source").State != "canceled" || h.g.groups != 0 || len(h.n.starts) != 0 {
					t.Fatal("cancel after validation errors created remote state")
				}
			}
		})
	}
}

func TestManagerCreationInProcessConfirmationSaveFailureRecovery(t *testing.T) {
	for _, step := range []string{"native", "root"} {
		t.Run(step, func(t *testing.T) {
			h := newManagerHarness(t)
			d := h.receive("source", "task")
			// The fake remote accepts the request. Only its local confirmation
			// transaction fails, leaving the real dispatch journal unresolved.
			trigger := `CREATE TRIGGER fail_creation_confirm BEFORE UPDATE OF state ON creation_attempts WHEN OLD.step='` + step + `' AND NEW.state='confirmed' BEGIN SELECT RAISE(FAIL,'fixture confirmation failure'); END`
			if _, err := h.s.DB.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			h.submitLocal(d)
			a, err := h.s.CreationAttempt(context.Background(), d.ID, step)
			if err != nil || a.State != "dispatching" {
				t.Fatal("confirmation failure was not left dispatching", err)
			}
			if _, err := h.s.DB.Exec(`DROP TRIGGER fail_creation_confirm`); err != nil {
				t.Fatal(err)
			}
			originalManager := h.m
			h.tick()
			a, err = h.s.CreationAttempt(context.Background(), d.ID, step)
			if err != nil || a.State != "unknown" || h.m != originalManager {
				t.Fatal("same worker did not fence its previous unresolved dispatch", err)
			}
			if step == "native" {
				h.n.loaded = []string{"native-1"}
			} else {
				h.g.readback = true
			}
			for j := 0; j < 3; j++ {
				h.tick()
			}
			if h.intent(d).State != "confirmed" || h.g.groups != 1 || h.g.roots != 1 || len(h.n.starts) != 1 || len(h.n.adds) != 1 {
				t.Fatal("same-process readback retried or stranded accepted remote creation")
			}
			if step == "native" && h.n.creationReads != 1 {
				t.Fatal("native recovery did not verify its existing policy response once")
			}
		})
	}
}

func (h *managerHarness) seedDraft(source string, ordinal int) store.CreationDraft {
	h.t.Helper()
	d, fresh, err := h.s.ReceiveCreationDraft(context.Background(), store.CreationDraft{
		HostID: "host", AppID: "app", CreatorID: "ou_creator", SourceMessageID: source,
		SourceChatID: "oc_private", SourceChatType: "p2p", Body: "task", Directory: h.root, FollowMode: "group",
		CreatedAtMS: h.clock.UnixMilli() - 1000 + int64(ordinal), ExpiresAtMS: h.clock.Add(time.Hour).UnixMilli(),
	})
	if err != nil || !fresh {
		h.t.Fatal("seed draft failed", err)
	}
	return d
}

func (h *managerHarness) seedEntry(d store.CreationDraft) {
	h.t.Helper()
	raw, _ := json.Marshal(cardRequest{Revision: d.Revision})
	a, err := h.s.PrepareCreationAttempt(context.Background(), d.ID, fmt.Sprintf("entry:%d", d.Revision), string(raw), h.clock.UnixMilli())
	if err != nil {
		h.t.Fatal(err)
	}
	if ok, err := h.s.DispatchCreationAttempt(context.Background(), a); err != nil || !ok {
		h.t.Fatal("seed entry dispatch failed", err)
	}
	if err := h.s.FinishCreationAttempt(context.Background(), a, "confirmed", "seed-"+d.ID, "{}"); err != nil {
		h.t.Fatal(err)
	}
}

func TestManagerCreationSyncedDraftsDoNotStarve65thCard(t *testing.T) {
	h := newManagerHarness(t)
	for j := 0; j < 64; j++ {
		h.seedEntry(h.seedDraft(fmt.Sprintf("prior-%02d", j), j))
	}
	d := h.seedDraft("new-65th", 64)
	h.tick()
	d = h.draft(d.SourceMessageID)
	if d.CardID == "" || d.CardRevision != d.Revision || h.g.replies != 1 || h.g.updates != 0 {
		t.Fatal("64 live synchronized drafts starved a new creation card")
	}
	if h.count(`SELECT COUNT(*) FROM creation_drafts WHERE state='draft' AND expires_at_ms>?`, h.clock.UnixMilli()) != 65 {
		t.Fatal("fixture did not retain 64 unexpired prior drafts")
	}
}

func TestManagerCreationUnknownWorkflowsRotatePast64(t *testing.T) {
	h := newManagerHarness(t)
	for j := 0; j < 65; j++ {
		d := h.seedDraft(fmt.Sprintf("unknown-%02d", j), j)
		i, claimed, err := h.s.ClaimCreationIntent(context.Background(), d, store.CreationIntent{OwnerID: "ou_owner", Name: "task", Members: []string{"ou_owner", "ou_creator"}}, h.clock.UnixMilli())
		if err != nil || !claimed {
			t.Fatal("seed unknown intent failed", err)
		}
		p := groupRequest{Name: i.Name, Owner: i.OwnerID, Members: i.Members, UUID: i.ID, Description: "工作目录：" + i.Directory}
		raw, _ := json.Marshal(p)
		a, err := h.s.PrepareCreationAttempt(context.Background(), d.ID, "group", string(raw), h.clock.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := h.s.DispatchCreationAttempt(context.Background(), a); err != nil || !ok {
			t.Fatal("seed unknown dispatch failed", err)
		}
		if err := h.s.FinishCreationAttempt(context.Background(), a, "unknown", "", "{}"); err != nil {
			t.Fatal(err)
		}
		d = h.draft(d.SourceMessageID)
		h.seedEntry(d)
	}
	latest := h.seedDraft("after-unknowns", 65)
	h.tick()
	// Independent UI polling may publish it already; it must be reachable by
	// the next bounded pass even with 65 older uncertain remote workflows.
	h.tick()
	if h.draft(latest.SourceMessageID).CardID == "" || h.g.replies != 1 {
		t.Fatal("persisted polling order did not rotate to the new draft")
	}
	if h.count(`SELECT COUNT(*) FROM creation_workflows WHERE state='unknown'`) != 65 || h.g.groups != 0 || h.g.roots != 0 || len(h.n.starts) != 0 {
		t.Fatal("polling fairness reopened an unknown remote workflow")
	}
}

func enableManagerTopics(h *managerHarness) {
	h.m.Config.Creation.TopicsEnabled = true
	h.m.Config.Creation.WorkgroupChatIDs = []string{"oc_workgroup"}
	h.g.workgroups = map[string]string{"oc_workgroup": "authorized workgroup"}
	h.g.readback = true
}

func (h *managerHarness) submitTopic(d store.CreationDraft) {
	h.t.Helper()
	if err := h.action(d, "creation_submit", localCreationForm(map[string]any{"creation_mode": "topic", "creation_target": "oc_workgroup"})); err != nil {
		h.t.Fatal(err)
	}
	h.tick()
}

func TestManagerCreationTopicsInSameWorkgroupKeepDistinctRootsAndReceipts(t *testing.T) {
	h := newManagerHarness(t)
	enableManagerTopics(h)
	// Real root GETs may report no root/thread yet, or self-root plus an empty
	// parent. Both are valid roots, never inferred from a successful reply.
	h.g.afterRoot = func(id string) {
		if h.g.roots == 2 {
			message := h.g.cards[id]
			empty := ""
			thread := "platform-topic-2"
			message.RootID = &id
			message.ParentID = &empty
			message.ThreadID = &thread
			h.g.cards[id] = message
		}
	}
	first := h.receive("private-one", "first independent task")
	second := h.receive("private-two", "second independent task")
	h.submitTopic(first)
	h.submitTopic(second)
	one, two := h.intent(first), h.intent(second)
	if one.State != "confirmed" || two.State != "confirmed" || one.FollowMode != "topic" || two.FollowMode != "topic" || one.TargetChatID != "oc_workgroup" || two.TargetChatID != "oc_workgroup" {
		t.Fatal("private topic drafts did not complete in the authorized workgroup")
	}
	if one.RootMessageID == "" || one.RootMessageID == two.RootMessageID || one.NativeThreadID == two.NativeThreadID || one.ReceiptID == two.ReceiptID || h.n.starts[0].IntentID == h.n.starts[1].IntentID {
		t.Fatal("same-workgroup topic creation merged independent identities")
	}
	for _, pair := range []struct {
		draft  store.CreationDraft
		intent store.CreationIntent
	}{{first, one}, {second, two}} {
		binding, err := h.s.Binding(context.Background(), "host", pair.intent.NativeThreadID)
		if err != nil || binding.ChatID != "oc_workgroup" || binding.FollowMode != "topic" || binding.RootMessageID != pair.intent.RootMessageID || binding.IntentID != pair.intent.ID || binding.RepairRequired {
			t.Fatal("topic binding lost exact root/intent identity", err)
		}
		var source, reply, chat, thread, client string
		if err := h.s.DB.QueryRow(`SELECT message_id,reply_message_id,chat_id,thread_id,client_id FROM m0_receipts WHERE id=?`, pair.intent.ReceiptID).Scan(&source, &reply, &chat, &thread, &client); err != nil {
			t.Fatal(err)
		}
		if source != pair.draft.SourceMessageID || reply != pair.intent.RootMessageID || chat != "oc_workgroup" || thread != pair.intent.NativeThreadID {
			t.Fatal("first input used another private source or topic anchor")
		}
		found := false
		for _, add := range h.n.adds {
			if add.thread == thread && add.client == client && add.body == pair.intent.Body {
				found = true
			}
		}
		if !found {
			t.Fatal("topic first input did not use its original native thread/receipt")
		}
	}
	for j := 0; j < 2; j++ {
		h.tick()
	}
	if h.g.groups != 0 || h.g.roots != 2 || len(h.n.starts) != 2 || len(h.n.adds) != 2 || h.g.getCalls < 4 || h.count(`SELECT COUNT(*) FROM creation_attempts WHERE step='group'`) != 0 {
		t.Fatal("topic flow skipped fresh GETs, created a group, or repeated objects")
	}
}

func TestManagerCreationTopicRootDeletionStopsNativeAndInitialInput(t *testing.T) {
	for _, when := range []string{"before-native", "before-initial-add"} {
		t.Run(when, func(t *testing.T) {
			h := newManagerHarness(t)
			enableManagerTopics(h)
			d := h.receive("private-source", "topic task")
			markDeleted := func(id string) {
				message := h.g.cards[id]
				deleted := true
				message.Deleted = &deleted
				h.g.cards[id] = message
			}
			if when == "before-native" {
				h.g.afterRoot = markDeleted
			} else {
				h.n.afterStart = func(string) { markDeleted(h.intent(d).RootMessageID) }
			}
			h.submitTopic(d)
			if h.draft(d.SourceMessageID).ErrorCode != "target-root" || len(h.n.adds) != 0 || h.g.groups != 0 || h.g.roots != 1 {
				t.Fatal("deleted topic root did not block the creation write")
			}
			if when == "before-native" {
				if len(h.n.starts) != 0 || h.count(`SELECT COUNT(*) FROM bindings`) != 0 {
					t.Fatal("deleted root still created a native thread/binding")
				}
			} else {
				i := h.intent(d)
				binding, err := h.s.Binding(context.Background(), "host", i.NativeThreadID)
				if err != nil || !binding.RepairRequired || binding.RepairReason != "topic_root_unavailable" || len(h.n.starts) != 1 {
					t.Fatal("root deleted after native confirmation did not fence its existing binding", err)
				}
				attempt, err := h.s.CreationAttempt(context.Background(), d.ID, "native")
				if err != nil || attempt.State != "confirmed" {
					t.Fatal("confirmed native identity was discarded", err)
				}
				if h.count(`SELECT COUNT(*) FROM submission_attempts WHERE receipt_id=?`, i.ReceiptID) != 0 {
					t.Fatal("initial queue write was dispatched before final root verification")
				}
			}
			for j := 0; j < 2; j++ {
				h.tick()
			}
			if len(h.n.adds) != 0 || h.g.roots != 1 || h.g.groups != 0 || when == "before-native" && len(h.n.starts) != 0 || when == "before-initial-add" && len(h.n.starts) != 1 {
				t.Fatal("deleted topic root reopened or duplicated remote creation")
			}
		})
	}
}

func TestManagerCreationTopicGuardLeavesGroupFlowIndependentOfRootReadback(t *testing.T) {
	h := newManagerHarness(t)
	// ReadCardMessage intentionally returns unavailable, as it did before
	// topic creation. Ordinary dedicated groups need no new root GET proof.
	d := h.receive("private-source", "group task")
	h.submitLocal(d)
	if h.intent(d).State != "confirmed" || h.g.getCalls != 0 || h.g.groups != 1 || len(h.n.starts) != 1 || len(h.n.adds) != 1 {
		t.Fatal("topic root guard imposed a new root-read requirement on groups")
	}
}
