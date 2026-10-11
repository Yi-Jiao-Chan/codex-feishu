package sync

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func legacyFixture() (store.LegacyThreadSeed, []codexclient.Turn, store.LegacyInventory) {
	seed := store.LegacyThreadSeed{ThreadID: "thread", ChatID: "chat", FirstUserItemID: "u"}
	turns := []codexclient.Turn{{ID: "turn", Status: "completed", Items: []codexclient.Item{input("u", "question"), agent("a", "answer", "final_answer")}}}
	inv := store.LegacyInventory{Schema: 1, Bindings: []store.Binding{{ThreadID: "thread", ChatID: "chat"}}, Operations: []store.LegacyOperation{
		{Key: "m0.mirror.thread.u.0", State: "confirmed", RemoteID: "human"},
		{Key: "m0.final.thread.turn.0", State: "confirmed", RemoteID: "final"},
	}}
	return seed, turns, inv
}

func planLegacy(t *testing.T, seed store.LegacyThreadSeed, turns []codexclient.Turn, inv store.LegacyInventory) store.LegacyThreadSeed {
	t.Helper()
	got, err := PlanLegacyThread("host", seed, turns, inv, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLegacyPlanCanonicalIDsAndPatchOnly(t *testing.T) {
	seed, turns, inv := legacyFixture()
	inv.Operations = append(inv.Operations, store.LegacyOperation{Key: "m0.progress.thread.turn", State: "confirmed", RemoteID: "progress"})
	got := planLegacy(t, seed, turns, inv)
	keys := map[string]string{mirrorKey("human", "turn", "u", "0"): "human", mirrorKey("final", "turn", "0"): "final", mirrorKey("progress", "turn"): "progress"}
	for _, m := range got.Mirrors {
		if keys[m.Key] != m.RemoteID || m.State != "ready" || m.DeliveredRevision != 0 || m.Revision != 1 || !m.Replay {
			t.Fatal("bad adopted identity or dispatch state")
		}
		if m.Kind != "human" && m.ParentKey != mirrorKey("human", "turn", "u", "0") {
			t.Fatal("reply parent moved")
		}
	}
	if len(got.Mirrors) != 3 {
		t.Fatal("lost mirror")
	}
	// A subsequent live rendering must introduce no new delivery identity.
	p, err := (Engine{HostID: "host"}).renderSnapshot("thread", turns, nil, got.Mirrors, true, 1001)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range p.Docs {
		if _, ok := keys[m.Key]; !ok {
			t.Fatal("reconcile introduced a send identity")
		}
	}
}

func TestLegacyPlanFeishuAnchorAndTerminalQueueCard(t *testing.T) {
	seed, turns, inv := legacyFixture()
	client := "client"
	turns[0].Items[0].ClientID = &client
	inv.Operations = inv.Operations[1:]
	inv.Receipts = []store.Receipt{{ID: "receipt", AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: "original", ClientID: client, ActionToken: "token", State: "steered", CardID: "queue-card"}}
	inv.Operations = append(inv.Operations, store.LegacyOperation{Key: "m0.receipt-card.receipt", State: "confirmed", RemoteID: "queue-card"})
	inv.Actions = []store.LegacyAction{{ReceiptID: "receipt", Kind: "steer", State: "done"}}
	got := planLegacy(t, seed, turns, inv)
	if len(got.Links) != 1 || got.Links[0] != (store.InputLink{ReceiptID: "receipt", TurnID: "turn", ItemID: "u"}) {
		t.Fatal("lost exact receipt link")
	}
	for _, m := range got.Mirrors {
		if m.ParentMessageID != "original" || m.ParentKey != "" {
			t.Fatal("wrong original parent")
		}
		if m.Key == store.QueueCardKey("receipt") && (!m.Terminal || strings.Contains(m.Card, `"tag":"button"`) || strings.Contains(m.Card, `"type":"callback"`)) {
			t.Fatal("terminal queue controls remain")
		}
	}
	if len(got.Mirrors) != 2 {
		t.Fatal("Feishu input duplicated or queue card lost")
	}
}

func TestLegacyPlanDoesNotReviveRecalledConsumedCard(t *testing.T) {
	seed, turns, inv := legacyFixture()
	client := "client"
	turns[0].Items[0].ClientID = &client
	inv.Operations = inv.Operations[1:]
	inv.Operations = append(inv.Operations, store.LegacyOperation{Key: "m0.receipt-card.receipt", State: "confirmed", RemoteID: "recalled"})
	inv.Receipts = []store.Receipt{{ID: "receipt", AppID: "app", ThreadID: "thread", ChatID: "chat", MessageID: "original", ClientID: client, ActionToken: "token", State: "consumed"}}
	got := planLegacy(t, seed, turns, inv)
	if len(got.Mirrors) != 1 || got.Mirrors[0].RemoteID == "recalled" {
		t.Fatal("recalled queue card revived")
	}
}

func TestLegacyPlanShrinksOldPartitionsWithoutNewMessages(t *testing.T) {
	seed, turns, inv := legacyFixture()
	turns[0].Items[1].Text = strings.Repeat("x", 7001)
	inv.Operations = append(inv.Operations, store.LegacyOperation{Key: "m0.final.thread.turn.3500", State: "confirmed", RemoteID: "tail1"}, store.LegacyOperation{Key: "m0.final.thread.turn.7000", State: "confirmed", RemoteID: "tail2"})
	got := planLegacy(t, seed, turns, inv)
	count := 0
	for _, m := range got.Mirrors {
		if m.Kind == "final" {
			if m.Key != mirrorKey("final", "turn", strconv.Itoa(count)) {
				t.Fatal("wrong tail identity")
			}
			if count > 0 && !m.Retired {
				t.Fatal("old tail left active")
			}
			count++
		}
	}
	if count != 3 {
		t.Fatal("old cards discarded")
	}
	// Even when the body was corrected below an old offset, retain that tail.
	turns[0].Items[1].Text = "short"
	got = planLegacy(t, seed, turns, inv)
	if len(got.Mirrors) != 4 {
		t.Fatal("corrected-body tails lost")
	}
}

func TestLegacyPlanMixedOldFinalItemsAndAggregatedResult(t *testing.T) {
	seed, turns, inv := legacyFixture()
	turns[0].Items = append(turns[0].Items, agent("b", "second", "final_answer"))
	inv.Operations[1].Key = "m0.mirror.thread.a.0"
	inv.Operations = append(inv.Operations, store.LegacyOperation{Key: "m0.final.thread.turn.0", State: "confirmed", RemoteID: "second"}, store.LegacyOperation{Key: "m0.mirror.thread.comment.0", State: "retired", RemoteID: "recalled"})
	got := planLegacy(t, seed, turns, inv)
	if len(got.Mirrors) != 3 {
		t.Fatal("mixed final coverage lost")
	}
	if !strings.Contains(got.Mirrors[1].Card, "answer") || !strings.Contains(got.Mirrors[1].Card, "second") || !got.Mirrors[2].Retired {
		t.Fatal("final stream not consolidated")
	}
}

func TestLegacyPlanRetainsMaintenanceProgressAndNeverUsesRetiredID(t *testing.T) {
	seed, turns, inv := legacyFixture()
	turns[0].Items = append(turns[0].Items, agent("comment", "working", "commentary"), agent("old", "earlier", "commentary"))
	inv.Operations = append(inv.Operations, store.LegacyOperation{Key: "m0.mirror.thread.comment.0", State: "confirmed", RemoteID: "kept"}, store.LegacyOperation{Key: "m0.mirror.thread.old.0", State: "retired", RemoteID: "recalled"})
	got := planLegacy(t, seed, turns, inv)
	if len(got.Mirrors) != 3 {
		t.Fatal("maintenance progress lost")
	}
	for _, m := range got.Mirrors {
		if m.RemoteID == "recalled" {
			t.Fatal("recalled ID reused")
		}
		if m.RemoteID == "kept" && m.Key != mirrorKey("progress", "turn") {
			t.Fatal("commentary promoted to final")
		}
	}
}

func TestLegacyPlanRejectsIncompleteOrAmbiguousEvidence(t *testing.T) {
	cases := map[string]func(*store.LegacyThreadSeed, *[]codexclient.Turn, *store.LegacyInventory){
		"wrong first input": func(s *store.LegacyThreadSeed, _ *[]codexclient.Turn, _ *store.LegacyInventory) {
			s.FirstUserItemID = "later"
		},
		"no turns":    func(_ *store.LegacyThreadSeed, t *[]codexclient.Turn, _ *store.LegacyInventory) { *t = nil },
		"empty draft": func(_ *store.LegacyThreadSeed, t *[]codexclient.Turn, _ *store.LegacyInventory) { (*t)[0].Items = nil },
		"active turn": func(_ *store.LegacyThreadSeed, t *[]codexclient.Turn, _ *store.LegacyInventory) {
			(*t)[0].Status = "inProgress"
		},
		"missing delivery": func(_ *store.LegacyThreadSeed, _ *[]codexclient.Turn, i *store.LegacyInventory) {
			i.Operations = i.Operations[:1]
		},
		"unknown": func(_ *store.LegacyThreadSeed, _ *[]codexclient.Turn, i *store.LegacyInventory) {
			i.Operations[0].State = "unknown"
		},
		"reused ID": func(_ *store.LegacyThreadSeed, _ *[]codexclient.Turn, i *store.LegacyInventory) {
			i.Operations[1].RemoteID = "human"
		},
		"binding mismatch": func(s *store.LegacyThreadSeed, _ *[]codexclient.Turn, _ *store.LegacyInventory) { s.ChatID = "other" },
		"duplicate binding": func(_ *store.LegacyThreadSeed, _ *[]codexclient.Turn, i *store.LegacyInventory) {
			i.Bindings = append(i.Bindings, i.Bindings[0])
		},
		"item reused across turns": func(_ *store.LegacyThreadSeed, t *[]codexclient.Turn, _ *store.LegacyInventory) {
			v := (*t)[0]
			v.ID = "other"
			*t = append(*t, v)
		},
		"pending receipt": func(_ *store.LegacyThreadSeed, _ *[]codexclient.Turn, i *store.LegacyInventory) {
			i.Receipts = []store.Receipt{{ID: "r", ThreadID: "thread", ChatID: "chat", ClientID: "c", MessageID: "m", ActionToken: "a", State: "queued"}}
		},
		"lost consumed client": func(_ *store.LegacyThreadSeed, _ *[]codexclient.Turn, i *store.LegacyInventory) {
			i.Receipts = []store.Receipt{{ID: "r", ThreadID: "thread", ChatID: "chat", ClientID: "c", MessageID: "m", ActionToken: "a", State: "consumed"}}
		},
		"missing long tail": func(_ *store.LegacyThreadSeed, t *[]codexclient.Turn, _ *store.LegacyInventory) {
			(*t)[0].Items[1].Text = strings.Repeat("x", 3501)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			seed, turns, inv := legacyFixture()
			change(&seed, &turns, &inv)
			if _, err := PlanLegacyThread("host", seed, turns, inv, 1000, 0); !errors.Is(err, store.ErrLegacyPending) {
				t.Fatalf("expected pending, got %v", err)
			}
		})
	}
	seed, turns, inv := legacyFixture()
	turns[0].Items[1].Text = strings.Repeat("x", 2000)
	if _, err := PlanLegacyThread("host", seed, turns, inv, 1000, 500); !errors.Is(err, store.ErrLegacyPending) {
		t.Fatal("new segment would require send")
	}
}

func TestLegacySeedsDispatchOnlyUpdatesAndReconcileDoesNotReplay(t *testing.T) {
	seed, turns, inv := legacyFixture()
	inv.Operations = append(inv.Operations, store.LegacyOperation{Key: "m0.progress.thread.turn", State: "confirmed", RemoteID: "progress"})
	got := planLegacy(t, seed, turns, inv)
	e, _ := engineFixture(t)
	bindEngine(t, e)
	ctx := context.Background()
	if err := e.Store.StageSnapshot(ctx, "host", "thread", got.Mirrors, nil, store.Watermark{ObservedAtMS: 1000}); err != nil {
		t.Fatal(err)
	}
	g := &cardGateway{cards: map[string]string{}}
	for _, m := range got.Mirrors {
		if _, err := e.Store.DB.ExecContext(ctx, `UPDATE mirrors SET remote_id=? WHERE host_id=? AND thread_id=? AND mirror_key=?`, m.RemoteID, "host", "thread", m.Key); err != nil {
			t.Fatal(err)
		}
		g.cards[m.RemoteID] = "old presentation"
	}
	drain(t, e, g, 1001)
	if len(g.writes) != len(got.Mirrors) {
		t.Fatal("adopted cards not updated")
	}
	for _, w := range g.writes {
		if w.Operation != "update" {
			t.Fatal("historical send or reply")
		}
	}
	apply(t, e, 1002, true, turns...)
	drain(t, e, g, 1003)
	if len(g.writes) != len(got.Mirrors) {
		t.Fatal("history replayed after adoption")
	}
	if done, err := e.FinalDelivered(ctx, "thread", "turn"); err != nil || !done {
		t.Fatal("final delivery not preserved", err)
	}
}
