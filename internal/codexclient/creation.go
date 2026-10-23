package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// CreationSourcePrefix identifies bridge-created sessions, not App provenance.
const CreationSourcePrefix = "feishu.bridge.creation."

var creationIntentID = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

type ProjectRoot struct {
	Path string `json:"path"`
}

// Project follows the local 0.162.1 ProjectListResponse schema. Roots are
// directories on the daemon host; callers must canonicalize and authorize them.
type Project struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Roots     []ProjectRoot     `json:"roots"`
	Metadata  map[string]string `json:"metadata"`
	Position  int64             `json:"position"`
	CreatedAt int64             `json:"createdAt"`
	UpdatedAt int64             `json:"updatedAt"`
	RecencyAt *int64            `json:"recencyAt"`
}

// ListProjects follows every opaque cursor, including empty intermediate pages.
// A failed or malformed page never yields a partial directory selection list.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	params := struct {
		Limit         int    `json:"limit"`
		SortKey       string `json:"sortKey"`
		SortDirection string `json:"sortDirection"`
		Cursor        string `json:"cursor,omitempty"`
	}{Limit: 100, SortKey: "position", SortDirection: "asc"}
	projects := []Project{}
	seen := map[string]bool{}
	for {
		var r struct {
			Data       *[]Project `json:"data"`
			NextCursor *string    `json:"nextCursor"`
		}
		if err := c.Call(ctx, "project/list", params, &r); err != nil {
			return nil, err
		}
		if r.Data == nil {
			return nil, errors.New("invalid native project page")
		}
		projects = append(projects, (*r.Data)...)
		if r.NextCursor == nil || *r.NextCursor == "" {
			break
		}
		if seen[*r.NextCursor] {
			return nil, errors.New("repeated pagination cursor")
		}
		seen[*r.NextCursor] = true
		params.Cursor = *r.NextCursor
	}
	for _, project := range projects {
		if project.ID == "" || project.Roots == nil || project.Metadata == nil {
			return nil, errors.New("invalid native project metadata")
		}
		for _, root := range project.Roots {
			if !creationAbsolutePath(root.Path) {
				return nil, errors.New("invalid native project root")
			}
		}
	}
	return projects, nil
}

// CreationSpec contains only user-confirmed creation identity and location.
// Before freezing this spec, the caller must check the physical directory,
// access, allowed roots, and any project association on the daemon host.
// IntentID is a durable, unique creation intent, never a native deduplication key.
type CreationSpec struct {
	IntentID  string
	CWD       string
	ProjectID string
}

type creationParams struct {
	CWD          string `json:"cwd"`
	ProjectID    string `json:"projectId,omitempty"`
	ThreadSource string `json:"threadSource"`
	Ephemeral    bool   `json:"ephemeral"`
}

// CreationThreadSource is an exact per-intent marker. An absent history match
// does not prove thread/start was rejected, especially before the first input.
func CreationThreadSource(intentID string) (string, error) {
	if !creationIntentID.MatchString(intentID) {
		return "", errors.New("invalid native creation intent")
	}
	return CreationSourcePrefix + intentID, nil
}

func creationAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && strings.IndexFunc(path, unicode.IsControl) < 0
}

func creationRequest(s CreationSpec) (creationParams, error) {
	source, err := CreationThreadSource(s.IntentID)
	if err != nil {
		return creationParams{}, err
	}
	if !creationAbsolutePath(s.CWD) || strings.IndexFunc(s.ProjectID, unicode.IsControl) >= 0 || strings.TrimSpace(s.ProjectID) != s.ProjectID {
		return creationParams{}, errors.New("invalid native creation location")
	}
	return creationParams{CWD: s.CWD, ProjectID: s.ProjectID, ThreadSource: source, Ephemeral: false}, nil
}

// CreationRequestJSON returns the exact params bytes used by StartCreation.
// Persist these in the private journal before dispatch; they include private
// paths. No policy, model, instructions, environment or config overrides exist.
func CreationRequestJSON(s CreationSpec) (string, error) {
	p, err := creationRequest(s)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(p)
	return string(b), err
}

// CreationResult preserves effective native policies for the private journal.
// ApprovalPolicy is the native string/granular union; Sandbox is the native
// tagged union. They are evidence of inherited settings, not caller overrides.
type CreationResult struct {
	Thread                  Thread          `json:"thread"`
	CWD                     string          `json:"cwd"`
	Model                   string          `json:"model"`
	ModelProvider           string          `json:"modelProvider"`
	ApprovalPolicy          json.RawMessage `json:"approvalPolicy"`
	ApprovalsReviewer       string          `json:"approvalsReviewer"`
	Sandbox                 json.RawMessage `json:"sandbox"`
	ActivePermissionProfile json.RawMessage `json:"activePermissionProfile"`
}

// StartCreation sends thread/start exactly once on this existing connection.
// The caller must journal dispatch before calling and must not retry unknown
// results. Keep this connection for the initial queue/add: a new empty thread
// need not have a readable rollout yet. Even validation errors retain decoded
// response evidence, including a returned thread ID, and are always unknown.
// A returned ID does not register a managed worktree. The creation manager
// must durably register and read back its owner before submitting first input.
func (c *Client) StartCreation(ctx context.Context, s CreationSpec) (CreationResult, error) {
	params, err := CreationRequestJSON(s)
	if err != nil {
		return CreationResult{}, err
	}
	var raw json.RawMessage
	if err := c.Call(ctx, "thread/start", json.RawMessage(params), &raw); err != nil {
		return CreationResult{}, err
	}
	return checkedCreationResponse(raw, s, "thread/start", "", true)
}

