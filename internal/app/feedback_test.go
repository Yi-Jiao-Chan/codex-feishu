package app

import (
	"codex-feishu/internal/store"
	"context"
	"strings"
	"testing"
)

func TestStageReceivedIsLocalOnlyAndKeepsOriginalReply(t *testing.T) {
	r, db := receiverFixture(t)
	ctx := context.Background()
	if e := r.Message(ctx, receiverMessage(t, "chat", "ou_operator", "original-input", "group", true)); e != nil {
		t.Fatal(e)
	}
	f := Feedback{Store: db, HostID: "host", AppID: "app"} // no gateway or native connection
	if e := f.StageReceived(ctx, "thread"); e != nil {
		t.Fatal(e)
	}
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	ns, e := db.Submissions(ctx, scope)
	if e != nil || len(ns) != 1 {
		t.Fatal(e)
	}
	m, e := db.Mirror(ctx, "host", "thread", store.QueueCardKey(ns[0].ID))
	if e != nil || m.ParentMessageID != "original-input" || m.RemoteID != "" || m.State != "ready" || !strings.Contains(m.Card, "cancel_pending") || strings.Contains(m.Card, ns[0].Body) {
		t.Fatal("local stage echoed body or lost parent", e)
	}
	var attempts int
	if e := db.DB.QueryRow(`SELECT COUNT(*) FROM outbound_attempts`).Scan(&attempts); e != nil || attempts != 0 {
		t.Fatal("stage performed remote claim", e)
	}
	if e := f.StageReceived(ctx, "thread"); e != nil {
		t.Fatal(e)
	}
	again, e := db.Mirror(ctx, "host", "thread", m.Key)
	if e != nil || again.Revision != m.Revision {
		t.Fatal("offline poll changed unchanged card", e)
	}
}
