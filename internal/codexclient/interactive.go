package codexclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

var ErrRequestStale = errors.New("native interactive request is no longer actionable")
var ErrInvalidAnswer = errors.New("invalid interactive decision or answer")

// InteractiveRequest is a private, typed snapshot, never an ordinary log value.
// ID is the server's JSON-RPC id, distinct from ItemID and approvalId.
type InteractiveRequest struct {
	GrantRoot                                                                 string
	ID                                                                        json.RawMessage
	Generation                                                                uint64
	Method, ThreadID, TurnID, ItemID, Kind, Reason, CWD, Command, Fingerprint string
	StartedAtMS                                                               int64
	Questions                                                                 []InputQuestion
	Decisions                                                                 []string
	Permissions                                                               json.RawMessage
}
type InputOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}
type InputQuestion struct {
	ID       string        `json:"id"`
	Header   string        `json:"header"`
	Question string        `json:"question"`
	Other    bool          `json:"isOther"`
	Secret   bool          `json:"isSecret"`
	Options  []InputOption `json:"options"`
}
type PermissionChoice struct {
	Value, Label string
	Profile      json.RawMessage
}

func NativeRequestID(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return "s:" + s
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return "n:" + strconv.FormatInt(n, 10)
	}
	return ""
}
func ParseInteractiveRequest(event Event) (InteractiveRequest, error) {
	r := InteractiveRequest{ID: bytes.Clone(event.RequestID), Generation: event.Generation, Method: event.Method}
	if NativeRequestID(r.ID) == "" || r.Generation == 0 {
		return r, ErrInvalidAnswer
	}
	var p struct {
		GrantRoot   string            `json:"grantRoot"`
		ThreadID    string            `json:"threadId"`
		TurnID      string            `json:"turnId"`
		ItemID      string            `json:"itemId"`
		Reason      string            `json:"reason"`
		CWD         string            `json:"cwd"`
		Command     string            `json:"command"`
		Kind        string            `json:"kind"`
		StartedAtMS int64             `json:"startedAtMs"`
		Available   []json.RawMessage `json:"availableDecisions"`
		Permissions json.RawMessage   `json:"permissions"`
		Network     json.RawMessage   `json:"networkApprovalContext"`
		Additional  json.RawMessage   `json:"additionalPermissions"`
		Questions   []InputQuestion   `json:"questions"`
	}
	if json.Unmarshal(event.Params, &p) != nil || p.ThreadID == "" || p.TurnID == "" || p.ItemID == "" {
		return r, ErrInvalidAnswer
	}
	r.GrantRoot = p.GrantRoot
	r.ThreadID, r.TurnID, r.ItemID, r.Reason, r.CWD, r.Command, r.StartedAtMS = p.ThreadID, p.TurnID, p.ItemID, p.Reason, p.CWD, p.Command, p.StartedAtMS
	var canonical any
	if json.Unmarshal(event.Params, &canonical) != nil {
		return r, ErrInvalidAnswer
	}
	b, _ := json.Marshal(canonical)
	sum := sha256.Sum256(b)
	r.Fingerprint = hex.EncodeToString(sum[:])
	switch event.Method {
	case "item/commandExecution/requestApproval":
		r.Kind = "command"
		if p.Kind != "" && p.Kind != "command" {
			r.Kind = "unsupported"
		}
		if r.Kind != "unsupported" && len(p.Network) > 0 && string(p.Network) != "null" {
			r.Kind = "network"
			r.Permissions = bytes.Clone(p.Network)
		} else {
			r.Permissions = bytes.Clone(p.Additional)
		}
		if p.Available == nil {
			r.Decisions = []string{"accept", "decline", "cancel"}
		} else {
			for _, raw := range p.Available {
				var v string
				if json.Unmarshal(raw, &v) == nil && (v == "accept" || v == "decline" || v == "cancel" || v == "acceptForSession") {
					r.Decisions = append(r.Decisions, v)
				}
			}
		}
	case "item/fileChange/requestApproval":
		r.Kind = "file"
		r.Decisions = []string{"accept", "decline", "cancel"}
	case "item/permissions/requestApproval":
		r.Kind = "permissions"
		r.Permissions = bytes.Clone(p.Permissions)
		if _, err := r.PermissionChoices(); err != nil {
			return r, err
		}
	case "item/tool/requestUserInput":
		r.Kind = "question"
		r.Questions = p.Questions
		if len(r.Questions) == 0 || len(r.Questions) > 16 {
			return r, ErrInvalidAnswer
		}
		seen := map[string]bool{}
		for _, q := range r.Questions {
			if q.ID == "" || seen[q.ID] || strings.TrimSpace(q.Question) == "" {
				return r, ErrInvalidAnswer
			}
			seen[q.ID] = true
			if q.Secret {
				r.Kind = "sensitive"
			}
			for _, o := range q.Options {
				if strings.TrimSpace(o.Label) == "" {
					return r, ErrInvalidAnswer
				}
			}
		}
	default:
		return r, ErrInvalidAnswer
	}
	return r, nil
}

