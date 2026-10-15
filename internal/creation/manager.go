package creation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codex-feishu/internal/bindings"
	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"codex-feishu/internal/submissions"
	messages "codex-feishu/internal/sync"
)

type Native interface {
	submissions.Native
	ListProjects(context.Context) ([]codexclient.Project, error)
	StartCreation(context.Context, codexclient.CreationSpec) (codexclient.CreationResult, error)
	Metadata(context.Context, string) (codexclient.Thread, error)
	ListArchive(context.Context, bool) ([]codexclient.Thread, error)
	LoadedIDs(context.Context) ([]string, error)
}

type Gateway interface {
	bindings.Groups
	messages.MessageGateway
	messages.MessageReadbackGateway
	MemberIDs(context.Context, string) ([]string, error)
	BotInChat(context.Context, string) (bool, error)
	GetGroupInfo(context.Context, string) (feishu.GroupInfo, error)
}

type Inbound struct {
	MessageID, ChatID, ChatType, UserID, Text, RootID, PlatformThreadID string
	Bound                                                               bool
	Topic                                                               bool
}

const MaxTaskRunes = 4000

var ErrTaskTooLong = errors.New("creation task exceeds supported text length")

type actionPayload struct {
	Form  map[string]any `json:"form"`
	Value map[string]any `json:"value"`
}

// Manager has one UI writer and one remote-creation worker. Ingress and card
// callbacks only commit local state; each lane journals before remote writes.
type Manager struct {
	Store            *store.Store
	Config           config.Config
	Gateway          Gateway
	Native           func() Native
	Activate         func(string)
	Now              func() time.Time
	Notice           func(string)
	mu               sync.Mutex
	uiMu             sync.Mutex
	wakeOnce         sync.Once
	uiWake, workWake chan struct{}
	workgroupNames   map[string]string // UI-lane display cache, never authorization.
	controllers      map[string]*submissions.Controller
	controllerNative Native
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}
func (m *Manager) notice(code string) {
	if m.Notice != nil {
		m.Notice(code)
	}
}
func contains(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
func (m *Manager) directories(n Native) DirectoryService {
	s := DirectoryService{Roots: m.Config.Creation.AllowedRoots, Common: m.Config.Creation.CommonDirectories}
	if n != nil {
		s.Projects = n.ListProjects
	}
	return s
}
func textValue(values map[string]any, key string) string { v, _ := values[key].(string); return v }

// Receive handles only explicit /new or a permitted unbound text entrance.
// All other slash commands retain their original management routing.
func (m *Manager) Receive(ctx context.Context, in Inbound) (bool, error) {
	if !m.Config.Creation.Enabled || !m.Config.Operator(in.UserID) {
		return false, nil
	}
	text := strings.TrimSpace(in.Text)
	fields := strings.Fields(text)
	explicit := len(fields) > 0 && fields[0] == "/new"
	if strings.HasPrefix(text, "/") && !explicit {
		return false, nil
	}
	if in.Bound && !explicit {
		return false, nil
	}
	if in.ChatType != "p2p" && (in.ChatType != "group" || !in.Bound && !contains(m.Config.Creation.EntryChatIDs, in.ChatID)) {
		return false, nil
	}
	if explicit {
		text = strings.TrimSpace(strings.TrimPrefix(text, "/new"))
	}
	if utf8.RuneCountInString(text) > MaxTaskRunes {
		return true, ErrTaskTooLong
	}
	mode := m.Config.Creation.DefaultFollowMode
	if mode == "" {
		mode = "group"
	}
	target := ""
	if contains(m.Config.Creation.WorkgroupChatIDs, in.ChatID) {
		target = in.ChatID
	}
	d := store.CreationDraft{HostID: m.Config.Host.ID, AppID: m.Config.Feishu.AppID, CreatorID: in.UserID, SourceMessageID: in.MessageID, SourceChatID: in.ChatID, SourceChatType: in.ChatType, Body: text, FollowMode: mode, TargetChatID: target, CreatedAtMS: m.now().UnixMilli(), ExpiresAtMS: m.now().Add(m.Config.CreationTTL()).UnixMilli()}
	if in.Topic {
		d.SourceRootID, d.SourceThreadID = in.RootID, in.PlatformThreadID
	}
	var recent string
	err := m.Store.DB.QueryRowContext(ctx, `SELECT directory FROM creation_drafts WHERE host_id=? AND app_id=? AND creator_id=? AND state='created' ORDER BY created_at_ms DESC LIMIT 1`, d.HostID, d.AppID, d.CreatorID).Scan(&recent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return true, err
	}
	choice, err := m.directories(nil).Default(ctx, recent)
	if err != nil {
		return true, err
	}
	d.Directory = choice.Path
	_, _, err = m.Store.ReceiveCreationDraft(ctx, d)
	if err == nil {
		m.signalUI()
	}
	return true, err
}

func (m *Manager) Action(ctx context.Context, user, chat, message, token string, revision int64, kind string, value, form map[string]any) error {
	if !m.Config.Creation.Enabled || !m.Config.Operator(user) {
		return store.ErrSubmissionStale
	}
	switch kind {
	case "creation_submit", "creation_browse", "creation_back", "creation_pick", "creation_path", "creation_cancel", "creation_retry", "creation_check", "creation_search", "creation_search_next", "creation_search_pick", "creation_panel", "creation_goto", "creation_filter", "creation_lookup", "creation_projects":
	default:
		return store.ErrSubmissionStale
	}
	payload, err := json.Marshal(actionPayload{Form: form, Value: value})
	if err != nil {
		return store.ErrSubmissionStale
	}
	_, _, err = m.Store.ClaimCreationAction(ctx, m.Config.Host.ID, m.Config.Feishu.AppID, user, chat, message, token, revision, kind, string(payload), m.now().UnixMilli())
	if err == nil {
		m.signalUI()
	}
	return err
}

func (m *Manager) initWake() {
	m.wakeOnce.Do(func() {
		m.uiWake, m.workWake = make(chan struct{}, 1), make(chan struct{}, 1)
	})
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (m *Manager) signalUI()   { m.initWake(); signal(m.uiWake) }
func (m *Manager) signalWork() { m.initWake(); signal(m.workWake) }

func (m *Manager) runLane(ctx context.Context, wake <-chan struct{}, timeout time.Duration, tick func(context.Context) error) {
	timer := time.NewTicker(time.Second) // Recovery fallback, not interaction latency.
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-wake:
		}
		call, cancel := context.WithTimeout(ctx, timeout)
		if err := tick(call); err != nil && ctx.Err() == nil {
			m.notice("creation.work_pending")
		}
		cancel()
	}
}

