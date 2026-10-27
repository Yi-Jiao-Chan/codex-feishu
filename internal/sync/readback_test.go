package sync

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type readbackGateway struct {
	getCalls, listCalls int
	message             feishu.CardMessage
	pages               []feishu.CardMessagePage
	err                 error
	onGet               func(context.Context)
}

func (g *readbackGateway) ReadCardMessage(ctx context.Context, _ string) (feishu.CardMessage, error) {
	g.getCalls++
	if g.onGet != nil {
		g.onGet(ctx)
	}
	return g.message, g.err
}
func (g *readbackGateway) ListCardMessages(_ context.Context, kind, id, cursor string) (feishu.CardMessagePage, error) {
	if kind != "chat" || id != "chat" {
		return feishu.CardMessagePage{}, errors.New("wrong readback scope")
	}
	n := g.listCalls
	g.listCalls++
	if g.err != nil {
		return feishu.CardMessagePage{}, g.err
	}
	if n >= len(g.pages) {
		return feishu.CardMessagePage{}, errors.New("unexpected extra page")
	}
	return g.pages[n], nil
}

func markedUnknownFixture(t *testing.T, kind string) (Outbound, *outboundGateway, *readbackGateway, store.MirrorAttempt, string) {
	t.Helper()
	ctx := context.Background()
	s, _, m := outboundFixture(t, kind)
	m.Card = cards.Message("AI 回复", "Codex", "public result")
	if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 103}); err != nil {
		t.Fatal(err)
	}
	g := &outboundGateway{s: s, t: t, err: &feishu.Unknown{Operation: "fixture"}}
	o := Outbound{Store: s, Gateway: g}
	result, err := o.DispatchOne(ctx, "host", "thread", 1000)
	if err != nil || result.State != "unknown" {
		t.Fatal(result, err)
	}
	a, err := s.MirrorAttempt(ctx, result.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := s.MirrorRequestMarker(ctx, a.ID)
	if err != nil || len(marker) != 20 {
		t.Fatal(marker, err)
	}
	deleted := false
	msg := feishu.CardMessage{ID: "remote-found", ChatID: a.ChatID, Type: "interactive", SenderID: "app", SenderIDType: "app_id", SenderType: "app", Deleted: &deleted, Content: a.Card}
	if a.ParentMessageID != "" {
		msg.ParentID = &a.ParentMessageID
	}
	reader := &readbackGateway{message: msg, pages: []feishu.CardMessagePage{{HasMore: true, Token: "second"}, {Items: []feishu.CardMessage{msg}, HasMore: false}}}
	o.Readback = reader
	return o, g, reader, a, marker
}

func TestMirrorReadbackFindsMarkedSendReplyAcrossPagesWithoutResendingOrReminding(t *testing.T) {
	for _, kind := range []string{"human", "final"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			o, g, reader, a, _ := markedUnknownFixture(t, kind)
			// Harmless JSON formatting/key order differences remain equivalent.
			var v any
			if err := json.Unmarshal([]byte(reader.message.Content), &v); err != nil {
				t.Fatal(err)
			}
			formatted, _ := json.MarshalIndent(v, "", " ")
			reader.message.Content = string(formatted)
			r, err := o.ReconcileOne(ctx, "host", "thread", 1001)
			if err != nil || r.State != "confirmed" || r.AttemptID != a.ID || r.MessageID != "remote-found" || len(g.calls) != 1 || reader.listCalls != 2 || reader.getCalls != 1 {
				t.Fatal(r, err, g.calls, reader)
			}
			stored, err := o.Store.MirrorAttempt(ctx, a.ID)
			if err != nil || stored.Card != "" || stored.CardHash != a.CardHash || stored.State != "confirmed" {
				t.Fatal(stored, err)
			}
			if r, err := o.DispatchOne(ctx, "host", "thread", 90000); err != nil || r.AttemptID != "" || len(g.calls) != 1 {
				t.Fatal("confirmed request repeated", r, err, g.calls)
			}
		})
	}
}

func TestMirrorReadbackInsufficientEvidenceAndPaginationNeverReopenUnknown(t *testing.T) {
	for _, variant := range []string{"app", "sender_type", "sender_id_type", "chat", "parent", "deleted", "missing_deleted", "missing_parent", "thread", "different_body", "duplicate_json", "lost_marker", "duplicate_marker", "malformed_marker_copy", "partial_pages", "repeated_token", "list_failure", "changed_after_list"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			o, g, reader, a, marker := markedUnknownFixture(t, "final")
			msg := reader.message
			switch variant {
			case "app":
				msg.SenderID = "other-app"
			case "sender_type":
				msg.SenderType = "user"
			case "sender_id_type":
				msg.SenderIDType = "open_id"
			case "chat":
				msg.ChatID = "other-chat"
			case "parent":
				wrong := "other-parent"
				msg.ParentID = &wrong
			case "deleted":
				yes := true
				msg.Deleted = &yes
			case "missing_deleted":
				msg.Deleted = nil
			case "missing_parent":
				msg.ParentID = nil
			case "thread":
				id := "topic"
				msg.ThreadID = &id
			case "different_body":
				msg.Content = strings.Replace(msg.Content, "public result", "different result", 1)
			case "duplicate_json":
				msg.Content = `{"schema":"other",` + msg.Content[1:]
			case "lost_marker":
				msg.Content = strings.Replace(msg.Content, marker, "other-marker", 1)
			case "duplicate_marker":
				copy := msg
				copy.ID = "copy"
				reader.pages[1].Items = append(reader.pages[1].Items, copy)
			case "malformed_marker_copy":
				copy := msg
				copy.ID = "malformed-copy"
				copy.Content = `{"shadow":1,"shadow":2,` + copy.Content[1:]
				reader.pages[1].Items = append(reader.pages[1].Items, copy)
			case "partial_pages":
				reader.pages = nil
				for n := 0; n < 20; n++ {
					reader.pages = append(reader.pages, feishu.CardMessagePage{Items: []feishu.CardMessage{msg}, HasMore: true, Token: strings.Repeat("x", n+1)})
				}
			case "repeated_token":
				reader.pages[1].HasMore = true
				reader.pages[1].Token = "second"
			case "list_failure":
				reader.err = errors.New("fixture read failure")
			case "changed_after_list":
				msg.Content = strings.Replace(msg.Content, "public result", "later edit", 1)
			}
			reader.message = msg
			// Most failures must be rejected on the final GET as well as the list.
			r, err := o.ReconcileOne(ctx, "host", "thread", 1001)
			if r.AttemptID != "" {
				t.Fatal("insufficient evidence confirmed", variant, r, err)
			}
			frozen, e := o.Store.MirrorAttempt(ctx, a.ID)
			if e != nil || frozen.State != "unknown" || frozen.Card != a.Card || len(g.calls) != 1 {
				t.Fatal("unknown reopened", frozen, e, g.calls)
			}
			before := reader.listCalls + reader.getCalls
			if r, err := o.ReconcileOne(ctx, "host", "thread", 30000); r.AttemptID != "" || err != nil || reader.listCalls+reader.getCalls != before {
				t.Fatal("readback wait ignored", r, err)
			}
		})
	}
}

