package sync

import (
	"context"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

const replySettingsReadTimeout = 3 * time.Second

type threadSettingsReader interface {
	ReadThreadSettings(context.Context, string) (codexclient.ThreadSettings, error)
}

func unknownReplySettingsSource() string {
	return cards.ModelSettingsText("", nil, nil, false)
}

func (e Engine) readReplySettings(ctx context.Context, thread string) (string, error) {
	reader, ok := e.Native.(threadSettingsReader)
	if !ok {
		return unknownReplySettingsSource(), nil
	}
	readCtx, cancel := context.WithTimeout(ctx, replySettingsReadTimeout)
	defer cancel()
	settings, err := reader.ReadThreadSettings(readCtx, thread)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return unknownReplySettingsSource(), nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if settings.ThreadID != thread {
		return unknownReplySettingsSource(), nil
	}
	return cards.ModelSettingsText(settings.Model, settings.ReasoningEffort, settings.ServiceTier, settings.ServiceTierKnown), nil
}

func replySettingsCandidates(turns []codexclient.Turn, old, docs []store.Mirror, sources map[string]string) []string {
	oldFinal := make(map[string]bool)
	for _, mirror := range old {
		if mirror.Kind == "final" {
			oldFinal[mirror.TurnID] = true
		}
	}
	newFinal := make(map[string]bool)
	for _, doc := range docs {
		if doc.Kind != "final" || oldFinal[doc.TurnID] {
			continue
		}
		if _, stored := sources[doc.TurnID]; !stored {
			newFinal[doc.TurnID] = true
		}
	}
	result := make([]string, 0, len(newFinal))
	for _, turn := range turns {
		if newFinal[turn.ID] {
			result = append(result, turn.ID)
			delete(newFinal, turn.ID)
		}
	}
	return result
}

// observeReplySources freezes one source per newly observed final turn. Only
// the newest live turn may read native defaults; every historical/replay turn
// remains explicitly unknown so a later reconciliation cannot label it with
// today's thread settings.
func (e Engine) observeReplySources(ctx context.Context, thread string, turns []codexclient.Turn, old, docs []store.Mirror, replay bool) (bool, error) {
	candidates := replySettingsCandidates(turns, old, docs, e.replySources)
	if len(candidates) == 0 {
		return false, nil
	}
	if e.replySources == nil {
		e.replySources = make(map[string]string)
	}
	unknown := unknownReplySettingsSource()
	liveTurn := ""
	if !replay && len(turns) > 0 {
		liveTurn = turns[len(turns)-1].ID
	}
	var liveSource string
	if liveTurn != "" && containsString(candidates, liveTurn) {
		var err error
		liveSource, err = e.readReplySettings(ctx, thread)
		if err != nil {
			return false, err
		}
	}
	for _, turnID := range candidates {
		source := unknown
		if turnID == liveTurn && liveSource != "" {
			source = liveSource
		}
		stored, err := e.Store.SaveReplySettingsSource(ctx, e.HostID, thread, turnID, source)
		if err != nil {
			return false, err
		}
		e.replySources[turnID] = stored
	}
	return true, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func replySourceForTurn(turnID string, sources map[string]string) string {
	if source, ok := sources[turnID]; ok && source != "" {
		return source
	}
	// renderSnapshot is also used directly by reviewed legacy adoption, where
	// the caller must retain the pre-settings source. New live/replay finals
	// are changed to the unknown source by observeReplySources before staging.
	return "Codex"
}