func (m *Manager) Run(ctx context.Context) {
	m.initWake()
	m.signalUI()
	m.signalWork()
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		m.runLane(ctx, m.workWake, 30*time.Second, m.tickWorkflows)
	}()
	m.runLane(ctx, m.uiWake, 10*time.Second, m.tickUI)
	workers.Wait()
}

// Tick preserves the synchronous integration/test contract. Run schedules the
// same two bounded owners independently, so native RPCs do not block browsing.
func (m *Manager) Tick(ctx context.Context) error {
	if err := m.tickUI(ctx); err != nil {
		return err
	}
	if err := m.tickWorkflows(ctx); err != nil {
		return err
	}
	return m.tickUI(ctx)
}

func (m *Manager) tickWorkflows(ctx context.Context) error {
	if !m.mu.TryLock() {
		return nil
	}
	defer m.mu.Unlock()
	if !m.Config.Creation.Enabled {
		return nil
	}
	if err := m.Store.RecoverCreationWorkflows(ctx, m.Config.Host.ID, m.Config.Feishu.AppID); err != nil {
		return err
	}
	var n Native
	if m.Native != nil {
		n = m.Native()
	}
	drafts, err := m.Store.CreatingDrafts(ctx, m.Config.Host.ID, m.Config.Feishu.AppID)
	if err != nil {
		return err
	}
	for _, d := range drafts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.Store.PollCreationDraft(ctx, d.HostID, d.AppID, d.ID, m.now().UnixMilli()); err != nil {
			return err
		}
		if n != nil {
			if err := m.advance(ctx, n, d); err != nil {
				m.notice("creation.reconciliation_pending")
			}
		}
		m.signalUI()
	}
	return nil
}

func (m *Manager) tickUI(ctx context.Context) error {
	if !m.uiMu.TryLock() {
		return nil
	}
	defer m.uiMu.Unlock()
	if !m.Config.Creation.Enabled {
		return nil
	}
	if err := m.Store.RecoverCreationUI(ctx, m.Config.Host.ID, m.Config.Feishu.AppID); err != nil {
		return err
	}
	var n Native
	if m.Native != nil {
		n = m.Native()
	}
	for j := 0; j < 8; j++ {
		a, ok, err := m.Store.NextCreationAction(ctx, m.Config.Host.ID, m.Config.Feishu.AppID)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		ready, err := m.actionBusy(ctx, a)
		if err != nil {
			m.notice("creation.card_pending")
			continue // Never turn an uncertain UI write into a result mutation.
		}
		if err == nil && !ready {
			continue // An uncertain busy write must be proven before any read/result.
		}
		if err == nil {
			read, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = m.applyAction(read, n, a)
			cancel()
		}
		if err != nil && !errors.Is(err, store.ErrSubmissionStale) {
			if save := m.rejectSelectionAction(ctx, a, err); save != nil {
				return save
			}
		}
		// A no-op or stale action can still have published a confirmed busy
		// card. Give its unchanged revision a final view rather than leaving
		// that card permanently disabled. Concurrent progress wins the guard.
		current, save := m.Store.CreationDraft(ctx, a.DraftID)
		if save != nil {
			return save
		}
		if save = m.Store.RefreshCreationAction(ctx, a, current.ErrorCode); save != nil {
			return save
		}
		if save := m.Store.FinishCreationAction(ctx, a, err == nil); save != nil {
			return save
		}
		m.signalWork()
	}
	drafts, err := m.Store.CreationDrafts(ctx, m.Config.Host.ID, m.Config.Feishu.AppID, m.now().UnixMilli())
	if err != nil {
		return err
	}
	for _, d := range drafts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := m.Store.PollCreationDraft(ctx, d.HostID, d.AppID, d.ID, m.now().UnixMilli()); err != nil {
			return err
		}
		if err := m.entryCard(ctx, n, d); err != nil {
			m.notice("creation.card_pending")
		}
	}
	return nil
}

func actionError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "directory-read"
	}
	if errors.Is(err, ErrSearchSelectionRequired) {
		return "directory-selection"
	}
	if errors.Is(err, ErrDirectoryLimit) {
		return "directory-limit"
	}
	if errors.Is(err, ErrTaskTooLong) {
		return "task-length"
	}
	if errors.Is(err, ErrDirectoryOutsideRoots) {
		return "directory-scope"
	}
	if errors.Is(err, ErrDirectoryProject) {
		return "project"
	}
	if errors.Is(err, ErrDirectoryUnavailable) {
		return "directory"
	}
	return "validation"
}

