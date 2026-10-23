package codexclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

// SettingsPatch is a partial explicit change. No unrelated thread settings are sent.
type SettingsPatch struct {
	Model           *string
	ReasoningEffort *string
	ServiceTier     *ServiceTierChange
}

// ServiceTierChange distinguishes omission from clearing the requested tier.
// A nil Value clears it; this may inherit a catalog default, including priority.
type ServiceTierChange struct{ Value *string }

var ErrInvalidSettings = errors.New("invalid native settings change")
var ErrSettingsUnavailable = errors.New("native settings unavailable")

func settingValue(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}

func settingsParams(threadID string, patch SettingsPatch) (map[string]any, error) {
	if !settingValue(threadID) || patch.Model == nil && patch.ReasoningEffort == nil && patch.ServiceTier == nil {
		return nil, ErrInvalidSettings
	}
	p := map[string]any{"threadId": threadID}
	for key, value := range map[string]*string{"model": patch.Model, "effort": patch.ReasoningEffort} {
		if value != nil {
			if !settingValue(*value) {
				return nil, ErrInvalidSettings
			}
			p[key] = *value
		}
	}
	if patch.ServiceTier != nil {
		if patch.ServiceTier.Value == nil {
			p["serviceTier"] = nil
		} else {
			if !settingValue(*patch.ServiceTier.Value) {
				return nil, ErrInvalidSettings
			}
			p["serviceTier"] = *patch.ServiceTier.Value
		}
	}
	return p, nil
}

// SettingsRequestJSON freezes the exact thread/settings/update params so the
// caller can persist the request before changing remote state.
func SettingsRequestJSON(threadID string, patch SettingsPatch) (string, error) {
	p, err := settingsParams(threadID, patch)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(p)
	return string(b), err
}