func (r InteractiveRequest) PermissionChoices() ([]PermissionChoice, error) {
	var p map[string]json.RawMessage
	if json.Unmarshal(r.Permissions, &p) != nil || len(p) == 0 {
		return nil, ErrInvalidAnswer
	}
	var choices []PermissionChoice
	add := func(label string, profile any) {
		b, _ := json.Marshal(profile)
		choices = append(choices, PermissionChoice{fmt.Sprintf("p_%d", len(choices)), label, b})
	}
	for k := range p {
		if k != "network" && k != "fileSystem" {
			return nil, ErrInvalidAnswer
		}
	}
	if raw := p["network"]; len(raw) > 0 && string(raw) != "null" {
		var n struct {
			Enabled *bool `json:"enabled"`
		}
		if json.Unmarshal(raw, &n) != nil {
			return nil, ErrInvalidAnswer
		}
		if n.Enabled != nil && *n.Enabled {
			add("访问网络", map[string]any{"network": json.RawMessage(raw)})
		}
	}
	if raw := p["fileSystem"]; len(raw) > 0 && string(raw) != "null" {
		var fs map[string]json.RawMessage
		if json.Unmarshal(raw, &fs) != nil {
			return nil, ErrInvalidAnswer
		}
		for k, arr := range fs {
			if k != "read" && k != "write" && k != "entries" && k != "globScanMaxDepth" {
				return nil, ErrInvalidAnswer
			}
			if k == "globScanMaxDepth" {
				continue
			}
			var entries []json.RawMessage
			if string(arr) == "null" {
				continue
			}
			if json.Unmarshal(arr, &entries) != nil {
				return nil, ErrInvalidAnswer
			}
			for _, entry := range entries {
				if k == "entries" {
					var rule struct {
						Access string          `json:"access"`
						Path   json.RawMessage `json:"path"`
					}
					if json.Unmarshal(entry, &rule) != nil || !validPermissionPath(rule.Path) || (rule.Access != "read" && rule.Access != "write" && rule.Access != "deny") {
						return nil, ErrInvalidAnswer
					}
					if rule.Access == "deny" {
						continue
					}
				}
				label := string(entry)
				var path string
				if k != "entries" {
					if json.Unmarshal(entry, &path) != nil || !filepath.IsAbs(path) {
						return nil, ErrInvalidAnswer
					}
					label = path
				}
				if k == "entries" {
					var rule struct {
						Access string          `json:"access"`
						Path   json.RawMessage `json:"path"`
					}
					json.Unmarshal(entry, &rule)
					label = map[string]string{"read": "读取 ", "write": "写入 "}[rule.Access] + describePermissionPath(rule.Path)
				}
				prefix := map[string]string{"read": "读取：", "write": "写入：", "entries": "文件权限："}[k]
				profile := map[string]any{k: []json.RawMessage{entry}}
				if d := fs["globScanMaxDepth"]; len(d) > 0 {
					profile["globScanMaxDepth"] = d
				}
				add(prefix+label, map[string]any{"fileSystem": profile})
			}
		}
	}
	// Map iteration cannot define native scope identities. Canonicalize labels and
	// profiles before assigning values so replay and callback share one mapping.
	for i := 1; i < len(choices); i++ {
		for j := i; j > 0 && bytes.Compare(choices[j].Profile, choices[j-1].Profile) < 0; j-- {
			choices[j], choices[j-1] = choices[j-1], choices[j]
		}
	}
	for i := range choices {
		choices[i].Value = fmt.Sprintf("p_%d", i)
	}
	if len(choices) == 0 || len(choices) > 64 {
		return nil, ErrInvalidAnswer
	}
	return choices, nil
}

