package creation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"codex-feishu/internal/store"
	messages "codex-feishu/internal/sync"
)

type creationRepairGateway struct{ *managerGateway }

func (g creationRepairGateway) ReplyTopicCard(context.Context, string, string, string) (string, error) {
	g.h.t.Fatal("delivery continued after its topic root was recalled")
	return "", errors.New("unexpected reply")
}

func TestManagerCreationRepairRefreshesPreviouslyCreatedEntryCard(t *testing.T) {
	h := newManagerHarness(t)
	enableManagerTopics(h)
	d := h.receive("source", "created topic task")
	h.submitTopic(d)
	d = h.draft("source")
	i := h.intent(d)
	if d.State != "created" || d.CardRevision != d.Revision || i.State != "confirmed" {
		t.Fatal("fixture did not finish the initial creation card")
	}
	if !strings.Contains(h.g.cards[d.CardID].Content, "任务已创建") {
		t.Fatal("fixture has no visible successful creation")
	}
	before := d.Revision
	updates := h.g.updates
	root := h.g.cards[i.RootMessageID]
	deleted := true
	root.Deleted = &deleted
	h.g.cards[i.RootMessageID] = root
	ctx := context.Background()
	if err := h.s.StageMirrors(ctx, "host", i.NativeThreadID, []store.Mirror{{Key: "final:late", Kind: "final", TurnID: "late-turn", ParentMessageID: i.RootMessageID, Card: `{"body":"late result"}`}}, store.Watermark{ObservedAtMS: h.clock.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	o := messages.Outbound{Store: h.s, Gateway: creationRepairGateway{h.g}}
	if _, err := o.DispatchOne(ctx, "host", i.NativeThreadID, h.clock.UnixMilli()); !errors.Is(err, messages.ErrTopicRootGone) {
		t.Fatal("recalled root did not stop late result delivery", err)
	}
	d = h.draft("source")
	if d.Revision != before+1 || d.ErrorCode != "target-root" || d.CardRevision >= d.Revision || d.State != "created" {
		t.Fatal("late target repair did not reopen only entry-card delivery")
	}
	h.tick()
	d = h.draft("source")
	if d.CardRevision != d.Revision || h.g.updates != updates+1 || !strings.Contains(h.g.cards[d.CardID].Content, "需要修复") || strings.Contains(h.g.cards[d.CardID].Content, "首条输入已接收") {
		t.Fatal("creator still sees an apparently healthy task after root recall")
	}
	for repeat := 0; repeat < 2; repeat++ {
		if err := h.s.MarkBindingRepair(ctx, "host", i.NativeThreadID, "topic_root_unavailable"); err != nil {
			t.Fatal(err)
		}
		h.tick()
	}
	h.restart()
	h.tick()
	current := h.intent(d)
	b, err := h.s.Binding(ctx, "host", i.NativeThreadID)
	if err != nil || !b.RepairRequired || current.NativeThreadID != i.NativeThreadID || current.RootMessageID != i.RootMessageID || h.g.groups != 0 || h.g.roots != 1 || len(h.n.starts) != 1 || len(h.n.adds) != 1 || h.g.updates != updates+1 {
		t.Fatal("repair refresh retried creation, changed target, or did not survive restart", err)
	}
}