func (m *Manager) applyAction(ctx context.Context, n Native, a store.CreationAction) error {
	d, err := m.Store.CreationDraft(ctx, a.DraftID)
	if err != nil {
		return err
	}
	if d.Revision != a.Revision {
		return nil
	} // A local mutation already committed before restart.
	if !m.Config.Operator(d.CreatorID) {
		return store.ErrSubmissionStale
	}
	var p actionPayload
	if json.Unmarshal([]byte(a.Payload), &p) != nil {
		return store.ErrSubmissionStale
	}
	if a.Kind == "creation_retry" {
		return m.Store.RetryCreation(ctx, d)
	}
	if a.Kind == "creation_check" {
		return m.Store.RefreshCreationAction(ctx, a, "")
	}
	if d.State != "draft" {
		return nil
	}
	if a.Kind == "creation_cancel" {
		return m.Store.CancelCreationDraft(ctx, d, m.now().UnixMilli())
	}
	if body, ok := p.Form["creation_body"].(string); ok {
		d.Body = strings.TrimSpace(body)
	}
	if mode := textValue(p.Form, "creation_mode"); mode != "" {
		d.FollowMode = mode
	}
	if target, ok := p.Form["creation_target"].(string); ok {
		d.TargetChatID = target
	}
	if utf8.RuneCountInString(d.Body) > MaxTaskRunes {
		return ErrTaskTooLong
	}
	if d.FollowMode != "group" && d.FollowMode != "topic" || d.FollowMode == "topic" && !m.Config.Creation.TopicsEnabled {
		return store.ErrSubmissionStale
	}
	if a.Kind == "creation_submit" && d.View != "main" {
		return ErrSearchSelectionRequired
	}
	if d.ExpiresAtMS <= m.now().UnixMilli() {
		return store.ErrSubmissionStale
	}
	dirs := m.directories(nil)
	switch a.Kind {
	case "creation_search", "creation_search_next", "creation_projects", "creation_pick", "creation_path", "creation_search_pick", "creation_submit":
		dirs, err = m.selectionDirectories(ctx, n, d.CreatorID)
		if err != nil {
			return err
		}
	}
	if path := textValue(p.Form, "creation_directory"); d.View == "main" && path != "" && (path != d.Directory || d.ProjectID == "") {
		choice, err := dirs.Validate(ctx, path, "")
		if err == nil && a.Kind == "creation_submit" {
			choice, err = selectDirectory(ctx, dirs, path)
		}
		if err != nil {
			return err
		}
		d.Directory, d.ProjectID = choice.Path, choice.ProjectID
	}
	switch a.Kind {
	case "creation_search", "creation_search_next", "creation_projects":
		query := strings.TrimSpace(textValue(p.Form, "creation_query"))
		if a.Kind == "creation_projects" {
			query = ""
		} else if a.Kind == "creation_search_next" {
			if _, supplied := p.Form["creation_query"]; !supplied {
				query = d.BrowsePath
			}
		}
		if utf8.RuneCountInString(query) > 1000 {
			return ErrDirectoryUnavailable
		}
		after := ""
		if a.Kind == "creation_search_next" && query == d.BrowsePath {
			after = textValue(p.Value, "after")
		}
		if filepath.IsAbs(query) {
			if choice, err := dirs.Browse(ctx, query); err == nil {
				d.View, d.BrowsePath, d.BrowseCursor = "directories", choice.Path, ""
				break
			}
			if _, err := m.directories(nil).Search(ctx, query, after, 100); err != nil {
				return err
			}
		}
		d.View, d.BrowsePath, d.BrowseCursor = "search", query, after
	case "creation_panel":
		var path string
		for _, candidate := range append([]string{d.Directory}, dirs.Roots...) {
			if choice, err := dirs.Browse(ctx, candidate); err == nil {
				path = choice.Path
				break
			}
		}
		if path == "" {
			return ErrDirectoryUnavailable
		}
		d.View, d.BrowsePath, d.BrowseCursor = "directories", path, ""
	case "creation_lookup":
		query := strings.TrimSpace(textValue(p.Form, "creation_query"))
		if err := validateDirectoryFilter(query); err != nil {
			return err
		}
		if filepath.IsAbs(query) {
			choice, err := dirs.Browse(ctx, query)
			if err != nil {
				return err
			}
			d.View, d.BrowsePath, d.BrowseCursor = "directories", choice.Path, ""
		} else {
			if _, err := dirs.ListMatching(ctx, d.BrowsePath, query, "", 100); err != nil {
				return err
			}
			d.View, d.BrowseCursor = "directories", directoryCursor(query, "")
		}
	case "creation_goto":
		choice, err := dirs.Browse(ctx, strings.TrimSpace(textValue(p.Form, "creation_path")))
		if err != nil {
			return err
		}
		d.View, d.BrowsePath, d.BrowseCursor = "directories", choice.Path, ""
	case "creation_filter":
		filter := textValue(p.Form, "creation_filter")
		if err := validateDirectoryFilter(filter); err != nil {
			return err
		}
		if _, err := dirs.ListMatching(ctx, d.BrowsePath, filter, "", 100); err != nil {
			return err
		}
		d.View, d.BrowseCursor = "directories", directoryCursor(filter, "")
	case "creation_browse":
		path := textValue(p.Value, "option")
		if path == "" {
			path = textValue(p.Value, "path")
		}
		if _, fixed := p.Value["path"]; !fixed && path == "" {
			path = textValue(p.Form, "creation_entry")
		}
		if path != "" {
			choice, err := dirs.Browse(ctx, path)
			if err != nil {
				return err
			}
			path = choice.Path
		}
		filter, after := "", textValue(p.Value, "after")
		if after != "" && path == d.BrowsePath {
			previous, _, err := parseDirectoryCursor(d.BrowseCursor)
			if err != nil {
				return err
			}
			filter = previous
			if changed, ok := p.Form["creation_filter"].(string); ok && changed != previous {
				filter, after = changed, ""
			}
			if err := validateDirectoryFilter(filter); err != nil {
				return err
			}
		}
		d.View, d.BrowsePath, d.BrowseCursor = "directories", path, directoryCursor(filter, after)
	case "creation_back":
		d.View, d.BrowsePath, d.BrowseCursor = "main", "", ""
	case "creation_pick", "creation_path", "creation_search_pick":
		path, project := textValue(p.Value, "path"), textValue(p.Value, "project_id")
		if option := textValue(p.Value, "option"); option != "" {
			path, project = option, ""
		}
		if a.Kind == "creation_path" {
			path, project = textValue(p.Form, "creation_path"), ""
		} else if a.Kind == "creation_search_pick" {
			path, project = textValue(p.Form, "creation_search_choice"), ""
		} else if _, fixed := p.Value["path"]; !fixed && path == "" {
			path = textValue(p.Form, "creation_entry")
		}
		choice, err := dirs.Validate(ctx, path, project)
		if err != nil {
			return err
		}
		if project == "" {
			choice, err = selectDirectory(ctx, dirs, path)
			if err != nil {
				return err
			}
		}
		resetDirectoryView(&d, choice)
	case "creation_submit":
		if n == nil {
			return errors.New("creation requires native connection")
		}
		if strings.TrimSpace(d.Body) == "" {
			return errors.New("creation requires task text")
		}
		choice, err := dirs.Validate(ctx, d.Directory, d.ProjectID)
		if err == nil && d.ProjectID == "" {
			choice, err = selectDirectory(ctx, dirs, d.Directory)
		}
		if err != nil {
			return err
		}
		d.Directory, d.ProjectID = choice.Path, choice.ProjectID
		if d.FollowMode == "topic" {
			if !contains(m.Config.Creation.WorkgroupChatIDs, d.TargetChatID) {
				return errors.New("creation requires an authorized workgroup")
			}
			if err = m.verifyTarget(ctx, d.TargetChatID, d.CreatorID, false); err != nil {
				return err
			}
		}
		name := []rune(d.Body)
		if len(name) > 40 {
			name = name[:40]
		}
		i := store.CreationIntent{OwnerID: m.Config.Access.OwnerOpenID, Name: bindings.WithRobotPrefix(string(name)), Members: append([]string{m.Config.Access.OwnerOpenID, d.CreatorID}, m.Config.Groups.AdditionalMemberOpenIDs...)}
		_, _, err = m.Store.ClaimCreationIntent(ctx, d, i, m.now().UnixMilli())
		return err
	default:
		return store.ErrSubmissionStale
	}
	d.ErrorCode = ""
	_, err = m.Store.UpdateCreationDraft(ctx, d, a.Revision, m.now().UnixMilli())
	return err
}

