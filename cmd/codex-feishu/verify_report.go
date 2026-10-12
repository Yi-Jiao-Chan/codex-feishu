package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"codex-feishu/compat"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/control"
	"codex-feishu/internal/store"
)

type verificationArtifact struct{ Commit, SHA256 string }

// This is an explicit case report, not an automatic UI observer or permission
// to run probes. A reviewer remains responsible for the meaning of its evidence.
type acceptanceReport struct {
	SchemaVersion         int              `json:"schema_version"`
	Template              bool             `json:"template"`
	Phase                 string           `json:"phase"`
	ArtifactCommit        string           `json:"artifact_commit"`
	BinarySHA256          string           `json:"binary_sha256"`
	BridgeVersion         string           `json:"bridge_version"`
	CodexCLIVersion       string           `json:"codex_cli_version"`
	CodexAppServerVersion string           `json:"codex_app_server_version"`
	ProtocolSchemaDigest  string           `json:"protocol_schema_digest"`
	FeishuSDKVersion      string           `json:"feishu_sdk_version"`
	HostID                string           `json:"host_id"`
	AppID                 string           `json:"app_id"`
	OwnerID               string           `json:"owner_open_id"`
	ConfigurationIdentity string           `json:"configuration_identity"`
	DatabaseSchema        int              `json:"database_schema"`
	Cases                 []acceptanceCase `json:"cases"`
	RequiredCaseIDs       []string         `json:"required_case_ids"`
	ReadyToRelease        bool             `json:"ready_to_release"`
}

type acceptanceCase struct {
	ID             string               `json:"id"`
	Status         string               `json:"status"`
	Evidence       []acceptanceEvidence `json:"evidence"`
	BlockingReason string               `json:"blocking_reason"`
}

type acceptanceEvidence struct {
	Kind       string `json:"kind"` // automated or manual; never inferred from an image or a log.
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	ObservedBy string `json:"observed_by,omitempty"` // manual evidence must name the configured owner.
}

var phase1CaseIDs = []string{"P1-01", "P1-02", "P1-03", "P1-04", "P1-05", "P1-06", "P1-07", "P1-08", "P1-09", "P1-10", "P1-11", "P1-12"}

func reportDigest(s string, n int) bool {
	if len(s) != n || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// Do not expose underlying path errors or file contents. O_NOFOLLOW plus fd
// checks keeps report/evidence reads on the private regular file actually opened.
func readPrivateEvidence(ctx context.Context, path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\n\r") || ctx.Err() != nil {
		return nil, errors.New("invalid evidence input")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("evidence unavailable")
	}
	f := os.NewFile(uintptr(fd), "private evidence")
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, errors.New("evidence unavailable")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > limit {
		return nil, errors.New("evidence must be a private regular file")
	}
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(body)) > limit || ctx.Err() != nil {
		return nil, errors.New("evidence unreadable")
	}
	return body, nil
}

func readAcceptanceReport(ctx context.Context, path string) (*acceptanceReport, error) {
	body, err := readPrivateEvidence(ctx, path, 1<<20)
	if err != nil {
		return nil, err
	}
	var r acceptanceReport
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF || r.SchemaVersion != 1 || r.Template || r.Phase != "phase1" || !reportDigest(r.ArtifactCommit, 40) || !reportDigest(r.BinarySHA256, 64) || !reportDigest(r.ConfigurationIdentity, 64) || !reportDigest(r.ProtocolSchemaDigest, 64) || r.BridgeVersion == "" || r.CodexCLIVersion == "" || r.CodexAppServerVersion == "" || r.FeishuSDKVersion == "" || !config.ValidAppID(r.AppID) || !config.ValidOpenID(r.OwnerID) || r.HostID == "" || r.DatabaseSchema != store.SchemaVersion || len(r.Cases) != 12 || len(r.RequiredCaseIDs) != 12 {
		return nil, errors.New("invalid case report")
	}
	want := make(map[string]bool, len(phase1CaseIDs))
	for _, id := range phase1CaseIDs {
		want[id] = true
	}
	seen := map[string]bool{}
	for _, id := range r.RequiredCaseIDs {
		if !want[id] || seen[id] {
			return nil, errors.New("invalid required cases")
		}
		seen[id] = true
	}
	seen = map[string]bool{}
	for _, c := range r.Cases {
		if !want[c.ID] || seen[c.ID] || c.Status != "pass" && c.Status != "fail" && c.Status != "pending" && c.Status != "skipped" || len(c.Evidence) > 32 || c.Status == "pass" && len(c.Evidence) == 0 {
			return nil, errors.New("invalid case evidence")
		}
		seen[c.ID] = true
		for _, e := range c.Evidence {
			if e.Kind != "manual" && e.Kind != "automated" || !filepath.IsAbs(e.Path) || !reportDigest(e.SHA256, 64) || e.Kind == "manual" && e.ObservedBy != r.OwnerID || e.Kind == "automated" && e.ObservedBy != "" {
				return nil, errors.New("invalid evidence reference")
			}
		}
	}
	return &r, nil
}