// BuildInteractiveResult validates every field before any callback is consumed.
// Form names and choice values are bridge-owned indices, not trusted native IDs.
func BuildInteractiveResult(r InteractiveRequest, decision string, form map[string]any) (json.RawMessage, error) {
	var result any
	switch r.Kind {
	case "command", "file", "network":
		allowed := false
		for _, v := range r.Decisions {
			allowed = allowed || v == decision
		}
		if !allowed || len(form) > 0 {
			return nil, ErrInvalidAnswer
		}
		result = map[string]string{"decision": decision}
	case "permissions":
		choices, err := r.PermissionChoices()
		if err != nil {
			return nil, err
		}
		// Form-local allow-all/decline buttons can carry the optional current
		// selection too. Validate that field, then use the explicit decision;
		// a selected value never supplies or expands the native grant profile.
		if decision != "" && len(form) > 0 {
			_, hasSelection := form["permissions"]
			if len(form) != 1 || !hasSelection {
				return nil, ErrInvalidAnswer
			}
			var selected []any
			switch value := form["permissions"].(type) {
			case nil:
			case []any:
				selected = value
			case []string:
				for _, s := range value {
					selected = append(selected, s)
				}
			default:
				return nil, ErrInvalidAnswer
			}
			seen := map[string]bool{}
			for _, value := range selected {
				s, ok := value.(string)
				if !ok || seen[s] {
					return nil, ErrInvalidAnswer
				}
				found := false
				for _, choice := range choices {
					found = found || choice.Value == s
				}
				if !found {
					return nil, ErrInvalidAnswer
				}
				seen[s] = true
			}
			form = nil
		}
		profile := map[string]any{}
		switch decision {
		case "accept":
			if len(form) > 0 {
				return nil, ErrInvalidAnswer
			}
			json.Unmarshal(r.Permissions, &profile)
		case "decline":
			if len(form) > 0 {
				return nil, ErrInvalidAnswer
			}
		case "":
			if len(form) != 1 {
				return nil, ErrInvalidAnswer
			}
			var selected []string
			switch v := form["permissions"].(type) {
			case []any:
				for _, n := range v {
					s, ok := n.(string)
					if !ok {
						return nil, ErrInvalidAnswer
					}
					selected = append(selected, s)
				}
			case []string:
				selected = v
			default:
				return nil, ErrInvalidAnswer
			}
			if len(selected) == 0 {
				return nil, ErrInvalidAnswer
			}
			seen := map[string]bool{}
			for _, key := range selected {
				if seen[key] {
					return nil, ErrInvalidAnswer
				}
				seen[key] = true
				found := false
				for _, c := range choices {
					if c.Value != key {
						continue
					}
					found = true
					var part map[string]any
					json.Unmarshal(c.Profile, &part)
					for name, val := range part {
						if name == "network" {
							profile[name] = val
							continue
						}
						dst, _ := profile[name].(map[string]any)
						if dst == nil {
							dst = map[string]any{}
						}
						for field, entry := range val.(map[string]any) {
							if field == "globScanMaxDepth" {
								dst[field] = entry
							} else {
								old, _ := dst[field].([]any)
								dst[field] = append(old, entry.([]any)...)
							}
						}
						profile[name] = dst
					}
				}
				if !found {
					return nil, ErrInvalidAnswer
				}
			}
		default:
			return nil, ErrInvalidAnswer
		}
		if fs, ok := profile["fileSystem"].(map[string]any); ok && decision == "" {
			entries, _ := fs["entries"].([]any)
			for _, deny := range permissionDenies(r.Permissions) {
				var rule any
				json.Unmarshal(deny, &rule)
				entries = append(entries, rule)
			}
			if len(entries) > 0 {
				fs["entries"] = entries
			}
		}
		result = map[string]any{"permissions": profile, "scope": "turn"}
	case "question":
		if decision != "" {
			return nil, ErrInvalidAnswer
		}
		answers := map[string]any{}
		validFields := map[string]bool{}
		for i, q := range r.Questions {
			if q.Secret {
				return nil, ErrInvalidAnswer
			}
			field := fmt.Sprintf("q_%d", i)
			validFields[field] = true
			validFields[field+"_notes"] = true
			var values []string
			if v, ok := form[field]; ok && v != nil && v != "" {
				s, ok := v.(string)
				if !ok {
					return nil, ErrInvalidAnswer
				}
				n, err := strconv.Atoi(s)
				if err != nil || strconv.Itoa(n) != s || n < 0 || n >= len(q.Options) {
					return nil, ErrInvalidAnswer
				}
				values = append(values, q.Options[n].Label)
			}
			if v, ok := form[field+"_notes"]; ok && v != nil {
				s, ok := v.(string)
				if !ok || !utf8.ValidString(s) || utf8.RuneCountInString(s) > 1000 {
					return nil, ErrInvalidAnswer
				}
				if strings.TrimSpace(s) != "" {
					values = append(values, s)
				}
			}
			if len(values) == 0 {
				return nil, ErrInvalidAnswer
			}
			answers[q.ID] = map[string]any{"answers": values}
		}
		for field := range form {
			if !validFields[field] {
				return nil, ErrInvalidAnswer
			}
		}
		result = map[string]any{"answers": answers}
	default:
		return nil, ErrInvalidAnswer
	}
	b, err := json.Marshal(result)
	return b, err
}