func (m *Manager) verifyTarget(ctx context.Context, chat, creator string, owned bool) error {
	present, err := m.Gateway.BotInChat(ctx, chat)
	if err != nil || !present {
		return errors.New("creation target bot membership unavailable")
	}
	info, err := m.Gateway.GetGroupInfo(ctx, chat)
	if err != nil {
		return err
	}
	if info.ChatMode != "group" || owned && info.OwnerID != m.Config.Access.OwnerOpenID {
		return errors.New("creation target identity mismatch")
	}
	members, err := m.Gateway.MemberIDs(ctx, chat)
	if err != nil {
		return err
	}
	if !contains(members, creator) || owned && !contains(members, m.Config.Access.OwnerOpenID) {
		return errors.New("creation target user membership unavailable")
	}
	return nil
}

type groupRequest struct {
	Name, Owner, UUID, Description string
	Members                        []string
}
type cardRequest struct {
	Operation, ChatID, ParentID, TargetID, Card, Marker, UUID, FollowMode, RootID, ThreadID string
	Revision                                                                                int64 `json:"revision"`
}

func (m *Manager) advance(ctx context.Context, n Native, d store.CreationDraft) error {
	i, err := m.Store.CreationIntent(ctx, d.ID)
	if err != nil {
		return err
	}
	if !m.Config.Operator(i.CreatorID) || i.OwnerID != m.Config.Access.OwnerOpenID {
		return store.ErrConflict
	}
	if !sameMembers(i.Members, append([]string{m.Config.Access.OwnerOpenID, i.CreatorID}, m.Config.Groups.AdditionalMemberOpenIDs...)) {
		return store.ErrConflict
	}
	if i.State == "rejected" || i.State == "confirmed" {
		return nil
	}
	if _, err = m.directories(n).Validate(ctx, i.Directory, i.ProjectID); err != nil {
		return m.Store.SetCreationError(ctx, d.ID, actionError(err))
	}
	if i.FollowMode == "topic" && (!m.Config.Creation.TopicsEnabled || !contains(m.Config.Creation.WorkgroupChatIDs, i.TargetChatID)) {
		return store.ErrConflict
	}
	chat := i.ChatID
	if i.FollowMode == "group" && chat == "" {
		p := groupRequest{Name: i.Name, Owner: i.OwnerID, Members: i.Members, UUID: i.ID, Description: "工作目录：" + i.Directory}
		raw, _ := json.Marshal(p)
		a, err := m.Store.PrepareCreationAttempt(ctx, d.ID, "group", string(raw), m.now().UnixMilli())
		if err != nil {
			return err
		}
		if a.State == "unknown" || a.State == "rejected" {
			return nil
		}
		if a.State == "confirmed" {
			chat = a.RemoteID
		} else {
			ok, err := m.Store.DispatchCreationAttempt(ctx, a)
			if err != nil || !ok {
				return err
			}
			id, remoteErr := m.Gateway.CreateGroupWithDescription(ctx, p.Name, p.Owner, p.Members, p.UUID, p.Description)
			if err = m.Store.FinishCreationAttempt(ctx, a, remoteOutcome(remoteErr, id), id, "{}"); err != nil {
				return err
			}
			if remoteErr != nil || id == "" {
				return nil
			}
			chat = id
		}
	}
	if i.FollowMode == "topic" {
		chat = i.TargetChatID
	}
	if err = m.verifyTarget(ctx, chat, i.CreatorID, i.FollowMode == "group"); err != nil {
		return m.Store.SetCreationError(ctx, d.ID, "target-membership")
	}
	if i.RootMessageID == "" {
		root, err := m.ensureCard(ctx, d.ID, "root", cardRequest{Operation: "send", ChatID: chat}, cards.CreationRoot(i.Body, i.Directory, "来自飞书创建"))
		if err != nil || root == "" {
			return err
		}
		i.RootMessageID = root
	}
	if err = m.verifyCreationRoot(ctx, i); err != nil {
		return err
	}
	if i.NativeThreadID == "" {
		choice, err := m.directories(n).Validate(ctx, i.Directory, i.ProjectID)
		if err != nil || choice.Path != i.Directory {
			return m.Store.SetCreationError(ctx, d.ID, "directory")
		}
		spec := codexclient.CreationSpec{IntentID: i.ID, CWD: i.Directory, ProjectID: i.ProjectID}
		params, err := codexclient.CreationRequestJSON(spec)
		if err != nil {
			return err
		}
		a, err := m.Store.PrepareCreationAttempt(ctx, d.ID, "native", params, m.now().UnixMilli())
		if err != nil {
			return err
		}
		if a.State == "unknown" {
			return m.reconcileNative(ctx, n, i, a)
		}
		if a.State == "rejected" {
			return nil
		}
		if a.State == "confirmed" {
			i.NativeThreadID = a.RemoteID
		} else {
			ok, err := m.Store.DispatchCreationAttempt(ctx, a)
			if err != nil || !ok {
				return err
			}
			result, remoteErr := n.StartCreation(ctx, spec)
			resultJSON, _ := json.Marshal(result)
			if err = m.Store.FinishCreationAttempt(ctx, a, remoteOutcome(remoteErr, result.Thread.ID), result.Thread.ID, string(resultJSON)); err != nil {
				return err
			}
			if remoteErr != nil || result.Thread.ID == "" {
				return nil
			}
			i.NativeThreadID = result.Thread.ID
		}
	}
	// The binding is saved before input, but the ordinary actor is activated
	// only after input acceptance. Thus it cannot resume a nonexistent rollout.
	b, r, err := m.Store.MaterializeCreation(ctx, d.ID)
	if err != nil {
		return err
	}
	if m.controllers == nil || m.controllerNative != n {
		m.controllers = map[string]*submissions.Controller{}
		m.controllerNative = n
	}
	c := m.controllers[b.ThreadID]
	if c == nil {
		c = &submissions.Controller{Store: m.Store, Native: n, HostID: b.HostID, AppID: i.AppID, ThreadID: b.ThreadID}
		c.BeginWrite = func(ctx context.Context) (context.Context, func(), error) {
			if err := m.verifyCreationRoot(ctx, i); err != nil {
				return nil, nil, err
			}
			return ctx, func() {}, nil
		}
		m.controllers[b.ThreadID] = c
	}
	inputErr := c.SubmitInitial(ctx, r.ID)
	scope := store.SubmissionScope{HostID: b.HostID, AppID: i.AppID, ThreadID: b.ThreadID}
	now, err := m.Store.Submission(ctx, scope, r.ID)
	if err != nil {
		return err
	}
	switch now.State {
	case "accepted", "queued", "consumed", "steered":
		if err = m.Store.CompleteCreation(ctx, d.ID); err != nil {
			return err
		}
		if m.Activate != nil {
			m.Activate(b.ThreadID)
		}
	case "unknown":
		return m.Store.SetCreationInputState(ctx, d.ID, "unknown")
	case "failed":
		return m.Store.SetCreationInputState(ctx, d.ID, "rejected")
	}
	return inputErr
}