// ReadCreation verifies one exactly identified creation candidate on this
// existing connection. Resume sends no policy/config/location overrides and
// never creates a replacement. A resume rejection cannot reject the original
// thread/start: every remote verification failure remains UnknownError.
func (c *Client) ReadCreation(ctx context.Context, s CreationSpec, threadID string) (CreationResult, error) {
	if _, err := creationRequest(s); err != nil {
		return CreationResult{}, err
	}
	if threadID == "" || strings.TrimSpace(threadID) != threadID || strings.IndexFunc(threadID, unicode.IsControl) >= 0 {
		return CreationResult{}, errors.New("invalid native creation thread identity")
	}
	params := struct {
		ThreadID     string `json:"threadId"`
		ExcludeTurns bool   `json:"excludeTurns"`
	}{ThreadID: threadID, ExcludeTurns: true}
	var raw json.RawMessage
	if err := c.Call(ctx, "thread/resume", params, &raw); err != nil {
		return CreationResult{}, &UnknownError{Method: "thread/resume", Cause: err}
	}
	return checkedCreationResponse(raw, s, "thread/resume", threadID, false)
}

func checkedCreationResponse(raw json.RawMessage, s CreationSpec, method, expectedID string, birth bool) (CreationResult, error) {
	var r CreationResult
	var shape struct {
		Thread struct {
			Ephemeral     *bool   `json:"ephemeral"`
			ModelProvider string  `json:"modelProvider"`
			ProjectID     *string `json:"projectId"`
		} `json:"thread"`
	}
	invalid := func() (CreationResult, error) {
		return r, &UnknownError{Method: method, Cause: errors.New("native creation response proof is incomplete")}
	}
	if json.Unmarshal(raw, &r) != nil || json.Unmarshal(raw, &shape) != nil {
		return invalid()
	}
	source, _ := CreationThreadSource(s.IntentID)
	var actualSource string
	if r.Thread.ID == "" || expectedID != "" && r.Thread.ID != expectedID || r.Thread.CWD != s.CWD || r.CWD != s.CWD || shape.Thread.Ephemeral == nil || *shape.Thread.Ephemeral || r.Thread.ParentThreadID != nil || r.Thread.ForkedFromID != nil || birth && (r.Thread.Preview != "" || len(r.Thread.Turns) != 0) || json.Unmarshal(r.Thread.ThreadSource, &actualSource) != nil || actualSource != source || r.Model == "" || r.ModelProvider == "" || shape.Thread.ModelProvider != r.ModelProvider {
		return invalid()
	}
	// If no project was selected, the native server may assign one according
	// to its defaults. Only an explicit verified assignment must match exactly.
	if s.ProjectID != "" && (shape.Thread.ProjectID == nil || *shape.Thread.ProjectID != s.ProjectID) {
		return invalid()
	}
	if !validCreationApproval(r.ApprovalPolicy) || !validCreationSandbox(r.Sandbox) || !validCreationProfile(r.ActivePermissionProfile) || (r.ApprovalsReviewer != "user" && r.ApprovalsReviewer != "auto_review" && r.ApprovalsReviewer != "guardian_subagent") {
		return invalid()
	}
	return r, nil
}

func validCreationApproval(raw json.RawMessage) bool {
	var policy string
	if json.Unmarshal(raw, &policy) == nil {
		return policy == "untrusted" || policy == "on-request" || policy == "never"
	}
	var outer map[string]json.RawMessage
	var granular map[string]json.RawMessage
	if json.Unmarshal(raw, &outer) != nil || len(outer) != 1 || json.Unmarshal(outer["granular"], &granular) != nil || granular == nil {
		return false
	}
	for _, key := range []string{"mcp_elicitations", "rules", "sandbox_approval"} {
		var value *bool
		if json.Unmarshal(granular[key], &value) != nil || value == nil {
			return false
		}
	}
	for _, key := range []string{"request_permissions", "skill_approval"} {
		if value, ok := granular[key]; ok {
			var flag *bool
			if json.Unmarshal(value, &flag) != nil || flag == nil {
				return false
			}
		}
	}
	return true
}

func validCreationProfile(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var p struct {
		ID      string  `json:"id"`
		Extends *string `json:"extends"`
	}
	return json.Unmarshal(raw, &p) == nil && p.ID != ""
}

func validCreationSandbox(raw json.RawMessage) bool {
	var p struct {
		Type          string          `json:"type"`
		NetworkAccess json.RawMessage `json:"networkAccess"`
		WritableRoots []string        `json:"writableRoots"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	switch p.Type {
	case "dangerFullAccess":
		return true // Inheritance can legitimately select any supported policy.
	case "readOnly", "workspaceWrite":
		if len(p.NetworkAccess) != 0 {
			var flag *bool
			if json.Unmarshal(p.NetworkAccess, &flag) != nil || flag == nil {
				return false
			}
		}
		for _, root := range p.WritableRoots {
			if !creationAbsolutePath(root) {
				return false
			}
		}
		return true
	case "externalSandbox":
		if len(p.NetworkAccess) == 0 {
			return true
		}
		var access string
		return json.Unmarshal(p.NetworkAccess, &access) == nil && (access == "enabled" || access == "restricted")
	}
	return false
}