// RespondServerRequest sends exactly once and does not wait for an RPC reply:
// server responses have no acknowledgement. resolved proves closure, not which
// client decision won. Both native identity and payload must still match.
func (c *Client) RespondServerRequest(ctx context.Context, r InteractiveRequest, result json.RawMessage) error {
	check := func(consume bool) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		current, ok := c.serverRequests[NativeRequestID(r.ID)]
		if ctx.Err() != nil || c.terminal != nil || !ok || c.serverTerminal[serverTurnKey(r.ThreadID, r.TurnID)] || r.Generation != c.generation || current.Fingerprint != r.Fingerprint || current.ThreadID != r.ThreadID || current.TurnID != r.TurnID || current.Method != r.Method {
			return ErrRequestStale
		}
		if !validInteractiveResult(current, result) {
			return ErrInvalidAnswer
		}
		if consume {
			c.serverAnswered[NativeRequestID(r.ID)] = r.Fingerprint
			delete(c.serverRequests, NativeRequestID(r.ID))
		}
		return nil
	}
	if err := check(false); err != nil {
		return err
	}
	b, err := json.Marshal(map[string]any{"id": r.ID, "result": result})
	if err != nil {
		return ErrInvalidAnswer
	}
	began := false
	err = c.writeMessage(ctx, b, func() error {
		if err := check(true); err != nil {
			return err
		}
		began = true
		return nil
	})
	if err != nil {
		if !began && (errors.Is(err, ErrRequestStale) || errors.Is(err, ErrInvalidAnswer)) {
			return err
		}
		return &UnknownError{r.Method, err}
	}
	return nil
}