func (m *Manager) verifyCreationRoot(ctx context.Context, i store.CreationIntent) error {
	if i.FollowMode != "topic" {
		return nil
	}
	b := store.Binding{HostID: i.HostID, ThreadID: i.NativeThreadID, IntentID: i.ID, ChatID: i.TargetChatID, FollowMode: "topic", RootMessageID: i.RootMessageID, PlatformThreadID: i.PlatformThreadID}
	err := messages.VerifyTopicRoot(ctx, m.Gateway, b)
	if errors.Is(err, messages.ErrTopicRootGone) {
		if i.NativeThreadID != "" {
			if _, e := m.Store.Binding(ctx, i.HostID, i.NativeThreadID); e == nil {
				if e = m.Store.MarkBindingRepair(ctx, i.HostID, i.NativeThreadID, "topic_root_unavailable"); e != nil {
					return e
				}
			}
		}
		if e := m.Store.SetCreationError(ctx, i.DraftID, "target-root"); e != nil {
			return e
		}
		return store.ErrTargetRepair
	}
	return err
}

func sameMembers(a, b []string) bool {
	left, right := map[string]bool{}, map[string]bool{}
	for _, id := range a {
		if id != "" {
			left[id] = true
		}
	}
	for _, id := range b {
		if id != "" {
			right[id] = true
		}
	}
	if len(left) != len(right) {
		return false
	}
	for id := range left {
		if !right[id] {
			return false
		}
	}
	return true
}