func TestFailedReadbackDoesNotStarveIndependentReadyCards(t *testing.T) {
	ctx := context.Background()
	o, g, reader, a, _ := markedUnknownFixture(t, "final")
	reader.err = errors.New("fixture GET failure")
	g.err = nil
	for n := 0; n < 4; n++ {
		key := strings.Repeat("x", n+1)
		m := store.Mirror{Key: key, TurnID: "other-turn", Kind: "human", Card: cards.Message("人类发送", "Codex", "independent public input")}
		if err := o.Store.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: int64(104 + n)}); err != nil {
			t.Fatal(err)
		}
		result, err := o.DispatchOne(ctx, "host", "thread", int64(1001+n*30001))
		if err != nil || result.Key != key || result.State != "confirmed" || len(g.calls) != 2+n {
			t.Fatal("readback starved ready card", result, err, g.calls)
		}
	}
	if frozen, err := o.Store.MirrorAttempt(ctx, a.ID); err != nil || frozen.State != "unknown" {
		t.Fatal("unknown request reopened", frozen, err)
	}
}

func TestMirrorReadbackKnownPatchConfirmsExactMarkerThenUpdatesLatestRevision(t *testing.T) {
	ctx := context.Background()
	s, _, m := outboundFixture(t, "progress")
	m.Card = cards.Progress("running", "public")
	if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 103}); err != nil {
		t.Fatal(err)
	}
	g := &outboundGateway{s: s, t: t}
	o := Outbound{Store: s, Gateway: g}
	if _, err := o.DispatchOne(ctx, "host", "thread", 1000); err != nil {
		t.Fatal(err)
	}
	m.Card = cards.Progress("running", "new public")
	if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 104}); err != nil {
		t.Fatal(err)
	}
	g.err = &feishu.Unknown{Operation: "patch"}
	r, err := o.DispatchOne(ctx, "host", "thread", 3000)
	if err != nil || r.State != "unknown" {
		t.Fatal(r, err)
	}
	a, err := s.MirrorAttempt(ctx, r.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	m.Card = cards.Progress("completed", "latest public")
	if err := s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 105}); err != nil {
		t.Fatal(err)
	}
	deleted := false
	reader := &readbackGateway{message: feishu.CardMessage{ID: a.TargetMessageID, ChatID: a.ChatID, Type: "interactive", SenderID: "app", SenderIDType: "app_id", SenderType: "app", Deleted: &deleted, ParentID: &a.ParentMessageID, Content: a.Card}}
	o.Readback = reader
	r, err = o.DispatchOne(ctx, "host", "thread", 3001)
	if err != nil || r.State != "confirmed" || reader.getCalls != 1 || reader.listCalls != 0 || len(g.calls) != 2 {
		t.Fatal(r, err, reader, g.calls)
	}
	g.err = nil
	r, err = o.DispatchOne(ctx, "host", "thread", 5001)
	if err != nil || r.State != "confirmed" || g.calls[2] != "update" || r.MessageID != a.TargetMessageID {
		t.Fatal("latest update did not retain target", r, err, g.calls)
	}
}

func TestMirrorReadbackCancellationAndContradictoryRootRemainUnknown(t *testing.T) {
	for _, variant := range []string{"cancelled", "unexpected_root"} {
		t.Run(variant, func(t *testing.T) {
			o, g, reader, a, _ := markedUnknownFixture(t, "human")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if variant == "cancelled" {
				reader.onGet = func(check context.Context) {
					deadline, ok := check.Deadline()
					if !ok || time.Until(deadline) > 5*time.Second {
						t.Fatal("readback lacks total context budget")
					}
					cancel()
				}
			} else {
				root := "unexpected-reply-root"
				reader.message.RootID = &root
			}
			result, _ := o.ReconcileOne(ctx, "host", "thread", 1001)
			if result.AttemptID != "" || len(g.calls) != 1 {
				t.Fatal("invalid context/relationship confirmed", result, g.calls)
			}
			frozen, err := o.Store.MirrorAttempt(context.Background(), a.ID)
			if err != nil || frozen.State != "unknown" {
				t.Fatal(frozen, err)
			}
		})
	}
}