func evaluateAcceptanceReport(ctx context.Context, cfg config.Config, identity string, live control.Snapshot, managed bool, data map[string]any, o output, diagnosticCode int, r *acceptanceReport, artifact func() (verificationArtifact, error)) (output, int) {
	data["verification_scope"] = "phase1-case-report-and-current-deployment"
	data["ui_evidence_source"] = "explicit-manual-report"
	o.State = "acceptance-report-pending"
	o.NextSteps = []string{"reported evidence does not perform or independently observe UI tests; complete missing cases and retain real evidence; release status is unchanged"}
	if diagnosticCode != 0 {
		return o, diagnosticCode // Never let a report override current health/permission failures.
	}
	if !managed || artifact == nil {
		o.Checks = append(o.Checks, check{"verification.report_build", "pending", true, "matching live build metadata is required; snapshots and old providers cannot prove it"})
		return o, 5
	}
	a, err := artifact()
	if err != nil || a.Commit == "" || a.SHA256 == "" || live.BridgeCommit == "" || live.BinarySHA256 == "" {
		o.Checks = append(o.Checks, check{"verification.report_build", "pending", true, "current CLI and running bridge artifact identity unavailable"})
		return o, 5
	}
	daemon, ok := data["codex_daemon"].(codexclient.Daemon)
	if !ok || daemon.CLIVersion == "" {
		o.Checks = append(o.Checks, check{"verification.report_environment", "pending", true, "actual daemon CLI version unavailable; unknown metadata cannot match a report"})
		return o, 5
	}
	if r.HostID != cfg.Host.ID || r.AppID != cfg.Feishu.AppID || r.OwnerID != cfg.Access.OwnerOpenID || identity == "" || r.ConfigurationIdentity != identity || r.ArtifactCommit != a.Commit || r.BinarySHA256 != a.SHA256 || live.BridgeCommit != a.Commit || live.BinarySHA256 != a.SHA256 || live.Persistent.Schema != r.DatabaseSchema || r.BridgeVersion != bridgeVersion || r.CodexCLIVersion != daemon.CLIVersion || r.CodexAppServerVersion != daemon.AppServerVersion || r.ProtocolSchemaDigest != compat.ProtocolSchemaDigest || r.FeishuSDKVersion != compat.FeishuManifest().SDKVersion {
		o.State = "acceptance-report-conflict"
		o.Checks = append(o.Checks, check{"verification.report_identity", "fail", true, "case report does not match the current CLI, running build, configuration or environment; previous-build/M0 evidence cannot substitute"})
		return o, 6
	}
	o.Checks = append(o.Checks, check{"verification.report_identity", "pass", true, "exact CLI, running build, configuration and daemon versions matched; protocol digest matches the pinned compatibility corpus, not a live schema probe"})
	rows := make(map[string]acceptanceCase, len(r.Cases))
	for _, c := range r.Cases {
		rows[c.ID] = c
	}
	all, manualUI := true, true
	for i := range o.Checks {
		id := strings.TrimPrefix(o.Checks[i].ID, "acceptance.")
		c, found := rows[id]
		if !found {
			continue
		}
		status, manual := c.Status, false
		valid := true
		for _, e := range c.Evidence {
			body, err := readPrivateEvidence(ctx, e.Path, 32<<20)
			h := sha256.Sum256(body)
			if err != nil || hex.EncodeToString(h[:]) != e.SHA256 {
				valid = false
			}
			manual = manual || e.Kind == "manual"
		}
		// These cases explicitly require App/Feishu, setup-agent or lifecycle
		// observation; counters and automated mocks alone cannot pass them.
		needsManual := id != "P1-09"
		if !valid {
			status = "pending"
		} else if status == "pass" && needsManual && !manual {
			status = "pending"
		}
		o.Checks[i].Status = status
		o.Checks[i].Detail = map[string]any{"source": "explicit-case-report", "evidence_files_checked": valid, "manual_confirmation_recorded": manual, "independently_observed_ui": false}
		all = all && status == "pass"
		if needsManual {
			manualUI = manualUI && status == "pass" && manual && valid
		}
	}
	for i := range o.Checks {
		if o.Checks[i].ID == "verification.live_ui" && manualUI {
			o.Checks[i].Status = "pass"
			o.Checks[i].Detail = "owner manual confirmations explicitly recorded in matched case evidence; CLI did not independently observe the UI"
		}
	}
	data["recorded_cases_all_passed"] = all
	data["manual_ui_confirmations_recorded"] = manualUI
	if !all {
		return o, 5
	}
	o.OK, o.State = true, "acceptance-report-validated"
	o.NextSteps = []string{"current prerequisites and recorded case evidence validated; retain manual evidence; this command does not publish a release or change phase1_released"}
	return o, 0
}