func remoteOutcome(err error, id string) string {
	if err == nil && id != "" {
		return "confirmed"
	}
	var native *codexclient.RPCError
	var rejected *feishu.Rejected
	var limited *feishu.RateLimited
	if errors.As(err, &native) || errors.As(err, &rejected) || errors.As(err, &limited) {
		return "rejected"
	}
	return "unknown"
}

// Reconcile only exact per-intent source identity. A complete empty scan is
// still inconclusive, including before the first input creates a rollout.
func (m *Manager) reconcileNative(ctx context.Context, n Native, i store.CreationIntent, a store.CreationAttempt) error {
	source, _ := codexclient.CreationThreadSource(i.ID)
	listed, err := n.ListArchive(ctx, false)
	if err != nil {
		return err
	}
	archived, err := n.ListArchive(ctx, true)
	if err != nil {
		return err
	}
	loaded, err := n.LoadedIDs(ctx)
	if err != nil {
		return err
	}
	ids, candidates := map[string]bool{}, map[string]bool{}
	for _, t := range append(listed, archived...) {
		if t.ID != "" {
			candidates[t.ID] = true
		}
	}
	for _, id := range loaded {
		if id != "" {
			candidates[id] = true
		}
	}
	// 0.162.1 thread/list omits threadSource in real responses. Read every
	// candidate's metadata, including unloaded and archived threads. A partial
	// scan cannot prove that the single marker found is actually unique.
	complete := true
	for id := range candidates {
		t, err := n.Metadata(ctx, id)
		if err != nil {
			complete = false
			continue
		}
		if t.ID != id {
			complete = false
			continue
		}
		var label string
		if json.Unmarshal(t.ThreadSource, &label) == nil && label == source {
			ids[id] = true
		}
	}
	if !complete {
		return errors.New("native creation identity scan is incomplete")
	}
	if len(ids) != 1 {
		return nil
	}
	for id := range ids {
		t, err := n.Metadata(ctx, id)
		if err != nil {
			return err
		}
		var label string
		_ = json.Unmarshal(t.ThreadSource, &label)
		if label != source || t.ID != id || t.CWD != i.Directory || t.Ephemeral || t.ParentThreadID != nil || t.ForkedFromID != nil || i.ProjectID != "" && (t.ProjectID == nil || *t.ProjectID != i.ProjectID) {
			return store.ErrConflict
		}
		// Recover effective policies through a minimal resume, without overrides.
		// Missing rollout/policy evidence keeps the original start unknown.
		reader, ok := n.(interface {
			ReadCreation(context.Context, codexclient.CreationSpec, string) (codexclient.CreationResult, error)
		})
		if !ok {
			return nil
		}
		result, err := reader.ReadCreation(ctx, codexclient.CreationSpec{IntentID: i.ID, CWD: i.Directory, ProjectID: i.ProjectID}, id)
		if err != nil {
			return nil
		}
		raw, _ := json.Marshal(map[string]any{"result": result, "evidence": "exact_persistent_source_and_native_policy"})
		return m.Store.FinishCreationAttempt(ctx, a, "confirmed", id, string(raw))
	}
	return nil
}

