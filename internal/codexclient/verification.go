package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
)

const verificationSource = "feishu.bridge.validation"

var verificationRunID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// LocalVerificationSpec is only for an explicitly authorized, disposable
// local-model probe. The managed CLI has no endpoint accepting this struct.
// It cannot select a production provider, model, prompt or policy override.
type LocalVerificationSpec struct{ RunID, CWD, ModelURL string }

// LocalVerificationRequestJSON freezes the same validated wire params used
// by StartLocalVerification. It contains private paths and a loopback URL;
// callers must keep it in the private journal, not normal diagnostic output.
func LocalVerificationRequestJSON(s LocalVerificationSpec) (string, error) {
	params, err := localVerificationParams(s)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(params)
	return string(b), err
}

func localVerificationParams(s LocalVerificationSpec) (map[string]any, error) {
	if !verificationRunID.MatchString(s.RunID) || !filepath.IsAbs(s.CWD) || filepath.Clean(s.CWD) != s.CWD {
		return nil, errors.New("invalid local verification scope")
	}
	physical, err := filepath.EvalSymlinks(s.CWD)
	if err != nil || physical != s.CWD {
		return nil, errors.New("verification directory is not physical")
	}
	info, err := os.Lstat(s.CWD)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("verification directory must be private")
	}
	u, err := url.Parse(s.ModelURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.EscapedPath() != "/v1" || u.Hostname() == "" {
		return nil, errors.New("verification model must be local")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return nil, errors.New("verification model must be loopback with an explicit port")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("verification directory belongs to another user")
	}
	provider := "cfb_verify_" + s.RunID
	return map[string]any{"cwd": s.CWD, "model": "fixture-model", "modelProvider": provider, "approvalPolicy": "never", "sandbox": "read-only", "threadSource": verificationSource, "ephemeral": false, "config": map[string]any{"model_providers." + provider: map[string]any{"name": "Local verification fixture", "base_url": s.ModelURL, "wire_api": "responses", "requires_openai_auth": false, "supports_websockets": false, "request_max_retries": 0}, "model_reasoning_effort": "low"}}, nil
}

// StartLocalVerification performs exactly one thread/start on this existing
// connection. Callers must journal its frozen intent BEFORE calling it and
// must not blindly retry an UnknownError. It does not start a turn, fork,
// alter global config, connect another client or prove App UI provenance.
func (c *Client) StartLocalVerification(ctx context.Context, s LocalVerificationSpec) (Thread, error) {
	params, err := localVerificationParams(s)
	if err != nil {
		return Thread{}, err
	}
	var r struct {
		Thread         json.RawMessage `json:"thread"`
		CWD            string          `json:"cwd"`
		Model          string          `json:"model"`
		ModelProvider  string          `json:"modelProvider"`
		ApprovalPolicy string          `json:"approvalPolicy"`
		Sandbox        struct {
			Type          string `json:"type"`
			NetworkAccess bool   `json:"networkAccess"`
		} `json:"sandbox"`
	}
	if err = c.Call(ctx, "thread/start", params, &r); err != nil {
		return Thread{}, err
	}
	var t Thread
	var shape struct {
		Ephemeral     *bool  `json:"ephemeral"`
		ModelProvider string `json:"modelProvider"`
	}
	invalid := func() (Thread, error) {
		return Thread{}, &UnknownError{Method: "thread/start", Cause: errors.New("local verification creation proof is incomplete")}
	}
	if json.Unmarshal(r.Thread, &t) != nil || json.Unmarshal(r.Thread, &shape) != nil || shape.Ephemeral == nil || *shape.Ephemeral || t.ID == "" || t.CWD != s.CWD || r.CWD != s.CWD || r.Model != "fixture-model" || r.ModelProvider != params["modelProvider"] || shape.ModelProvider != r.ModelProvider || r.ApprovalPolicy != "never" || r.Sandbox.Type != "readOnly" || r.Sandbox.NetworkAccess || t.ParentThreadID != nil || t.ForkedFromID != nil || t.Preview != "" || len(t.Turns) != 0 {
		return invalid()
	}
	var label string
	if json.Unmarshal(t.ThreadSource, &label) != nil || label != verificationSource {
		return invalid()
	}
	return t, nil
}
