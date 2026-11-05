package creation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codex-feishu/internal/store"
)

func TestBusyFallbackShortensHomeBeforeClipping(t *testing.T) {
	home := "/synthetic/" + strings.Repeat("home", 40)
	t.Setenv("HOME", home)
	suffix := "/" + strings.Repeat("项目", 90)
	if card := busyFallback("host", home+suffix, "正在查找"); !strings.Contains(card, "$HOME"+suffix) {
		t.Fatal("busy fallback clipped the meaningful suffix before shortening home")
	}
}

func TestCreationHomeDisplayKeepsExecutionAndFrozenDescriptions(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "new"
		if legacy {
			name = "frozen legacy"
		}
		t.Run(name, func(t *testing.T) {
			h := newManagerHarness(t)
			t.Setenv("HOME", h.root)
			ctx := context.Background()
			wantDescription := "工作目录：$HOME"
			var d store.CreationDraft
			if legacy {
				d = h.seedDraft("home-display", 0)
				i, claimed, err := h.s.ClaimCreationIntent(ctx, d, store.CreationIntent{OwnerID: "ou_owner", Name: "task", Members: []string{"ou_owner", "ou_creator"}}, h.clock.UnixMilli())
				if err != nil || !claimed {
					t.Fatal("claim legacy creation", err)
				}
				wantDescription = "工作目录：" + h.root
				raw, _ := json.Marshal(groupRequest{Name: i.Name, Owner: i.OwnerID, Members: i.Members, UUID: i.ID, Description: wantDescription})
				if _, err := h.s.PrepareCreationAttempt(ctx, d.ID, "group", string(raw), h.clock.UnixMilli()); err != nil {
					t.Fatal(err)
				}
				h.g.expectedDirectory = h.root
				h.tick()
			} else {
				d = h.receive("home-display", "synthetic task")
				card := h.g.cards[d.CardID].Content
				if !strings.Contains(card, `"content":"$HOME"`) || !strings.Contains(card, `"value":"`+h.root+`"`) {
					t.Fatal("main directory label/value lost the display/execution distinction")
				}
				h.g.expectedDirectory = "$HOME"
				h.submitLocal(d)
			}
			i := h.intent(d)
			if i.State != "confirmed" || len(h.n.starts) != 1 || h.n.starts[0].CWD != h.root || i.ExecutionDirectory() != h.root {
				t.Fatal("display shortening changed actual creation")
			}
			var description string
			if err := h.s.DB.QueryRow(`SELECT description FROM create_intents WHERE intent_id=?`, i.ID).Scan(&description); err != nil || description != wantDescription {
				t.Fatal("binding did not retain the dispatched description", err)
			}
			a, err := h.s.CreationAttempt(ctx, d.ID, "group")
			var frozen groupRequest
			if err != nil || json.Unmarshal([]byte(a.Params), &frozen) != nil || frozen.Description != wantDescription {
				t.Fatal("frozen group description changed", err)
			}
			if root := h.g.cards[i.RootMessageID].Content; !strings.Contains(root, "$HOME") || strings.Contains(root, h.root) {
				t.Fatal("task root did not shorten its directory")
			}
			h.restart()
			h.tick()
			if h.g.groups != 1 || len(h.n.starts) != 1 {
				t.Fatal("display upgrade duplicated a remote resource")
			}
		})
	}
}