func (m *Manager) ensureCard(ctx context.Context, draft, step string, p cardRequest, base string) (string, error) {
	a, err := m.Store.CreationAttempt(ctx, draft, step)
	if errors.Is(err, sql.ErrNoRows) {
		p.UUID = store.ID()
		p.Marker = "c" + p.UUID[:19]
		p.Card, _, err = cards.WithRequestMarker(base, p.Marker)
		if err != nil {
			return "", err
		}
		raw, _ := json.Marshal(p)
		a, err = m.Store.PrepareCreationAttempt(ctx, draft, step, string(raw), m.now().UnixMilli())
	}
	if err != nil {
		return "", err
	}
	if json.Unmarshal([]byte(a.Params), &p) != nil {
		return "", store.ErrConflict
	}
	if a.State == "confirmed" {
		return a.RemoteID, nil
	}
	if a.State == "unknown" {
		id, err := messages.FindMarkedCard(ctx, m.Gateway, messages.CardReadbackProof{RootCreation: step == "root", AppID: m.Config.Feishu.AppID, ChatID: p.ChatID, ParentMessageID: p.ParentID, TargetMessageID: p.TargetID, Card: p.Card, Marker: p.Marker, FollowMode: p.FollowMode, RootMessageID: p.RootID, PlatformThreadID: p.ThreadID})
		if err != nil || id == "" {
			return "", err
		}
		return id, m.Store.FinishCreationAttempt(ctx, a, "confirmed", id, "{}")
	}
	if a.State == "rejected" {
		return "", nil
	}
	if p.FollowMode == "topic" {
		if err := messages.VerifyTopicRoot(ctx, m.Gateway, store.Binding{FollowMode: "topic", ChatID: p.ChatID, RootMessageID: p.RootID, PlatformThreadID: p.ThreadID}); err != nil {
			return "", err
		}
	}
	ok, err := m.Store.DispatchCreationAttempt(ctx, a)
	if err != nil || !ok {
		return "", err
	}
	var id string
	var remoteErr error
	switch p.Operation {
	case "send":
		id, remoteErr = m.Gateway.SendCard(ctx, "chat_id", p.ChatID, p.Card, p.UUID)
	case "reply":
		if p.FollowMode == "topic" {
			g, ok := m.Gateway.(interface {
				ReplyTopicCard(context.Context, string, string, string) (string, error)
			})
			if !ok {
				remoteErr = errors.New("topic gateway unavailable")
			} else {
				id, remoteErr = g.ReplyTopicCard(ctx, p.ParentID, p.Card, p.UUID)
			}
		} else {
			id, remoteErr = m.Gateway.ReplyCard(ctx, p.ParentID, p.Card, p.UUID)
		}
	case "update":
		remoteErr = m.Gateway.UpdateCard(ctx, p.TargetID, p.Card)
		id = p.TargetID
	default:
		return "", store.ErrConflict
	}
	outcome := remoteOutcome(remoteErr, id)
	if err = m.Store.FinishCreationAttempt(ctx, a, outcome, id, "{}"); err != nil {
		return "", err
	}
	if outcome != "confirmed" {
		return "", nil
	}
	return id, nil
}

func (m *Manager) resolveEntry(ctx context.Context, draft string) (bool, error) {
	for j := 0; j < 8; j++ {
		var pending string
		err := m.Store.DB.QueryRowContext(ctx, `SELECT step FROM creation_attempts WHERE draft_id=? AND step LIKE 'entry:%' AND state IN ('prepared','dispatching','unknown') ORDER BY rowid LIMIT 1`, draft).Scan(&pending)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		id, err := m.ensureCard(ctx, draft, pending, cardRequest{}, "")
		if err != nil || id == "" {
			return false, err
		}
	}
	return false, nil
}

func entryRequest(d store.CreationDraft) cardRequest {
	p := cardRequest{Operation: "reply", ChatID: d.SourceChatID, ParentID: d.SourceMessageID, Revision: d.Revision}
	if d.SourceRootID != "" {
		p.FollowMode, p.RootID, p.ThreadID = "topic", d.SourceRootID, d.SourceThreadID
	}
	if d.CardID != "" {
		p.Operation, p.TargetID = "update", d.CardID
	}
	return p
}

func busyFallback(host, path, status string) string {
	preview := []rune(path)
	if len(preview) > 200 {
		path = string(preview[:199]) + "…"
	}
	return cards.Creation(cards.CreationView{Busy: status, Host: host, Directory: path})
}

// A busy card is a real journaled write, not a callback PATCH racing the final
// card. Unknown publication blocks the action until exact marker readback.
func (m *Manager) actionBusy(ctx context.Context, a store.CreationAction) (bool, error) {
	d, err := m.Store.CreationDraft(ctx, a.DraftID)
	if err != nil || d.Revision != a.Revision {
		return err == nil, err
	}
	if ready, err := m.resolveEntry(ctx, d.ID); err != nil || !ready {
		return false, err
	}
	path := d.Directory
	if d.View == "directories" {
		path = d.BrowsePath
	}
	busy := "正在处理，请稍候…"
	switch a.Kind {
	case "creation_browse", "creation_panel", "creation_goto":
		busy = "正在打开目录…"
	case "creation_lookup", "creation_filter", "creation_search", "creation_search_next", "creation_projects":
		busy = "正在查找目录…"
	case "creation_pick", "creation_path", "creation_search_pick":
		busy = "正在保存目录选择…"
	case "creation_submit":
		busy = "正在核验创建信息…"
	}
	step := "entry:busy:" + a.ID
	var base string
	if _, err := m.Store.CreationAttempt(ctx, d.ID, step); errors.Is(err, sql.ErrNoRows) {
		var frozen string
		err := m.Store.DB.QueryRowContext(ctx, `SELECT params_json FROM creation_attempts WHERE draft_id=? AND step LIKE 'entry:%' AND step NOT LIKE 'entry:busy:%' AND state='confirmed' ORDER BY rowid DESC LIMIT 1`, d.ID).Scan(&frozen)
		if errors.Is(err, sql.ErrNoRows) {
			base = busyFallback(d.HostID, path, busy)
		} else if err != nil {
			return false, err
		} else {
			var previous cardRequest
			var submitted actionPayload
			if json.Unmarshal([]byte(frozen), &previous) != nil || json.Unmarshal([]byte(a.Payload), &submitted) != nil {
				return false, store.ErrConflict
			}
			base, err = cards.CreationPending(previous.Card, busy, submitted.Form)
			if err != nil {
				// A local snapshot/layout failure is safe to replace before
				// freezing this first request; an existing busy is never rebuilt.
				base = busyFallback(d.HostID, path, busy)
			}
		}
	} else if err != nil {
		return false, err
	}
	id, err := m.ensureCard(ctx, d.ID, step, entryRequest(d), base)
	if err != nil || id != "" {
		return id != "", err
	}
	write, err := m.Store.CreationAttempt(ctx, d.ID, step)
	if err != nil {
		return false, err
	}
	if write.State == "rejected" {
		if err := m.Store.RefreshCreationAction(ctx, a, "feedback"); err != nil {
			return false, err
		}
		return false, m.Store.FinishCreationAction(ctx, a, false)
	}
	return false, nil
}

