package sync

import (
	"sort"
	"strconv"
	"strings"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

// PlanLegacyThread plans PATCH-only adoption of a complete, quiescent M0
// snapshot. Missing delivery evidence is pending, never permission to replay.
// Queue emptiness and native directory/origin/membership proof belong to caller.
func PlanLegacyThread(host string, seed store.LegacyThreadSeed, turns []codexclient.Turn, inventory store.LegacyInventory, observedAtMS int64, maxCardBytes int) (store.LegacyThreadSeed, error) {
	fail := func() (store.LegacyThreadSeed, error) { return store.LegacyThreadSeed{}, store.ErrLegacyPending }
	if host == "" || seed.ThreadID == "" || seed.ChatID == "" || observedAtMS <= 0 || inventory.Schema != 1 || len(seed.Mirrors) != 0 || len(seed.Links) != 0 {
		return fail()
	}
	matched := 0
	for _, b := range inventory.Bindings {
		if b.ThreadID == seed.ThreadID || b.ChatID == seed.ChatID {
			if b.ThreadID != seed.ThreadID || b.ChatID != seed.ChatID {
				return fail()
			}
			matched++
		}
	}
	if matched != 1 {
		return fail()
	}
	receipts := []store.Receipt{}
	clients := map[string]bool{}
	ids := map[string]bool{}
	for _, r := range inventory.Receipts {
		if r.ThreadID != seed.ThreadID {
			if r.ChatID == seed.ChatID {
				return fail()
			}
			continue
		}
		if r.ChatID != seed.ChatID || r.ID == "" || r.ClientID == "" || r.ActionToken == "" || r.MessageID == "" || ids[r.ID] || clients[r.ClientID] {
			return fail()
		}
		switch r.State {
		case "consumed", "steered", "removed", "failed":
		default:
			return fail()
		}
		ids[r.ID], clients[r.ClientID] = true, true
		receipts = append(receipts, r)
	}
	for _, a := range inventory.Actions {
		if ids[a.ReceiptID] && a.State != "done" && a.State != "failed" {
			return fail()
		}
	}
	for _, r := range inventory.Reactions {
		if ids[r.ReceiptID] && r.State != "confirmed" && r.State != "rejected" {
			return fail()
		}
	}
	ops := map[string]store.LegacyOperation{}
	for _, o := range inventory.Operations {
		if _, duplicate := ops[o.Key]; duplicate {
			return fail()
		}
		ops[o.Key] = o
		if o.State != "confirmed" && o.State != "retired" && o.State != "rejected" {
			return fail()
		}
	}
	itemTurn := map[string]string{}
	historyClients := map[string]int{}
	firstUserItemID := ""
	for _, t := range turns {
		if t.Status == "inProgress" {
			return fail()
		}
		for _, i := range t.Items {
			if i.Type == "userMessage" && firstUserItemID == "" {
				firstUserItemID = i.ID
			}
			if old, ok := itemTurn[i.ID]; ok && old != t.ID {
				return fail()
			}
			itemTurn[i.ID] = t.ID
			if i.Type == "userMessage" && i.ClientID != nil {
				historyClients[*i.ClientID]++
			}
		}
	}
	if firstUserItemID == "" || seed.FirstUserItemID != firstUserItemID {
		return fail()
	}
	for _, r := range receipts {
		if (r.State == "consumed" || r.State == "steered") && historyClients[r.ClientID] != 1 {
			return fail()
		}
		if historyClients[r.ClientID] > 1 || (r.State == "removed" || r.State == "failed") && historyClients[r.ClientID] != 0 {
			return fail()
		}
	}
	engine := Engine{HostID: host, MaxCardBytes: maxCardBytes}
	// Existing progress cards must be rendered even for completed history.
	var prior []store.Mirror
	progressIDs := map[string][]string{}
	for _, t := range turns {
		key := "m0.progress." + seed.ThreadID + "." + t.ID
		if o, ok := ops[key]; ok {
			if o.State == "confirmed" && o.RemoteID != "" {
				progressIDs[t.ID] = append(progressIDs[t.ID], o.RemoteID)
			} else if o.State != "retired" {
				return fail()
			}
		}
		// Maintenance kept one earlier commentary mirror as a completed progress
		// card without changing its journal. It must never become final coverage.
		finals := legacyFinalItems(t)
		finalIDs := map[string]bool{}
		for _, i := range finals {
			finalIDs[i.ID] = true
		}
		for _, i := range t.Items {
			if i.Type != "agentMessage" || finalIDs[i.ID] {
				continue
			}
			prefix := "m0.mirror." + seed.ThreadID + "." + i.ID + "."
			for offset := 0; offset < len([]rune(i.PublicText())); offset += 3500 {
				if o, ok := ops[prefix+strconv.Itoa(offset)]; ok && o.State == "confirmed" && o.RemoteID != "" {
					progressIDs[t.ID] = append(progressIDs[t.ID], o.RemoteID)
				}
			}
		}
		if len(progressIDs[t.ID]) > 0 {
			// renderSnapshot needs the actual immutable initiating anchor, populated
			// below from its first pure rendering pass.
			prior = append(prior, store.Mirror{Key: mirrorKey("progress", t.ID), TurnID: t.ID, Kind: "progress"})
		}
	}
	base, err := engine.renderSnapshot(seed.ThreadID, turns, receipts, nil, true, observedAtMS)
	if err != nil {
		return fail()
	}
	anchors := map[string]store.Mirror{}
	for _, m := range base.Docs {
		if m.Kind == "final" {
			anchors[m.TurnID] = m
		}
	}
	for n := range prior {
		a, ok := anchors[prior[n].TurnID]
		if !ok {
			return fail()
		}
		prior[n].ParentKey, prior[n].ParentMessageID = a.ParentKey, a.ParentMessageID
	}
	plan, err := engine.renderSnapshot(seed.ThreadID, turns, receipts, prior, true, observedAtMS)
	if err != nil {
		return fail()
	}
	humanGroups := map[string]string{}
	for _, t := range turns {
		for _, i := range t.Items {
			if i.Type != "userMessage" || i.ClientID != nil && clients[*i.ClientID] {
				continue
			}
			text := i.PublicText()
			if strings.TrimSpace(text) == "" {
				return fail()
			}
			parts, splitErr := cards.SplitMessage("人类发送", "Codex 端", text, maxCardBytes)
			if splitErr != nil {
				return fail()
			}
			for n := range parts {
				humanGroups[mirrorKey("human", t.ID, i.ID, strconv.Itoa(n))] = "human:" + t.ID + ":" + i.ID
			}
		}
	}
	groups := map[string][]store.Mirror{}
	order := []string{}
	for _, m := range plan.Docs {
		group := m.Kind + ":" + m.TurnID
		if m.Kind == "human" {
			var found bool
			group, found = humanGroups[m.Key]
			if !found {
				return fail()
			}
		}
		if _, ok := groups[group]; !ok {
			order = append(order, group)
		}
		groups[group] = append(groups[group], m)
	}
	used := map[string]bool{}
	result := seed
	result.Links = plan.Links
	for _, group := range order {
		docs := groups[group]
		first := docs[0]
		remote := []string{}
		switch first.Kind {
		case "progress":
			remote = progressIDs[first.TurnID]
		case "human":
			itemID := strings.TrimPrefix(group, "human:"+first.TurnID+":")
			var text string
			for _, t := range turns {
				if t.ID == first.TurnID {
					for _, i := range t.Items {
						if i.ID == itemID {
							text = i.PublicText()
						}
					}
				}
			}
			remote, err = legacyStreamIDs(ops, "m0.mirror."+seed.ThreadID+"."+itemID+".", text)
		case "final":
			var turn codexclient.Turn
			for _, t := range turns {
				if t.ID == first.TurnID {
					turn = t
					break
				}
			}
			var remaining []string
			for _, i := range legacyFinalItems(turn) {
				prefix := "m0.mirror." + seed.ThreadID + "." + i.ID + "."
				if o, ok := ops[prefix+"0"]; ok && o.State == "confirmed" {
					part, e := legacyStreamIDs(ops, prefix, i.Text)
					if e != nil {
						return fail()
					}
					remote = append(remote, part...)
				} else {
					remaining = append(remaining, i.Text)
				}
			}
			if len(remaining) > 0 {
				part, e := legacyStreamIDs(ops, "m0.final."+seed.ThreadID+"."+first.TurnID+".", strings.Join(remaining, "\n\n"))
				if e != nil {
					return fail()
				}
				remote = append(remote, part...)
			}
		}
		if err != nil || len(remote) < len(docs) {
			return fail()
		}
		for n, id := range remote {
			if id == "" || used[id] {
				return fail()
			}
			used[id] = true
			var m store.Mirror
			if n < len(docs) {
				m = docs[n]
			} else {
				m = first
				m.Retired = true
				m.Terminal = true
				m.EventType = ""
				m.EventAt = 0
				m.Card = cards.Message(role(first.Kind), source(first.Kind), "此分片内容已更新并合并到前文。")
				switch first.Kind {
				case "human":
					m.Key = mirrorKey("human", first.TurnID, strings.TrimPrefix(group, "human:"+first.TurnID+":"), strconv.Itoa(n))
				case "final":
					m.Key = mirrorKey("final", first.TurnID, strconv.Itoa(n))
				default:
					m.Key = mirrorKey("progress", first.TurnID, "legacy", strconv.Itoa(n))
				}
			}
			m.HostID, m.ThreadID = host, seed.ThreadID
			m.RemoteID, m.State, m.Revision, m.DeliveredRevision, m.Replay = id, "ready", 1, 0, true
			result.Mirrors = append(result.Mirrors, m)
		}
	}
	for _, r := range receipts {
		id := r.CardID
		o, present := ops["m0.receipt-card."+r.ID]
		if present && o.State == "retired" {
			if id != "" {
				return fail()
			}
			continue
		}
		if id != "" && (!present || o.State != "confirmed" || o.RemoteID != id) {
			return fail()
		}
		if id == "" && present {
			// Maintenance recalls consumed cards and clears CardID without
			// retiring their send journal. Those IDs cannot safely be revived.
			if r.State == "consumed" {
				continue
			}
			return fail()
		}
		if id == "" {
			continue
		}
		if used[id] {
			return fail()
		}
		used[id] = true
		key := store.QueueCardKey(r.ID)
		result.Mirrors = append(result.Mirrors, store.Mirror{
			HostID: host, ThreadID: seed.ThreadID, Key: key, TurnID: key, Kind: "progress",
			Card:     cards.QueueInputView(cards.QueueView{State: r.State, Token: r.ActionToken, Revision: 1}),
			Terminal: true, Replay: true, ParentMessageID: r.MessageID,
			RemoteID: id, State: "ready", Revision: 1, DeliveredRevision: 0,
		})
	}
	return result, nil
}

func legacyFinalItems(t codexclient.Turn) []codexclient.Item {
	var result []codexclient.Item
	var last *codexclient.Item
	for _, i := range t.Items {
		if i.Type != "agentMessage" || strings.TrimSpace(i.Text) == "" {
			continue
		}
		if i.Phase != nil && *i.Phase == "final_answer" {
			result = append(result, i)
		}
		if i.Phase == nil {
			copy := i
			last = &copy
		}
	}
	if len(result) == 0 && last != nil {
		result = append(result, *last)
	}
	return result
}

func legacyStreamIDs(ops map[string]store.LegacyOperation, prefix, text string) ([]string, error) {
	runes := len([]rune(text))
	if runes == 0 {
		return nil, store.ErrLegacyPending
	}
	for offset := 0; offset < runes; offset += 3500 {
		o, ok := ops[prefix+strconv.Itoa(offset)]
		if !ok || o.State != "confirmed" || o.RemoteID == "" {
			return nil, store.ErrLegacyPending
		}
	}
	// A corrected native body may be shorter than the old delivery. Preserve
	// all confirmed tails so adoption can PATCH them to retirement notices.
	var offsets []int
	for key, o := range ops {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		offset, err := strconv.Atoi(strings.TrimPrefix(key, prefix))
		if err != nil || offset < 0 || offset%3500 != 0 || strconv.Itoa(offset) != strings.TrimPrefix(key, prefix) {
			return nil, store.ErrLegacyPending
		}
		if o.State == "retired" {
			continue
		}
		if o.State != "confirmed" || o.RemoteID == "" {
			return nil, store.ErrLegacyPending
		}
		offsets = append(offsets, offset)
	}
	sort.Ints(offsets)
	var ids []string
	for _, offset := range offsets {
		ids = append(ids, ops[prefix+strconv.Itoa(offset)].RemoteID)
	}
	return ids, nil
}
