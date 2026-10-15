package sync

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type topicRootFixture struct {
	root  feishu.CardMessage
	err   error
	calls int
}

func (r *topicRootFixture) ReadCardMessage(context.Context, string) (feishu.CardMessage, error) {
	r.calls++
	return r.root, r.err
}
func validTopicRoot() (store.Binding, feishu.CardMessage) {
	live := false
	platform := "platform"
	return store.Binding{FollowMode: "topic", ChatID: "chat", RootMessageID: "root", PlatformThreadID: platform}, feishu.CardMessage{ID: "root", ChatID: "chat", ThreadID: &platform, Deleted: &live}
}
func TestVerifyTopicRootRequiresLiveExactRootProof(t *testing.T) {
	for _, variant := range []string{"valid_omitted_parent_root", "valid_self_root", "valid_empty_parent_root", "valid_initial_no_thread", "deleted", "missing_deleted", "wrong_id", "wrong_chat", "child_parent", "child_root", "missing_thread", "wrong_thread"} {
		t.Run(variant, func(t *testing.T) {
			b, root := validTopicRoot()
			other, empty := "other", ""
			gone := true
			want := error(nil)
			switch variant {
			case "valid_self_root":
				root.RootID = &b.RootMessageID
			case "valid_empty_parent_root":
				root.RootID, root.ParentID = &empty, &empty
			case "valid_initial_no_thread":
				b.PlatformThreadID = ""
				root.ThreadID = nil
			case "deleted":
				root.Deleted = &gone
				want = ErrTopicRootGone
			case "missing_deleted":
				root.Deleted = nil
				want = ErrTopicRootIncomplete
			case "wrong_id":
				root.ID = other
				want = ErrTopicRootIncomplete
			case "wrong_chat":
				root.ChatID = other
				want = ErrTopicRootIncomplete
			case "child_parent":
				root.ParentID = &other
				want = ErrTopicRootIncomplete
			case "child_root":
				root.RootID = &other
				want = ErrTopicRootIncomplete
			case "missing_thread":
				root.ThreadID = nil
				want = ErrTopicRootIncomplete
			case "wrong_thread":
				root.ThreadID = &other
				want = ErrTopicRootIncomplete
			}
			reader := &topicRootFixture{root: root}
			err := VerifyTopicRoot(context.Background(), reader, b)
			if !errors.Is(err, want) || reader.calls != 1 {
				t.Fatal(variant, err, want, reader.calls)
			}
		})
	}
}
func TestVerifyTopicRootClassifiesOnlyDocumentedReadRejectionsAsGone(t *testing.T) {
	for _, code := range []int{230002, 230013, 230027, 230050, 230110, 230001, 230020, 230099, 99991400, 99991403} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			b, _ := validTopicRoot()
			rejection := &feishu.Rejected{Code: code, HTTPStatus: 400}
			reader := &topicRootFixture{err: rejection}
			err := VerifyTopicRoot(context.Background(), reader, b)
			want := ErrTopicRootIncomplete
			switch code {
			case 230002, 230013, 230027, 230050, 230110:
				want = ErrTopicRootGone
			}
			if !errors.Is(err, want) || !errors.Is(err, rejection) {
				t.Fatal(code, err)
			}
		})
	}
	b, _ := validTopicRoot()
	if err := VerifyTopicRoot(context.Background(), &topicRootFixture{err: &feishu.Unknown{Operation: "GET root"}}, b); !errors.Is(err, ErrTopicRootIncomplete) || errors.Is(err, ErrTopicRootGone) {
		t.Fatal(err)
	}
}
func TestVerifyTopicRootMissingReaderOrRootPausesButGroupDoesNotRead(t *testing.T) {
	b, _ := validTopicRoot()
	if err := VerifyTopicRoot(context.Background(), nil, b); !errors.Is(err, ErrTopicRootIncomplete) {
		t.Fatal(err)
	}
	b.RootMessageID = ""
	reader := &topicRootFixture{}
	if err := VerifyTopicRoot(context.Background(), reader, b); !errors.Is(err, ErrTopicRootIncomplete) || reader.calls != 0 {
		t.Fatal(err, reader.calls)
	}
	b.FollowMode = ""
	if err := VerifyTopicRoot(context.Background(), reader, b); err != nil || reader.calls != 0 {
		t.Fatal("group added a root GET", err, reader.calls)
	}
}
func TestTopicDeletedRootStopsReplyAndPatchEvenWhenPlatformWouldAccept(t *testing.T) {
	ctx := context.Background()
	for _, operation := range []string{"reply", "patch"} {
		t.Run(operation, func(t *testing.T) {
			s, _, m := outboundFixture(t, "progress")
			makeTopic(t, s)
			g := &topicGateway{outboundGateway: &outboundGateway{s: s, t: t}}
			o := Outbound{Store: s, Gateway: g}
			expectedCalls := 0
			if operation == "patch" {
				result, err := o.DispatchOne(ctx, "host", "thread", 1000)
				if err != nil || result.State != "confirmed" {
					t.Fatal(result, err)
				}
				m.Card = `{"body":"updated progress"}`
				if err = s.StageMirrors(ctx, "host", "thread", []store.Mirror{m}, store.Watermark{ObservedAtMS: 2000}); err != nil {
					t.Fatal(err)
				}
				expectedCalls = 1
			}
			g.rootDeleted = true
			result, err := o.DispatchOne(ctx, "host", "thread", 4000)
			if !errors.Is(err, ErrTopicRootGone) || result.AttemptID != "" || len(g.calls) != expectedCalls {
				t.Fatal("deleted root was written", result, err, g.calls)
			}
			b, err := s.Binding(ctx, "host", "thread")
			if err != nil || !b.RepairRequired {
				t.Fatal("deleted root not persisted as repair", b, err)
			}
			if _, err = o.DispatchOne(ctx, "host", "thread", 5000); !errors.Is(err, store.ErrTargetRepair) || len(g.calls) != expectedCalls {
				t.Fatal("repair target was written", err, g.calls)
			}
		})
	}
}
func TestTopicRootUnknownOrPartialDoesNotConsumeMirrorOrMarkRepair(t *testing.T) {
	ctx := context.Background()
	for _, variant := range []string{"unknown", "missing_deleted"} {
		t.Run(variant, func(t *testing.T) {
			s, _, _ := outboundFixture(t, "human")
			makeTopic(t, s)
			g := &topicGateway{outboundGateway: &outboundGateway{s: s, t: t}}
			if variant == "unknown" {
				g.rootErr = &feishu.Unknown{Operation: "root GET"}
			} else {
				g.rootMissingDeleted = true
			}
			o := Outbound{Store: s, Gateway: g}
			result, err := o.DispatchOne(ctx, "host", "thread", 1000)
			if !errors.Is(err, ErrTopicRootIncomplete) || result.AttemptID != "" || len(g.calls) != 0 {
				t.Fatal(result, err, g.calls)
			}
			b, err := s.Binding(ctx, "host", "thread")
			if err != nil || b.RepairRequired {
				t.Fatal(b, err)
			}
			g.rootErr, g.rootMissingDeleted = nil, false
			result, err = o.DispatchOne(ctx, "host", "thread", 2000)
			if err != nil || result.State != "confirmed" || len(g.calls) != 1 {
				t.Fatal("temporary read consumed write", result, err, g.calls)
			}
		})
	}
}

type topicWriterWithoutRootReader struct {
	MessageGateway
	TopicMessageGateway
}

func TestTopicWriteWithoutRootReadCapabilityPausesBeforeClaim(t *testing.T) {
	ctx := context.Background()
	s, _, _ := outboundFixture(t, "human")
	makeTopic(t, s)
	g := &topicGateway{outboundGateway: &outboundGateway{s: s, t: t}}
	writer := topicWriterWithoutRootReader{MessageGateway: g, TopicMessageGateway: g}
	result, err := (Outbound{Store: s, Gateway: writer}).DispatchOne(ctx, "host", "thread", 1000)
	if !errors.Is(err, ErrTopicRootIncomplete) || result.AttemptID != "" || len(g.calls) != 0 || g.rootReads != 0 {
		t.Fatal("missing root reader allowed a write", result, err, g.calls)
	}
	b, err := s.Binding(ctx, "host", "thread")
	if err != nil || b.RepairRequired {
		t.Fatal("missing capability marked repair", b, err)
	}
}