func (m *Manager) entryCard(ctx context.Context, n Native, d store.CreationDraft) error {
	if ready, err := m.resolveEntry(ctx, d.ID); err != nil || !ready {
		return err
	}
	if d.CardRevision >= d.Revision {
		return nil
	}
	v := cards.CreationView{State: d.State, Error: creationErrorText(d.ErrorCode), Host: d.HostID, Body: d.Body, Directory: d.Directory, ProjectLabel: "独立目录", FollowMode: d.FollowMode, TargetChatID: d.TargetChatID, Token: d.Token, Revision: d.Revision, TopicsEnabled: m.Config.Creation.TopicsEnabled}
	if d.State == "draft" && d.View == "search" {
		read, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		dirs, err := m.selectionDirectories(ctx, n, d.CreatorID)
		if err != nil {
			return err
		}
		v.SearchQuery = d.BrowsePath
		var page DirectoryPage
		if d.BrowsePath == "" {
			page.Choices, err = dirs.Quick(read, 20)
		} else {
			page, err = dirs.Search(read, d.BrowsePath, d.BrowseCursor, 100)
		}
		if err != nil {
			v.Error = creationErrorText(actionError(err))
		}
		v.SearchPage = cardDirectoryPage(page)
	}
	if d.ProjectID != "" {
		v.ProjectLabel = "已核验项目"
	}
	if d.State == "draft" && d.View == "main" {
		m.workgroupOptions(ctx, &v)
	}
	if d.View == "directories" && d.State == "draft" {
		read, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		dirs := m.directories(nil)
		filter, after, err := parseDirectoryCursor(d.BrowseCursor)
		var page DirectoryPage
		if err == nil {
			page, err = dirs.ListMatching(read, d.BrowsePath, filter, after, 100)
		}
		if err != nil {
			v.Error = creationErrorText(actionError(err))
			page = DirectoryPage{Path: d.BrowsePath, ReadOnly: true}
		}
		v.DirectoryPage = cardDirectoryPage(page)
		v.DirectoryPage.Filter = filter
		if err == nil {
			v.DirectoryPage.Breadcrumbs = directoryBreadcrumbs(ctx, dirs, page.Path)
		}
	}
	if d.State == "creating" || d.State == "created" {
		i, err := m.Store.CreationIntent(ctx, d.ID)
		if err != nil {
			return err
		}
		v.State = i.State
		v.ThreadID = i.NativeThreadID
		if d.State == "created" {
			v.State = "created"
			chat := i.ChatID
			if i.FollowMode == "topic" {
				chat = i.TargetChatID
			}
			// The confirmed workflow already verified this target before its
			// native/input writes. Rendering does not recheck current membership.
			if chat != "" && i.RootMessageID != "" {
				v.ChatLink = "https://applink.feishu.cn/client/chat/open?openChatId=" + chat
			}
		}
		if d.ErrorCode == "target-root" {
			v.State = "repair"
		}
	}
	_, err := m.ensureCard(ctx, d.ID, fmt.Sprintf("entry:%d", d.Revision), entryRequest(d), cards.Creation(v))
	return err
}

func (m *Manager) workgroupOptions(ctx context.Context, v *cards.CreationView) {
	if m.workgroupNames == nil {
		m.workgroupNames = map[string]string{}
	}
	lookup, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	for _, chat := range m.Config.Creation.WorkgroupChatIDs {
		label := m.workgroupNames[chat]
		if label == "" {
			if info, err := m.Gateway.GetGroupInfo(lookup, chat); err == nil && info.Name != "" {
				label = info.Name
				m.workgroupNames[chat] = label
			}
		}
		if label == "" {
			label = "工作群（名称暂不可用） " + chat
		}
		v.Workgroups = append(v.Workgroups, cards.CreationOption{Label: label, Value: chat})
	}
}

func creationErrorText(code string) string {
	switch code {
	case "":
		return ""
	case "task-length":
		return fmt.Sprintf("任务正文最多支持 %d 字，请精简后重新发起 /new。", MaxTaskRunes)
	case "directory", "directory-scope":
		return "目录不可用或不在允许范围内，请重新选择目录。"
	case "directory-selection":
		return "请先选用目录，或返回创建卡后再创建。"
	case "feedback":
		return "操作反馈未能更新，任务与原目录已保留，请重新操作。"
	case "directory-limit":
		return "此目录条目较多，请输入更具体的绝对路径。"
	case "directory-read":
		return "目录读取较慢，任务与原目录已保留，请重试或逐层浏览。"
	case "project":
		return "目录的项目归属已变更，请重新选择目录。"
	case "target-membership":
		return "目标群成员或机器人身份无法核验，请检查群设置。"
	case "target-root":
		return "任务根消息已撤回或不可访问，当前位置需要修复。"
	case "validation":
		return "请填写任务、选择可用目录；话题还需要选择已授权工作群。"
	case "input-rejected":
		return "首条输入被明确拒绝，可在原会话中继续提交。"
	case "input-unknown":
		return "首条输入结果正在核验，请勿重复创建。"
	default:
		return "创建步骤正在核验，请查看当前进度。"
	}
	return ""
}