func validInteractiveResult(r InteractiveRequest, b json.RawMessage) bool {
	// Reconstruct the safe result using only known choices rather than allowing
	// arbitrary JSON-RPC payloads from a caller to consume the native callback.
	var p map[string]json.RawMessage
	if json.Unmarshal(b, &p) != nil {
		return false
	}
	switch r.Kind {
	case "command", "file", "network":
		var d string
		if len(p) != 1 || json.Unmarshal(p["decision"], &d) != nil {
			return false
		}
		_, err := BuildInteractiveResult(r, d, nil)
		return err == nil
	case "question":
		if len(p) != 1 {
			return false
		}
		var answers map[string]struct {
			Answers []string `json:"answers"`
		}
		if json.Unmarshal(p["answers"], &answers) != nil || len(answers) != len(r.Questions) {
			return false
		}
		for _, q := range r.Questions {
			a, ok := answers[q.ID]
			if q.Secret || !ok || len(a.Answers) == 0 || len(a.Answers) > 2 {
				return false
			}
			for _, v := range a.Answers {
				isLabel := false
				for _, o := range q.Options {
					isLabel = isLabel || o.Label == v
				}
				if strings.TrimSpace(v) == "" || !utf8.ValidString(v) || (!isLabel && utf8.RuneCountInString(v) > 1000) {
					return false
				}
			}
		}
		return true
	case "permissions":
		if len(p) != 2 || string(p["scope"]) != `"turn"` {
			return false
		}
		var granted map[string]json.RawMessage
		if json.Unmarshal(p["permissions"], &granted) != nil || granted == nil {
			return false
		}
		if len(granted) == 0 {
			return true
		}
		// Every granted leaf must be an unchanged member of the requested profile.
		var requested map[string]json.RawMessage
		json.Unmarshal(r.Permissions, &requested)
		if fsRaw, ok := granted["fileSystem"]; ok && string(bytes.TrimSpace(fsRaw)) != "null" {
			var fs map[string]json.RawMessage
			if json.Unmarshal(fsRaw, &fs) != nil || fs == nil {
				return false
			}
			var entries []json.RawMessage
			json.Unmarshal(fs["entries"], &entries)
			for _, deny := range permissionDenies(r.Permissions) {
				found := false
				for _, entry := range entries {
					found = found || jsonEquivalent(deny, entry)
				}
				if !found {
					return false
				}
			}
		}
		for key, raw := range granted {
			if key != "fileSystem" && key != "network" {
				return false
			}
			if string(bytes.TrimSpace(raw)) == "null" {
				continue
			}
			var a, orig map[string]json.RawMessage
			if json.Unmarshal(raw, &a) != nil || json.Unmarshal(requested[key], &orig) != nil {
				return false
			}
			for field, v := range a {
				if field == "read" || field == "write" || field == "entries" {
					var items, allowed []json.RawMessage
					if json.Unmarshal(v, &items) != nil || json.Unmarshal(orig[field], &allowed) != nil {
						return false
					}
					for _, item := range items {
						found := false
						for _, x := range allowed {
							found = found || jsonEquivalent(item, x)
						}
						if !found {
							return false
						}
					}
				} else if !jsonEquivalent(v, orig[field]) {
					return false
				}
			}
		}
		return true
	}
	return false
}
func jsonEquivalent(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	aa, _ := json.Marshal(x)
	bb, _ := json.Marshal(y)
	return bytes.Equal(aa, bb)
}

func permissionDenies(raw json.RawMessage) []json.RawMessage {
	var profile struct {
		FileSystem struct {
			Entries []json.RawMessage `json:"entries"`
		} `json:"fileSystem"`
	}
	if json.Unmarshal(raw, &profile) != nil {
		return nil
	}
	var denies []json.RawMessage
	for _, entry := range profile.FileSystem.Entries {
		var rule struct {
			Access string `json:"access"`
		}
		if json.Unmarshal(entry, &rule) == nil && rule.Access == "deny" {
			denies = append(denies, entry)
		}
	}
	return denies
}

func validPermissionPath(raw json.RawMessage) bool {
	var p struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Value   struct {
			Kind    string  `json:"kind"`
			Subpath *string `json:"subpath"`
		} `json:"value"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	switch p.Type {
	case "path":
		return filepath.IsAbs(p.Path)
	case "glob_pattern":
		return p.Pattern != ""
	case "special":
		switch p.Value.Kind {
		case "root", "minimal", "project_roots", "tmpdir", "slash_tmp":
			return true
		}
	}
	return false
}
func describePermissionPath(raw json.RawMessage) string {
	var p struct {
		Type, Path, Pattern string
		Value               struct {
			Kind    string
			Subpath *string
		}
	}
	json.Unmarshal(raw, &p)
	switch p.Type {
	case "path":
		return p.Path
	case "glob_pattern":
		return "匹配 " + p.Pattern
	case "special":
		label := map[string]string{"root": "整个文件系统", "minimal": "最小文件范围", "project_roots": "项目根目录", "tmpdir": "临时目录", "slash_tmp": "/tmp"}[p.Value.Kind]
		if p.Value.Subpath != nil {
			label += " / " + *p.Value.Subpath
		}
		return label
	}
	return string(raw)
}