// UpdateThreadSettings changes defaults for subsequent turns only. It sends
// once; RPCError is rejected, UnknownError requires reconciliation, and nil
// confirms acceptance, not any particular inference using the requested values.
func (c *Client) UpdateThreadSettings(ctx context.Context, threadID string, patch SettingsPatch) error {
	p, err := settingsParams(threadID, patch)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err = c.Call(ctx, "thread/settings/update", p, &raw); err != nil {
		return err
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil || response == nil {
		return &UnknownError{"thread/settings/update", errors.New("invalid settings update response")}
	}
	return nil
}

// ThreadSettings reports configured thread defaults, never per-turn execution
// telemetry. Null/absent reasoning effort means unset or unavailable upstream.
// ServiceTierKnown distinguishes a present null requested tier from no evidence.
type ThreadSettings struct {
	ThreadID         string
	Model            string
	ModelProvider    string
	ReasoningEffort  *string
	ServiceTier      *string
	ServiceTierKnown bool
}

func decodeThreadSettings(raw json.RawMessage, threadID, effortKey string) (ThreadSettings, error) {
	r := ThreadSettings{ThreadID: threadID}
	var p map[string]json.RawMessage
	if json.Unmarshal(raw, &p) != nil || p == nil || json.Unmarshal(p["model"], &r.Model) != nil || !settingValue(r.Model) || json.Unmarshal(p["modelProvider"], &r.ModelProvider) != nil || !settingValue(r.ModelProvider) {
		return ThreadSettings{}, ErrSettingsUnavailable
	}
	for key, dest := range map[string]**string{effortKey: &r.ReasoningEffort, "serviceTier": &r.ServiceTier} {
		if value, ok := p[key]; ok {
			if json.Unmarshal(value, dest) != nil || *dest != nil && !settingValue(**dest) {
				return ThreadSettings{}, ErrSettingsUnavailable
			}
			if key == "serviceTier" {
				r.ServiceTierKnown = true
			}
		}
	}
	return r, nil
}

// ReadThreadSettings uses the same observation-only resume as Attach, without
// configuration overrides. thread/read omits service tier in 0.162.1. There is
// no native read API for a live turn's settings in that protocol version.
func (c *Client) ReadThreadSettings(ctx context.Context, threadID string) (ThreadSettings, error) {
	if !settingValue(threadID) {
		return ThreadSettings{}, ErrInvalidSettings
	}
	var raw json.RawMessage
	if err := c.Call(ctx, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, &raw); err != nil {
		return ThreadSettings{}, err
	}
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Thread.ID != threadID {
		return ThreadSettings{}, ErrSettingsUnavailable
	}
	return decodeThreadSettings(raw, threadID, "reasoningEffort")
}

// ParseThreadSettingsUpdated reads the native thread/settings/updated snapshot.
// It does not infer settings for any currently running turn.
func ParseThreadSettingsUpdated(event Event) (ThreadSettings, error) {
	if event.Method != "thread/settings/updated" {
		return ThreadSettings{}, ErrSettingsUnavailable
	}
	var p struct {
		ThreadID string          `json:"threadId"`
		Settings json.RawMessage `json:"threadSettings"`
	}
	if json.Unmarshal(event.Params, &p) != nil || !settingValue(p.ThreadID) {
		return ThreadSettings{}, ErrSettingsUnavailable
	}
	return decodeThreadSettings(p.Settings, p.ThreadID, "effort")
}

type ReasoningEffortOption struct {
	ReasoningEffort string `json:"reasoningEffort"`
	Description     string `json:"description"`
}
type ModelServiceTier struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Model follows model/list in the installed 0.162.1 experimental schema.
// Model is the RPC model value; ID identifies the catalog entry.
type Model struct {
	ID                        string                  `json:"id"`
	Model                     string                  `json:"model"`
	DisplayName               string                  `json:"displayName"`
	Description               string                  `json:"description"`
	Hidden                    bool                    `json:"hidden"`
	IsDefault                 bool                    `json:"isDefault"`
	DefaultReasoningEffort    string                  `json:"defaultReasoningEffort"`
	SupportedReasoningEfforts []ReasoningEffortOption `json:"supportedReasoningEfforts"`
	ServiceTiers              []ModelServiceTier      `json:"serviceTiers"`
	DefaultServiceTier        *string                 `json:"defaultServiceTier"`
}

func (m Model) SupportsReasoningEffort(effort string) bool {
	for _, option := range m.SupportedReasoningEfforts {
		if option.ReasoningEffort == effort {
			return true
		}
	}
	return false
}
func (m Model) SupportsServiceTier(tier string) bool {
	for _, option := range m.ServiceTiers {
		if option.ID == tier {
			return true
		}
	}
	return false
}

// ListModels follows all cursors and rejects incomplete pages rather than
// providing a partial catalog for validating user selections.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	p := map[string]any{"limit": 100, "includeHidden": false}
	models := []Model{}
	seen := map[string]bool{}
	ids := map[string]bool{}
	for {
		var r struct {
			Data       *[]Model `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		if err := c.Call(ctx, "model/list", p, &r); err != nil {
			return nil, err
		}
		if r.Data == nil {
			return nil, errors.New("invalid native model page")
		}
		for _, model := range *r.Data {
			if !settingValue(model.ID) || !settingValue(model.Model) || ids[model.ID] || !settingValue(model.DefaultReasoningEffort) || model.SupportedReasoningEfforts == nil {
				return nil, errors.New("invalid native model metadata")
			}
			ids[model.ID] = true
			for _, effort := range model.SupportedReasoningEfforts {
				if !settingValue(effort.ReasoningEffort) {
					return nil, errors.New("invalid native model effort")
				}
			}
			for _, tier := range model.ServiceTiers {
				if !settingValue(tier.ID) {
					return nil, errors.New("invalid native model tier")
				}
			}
		}
		models = append(models, (*r.Data)...)
		if r.NextCursor == nil || *r.NextCursor == "" {
			return models, nil
		}
		if seen[*r.NextCursor] {
			return nil, errors.New("repeated pagination cursor")
		}
		seen[*r.NextCursor] = true
		p["cursor"] = *r.NextCursor
	}
}
