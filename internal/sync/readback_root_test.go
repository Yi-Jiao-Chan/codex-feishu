package sync

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/feishu"
)

func rootReadbackFixture() (CardReadbackProof, feishu.CardMessage) {
	deleted := false
	root, thread := "created-root", "platform-thread"
	card := `{"body":{"elements":[{"tag":"div","element_id":"root-marker"}]}}`
	proof := CardReadbackProof{RootCreation: true, AppID: "app", ChatID: "chat", Card: card, Marker: "root-marker"}
	message := feishu.CardMessage{ID: root, ChatID: "chat", Type: "interactive", SenderID: "app", SenderIDType: "app_id", SenderType: "app", RootID: &root, ThreadID: &thread, Deleted: &deleted, Content: card}
	return proof, message
}
func TestRootCreationReadbackAcceptsRootAfterFirstTopicReply(t *testing.T) {
	for _, variant := range []string{"self_root_and_thread", "missing_root", "empty_root", "plain_group_root"} {
		t.Run(variant, func(t *testing.T) {
			proof, message := rootReadbackFixture()
			empty := ""
			switch variant {
			case "missing_root":
				message.RootID = nil
			case "empty_root":
				message.RootID = &empty
			case "plain_group_root":
				message.RootID, message.ThreadID = nil, nil
			}
			reader := &readbackGateway{message: message, pages: []feishu.CardMessagePage{{Items: []feishu.CardMessage{message}, HasMore: true, Token: "second"}, {HasMore: false}}}
			id, err := FindMarkedCard(context.Background(), reader, proof)
			if err != nil || id != message.ID || reader.listCalls != 2 || reader.getCalls != 1 {
				t.Fatal("accepted root was not confirmed after full scan", id, err, reader)
			}
		})
	}
}
func TestRootCreationReadbackNeverAcceptsTopicChildOrOtherIdentity(t *testing.T) {
	for _, variant := range []string{"parent", "other_root", "expected_parent", "expected_root", "expected_thread", "sender", "chat", "deleted", "missing_deleted", "body", "marker"} {
		t.Run(variant, func(t *testing.T) {
			proof, message := rootReadbackFixture()
			other := "other"
			yes := true
			switch variant {
			case "parent":
				message.ParentID = &other
			case "other_root":
				message.RootID = &other
			case "expected_parent":
				proof.ParentMessageID = other
			case "expected_root":
				proof.RootMessageID = other
			case "expected_thread":
				proof.PlatformThreadID = other
			case "sender":
				message.SenderID = other
			case "chat":
				message.ChatID = other
			case "deleted":
				message.Deleted = &yes
			case "missing_deleted":
				message.Deleted = nil
			case "body":
				message.Content = `{"body":{"elements":[{"tag":"div","element_id":"root-marker","content":"other"}]}}`
			case "marker":
				proof.Marker = other
			}
			if matchesCardReadback(proof, message) {
				t.Fatal("root proof accepted insufficient identity or relationship", variant)
			}
		})
	}
}
func TestRootCreationReadbackRequiresCompleteUniqueMarkerAndFreshRoot(t *testing.T) {
	for _, variant := range []string{"duplicate", "targeted_duplicate", "incomplete", "became_child", "parent_on_proof"} {
		t.Run(variant, func(t *testing.T) {
			proof, message := rootReadbackFixture()
			reader := &readbackGateway{message: message, pages: []feishu.CardMessagePage{{Items: []feishu.CardMessage{message}, HasMore: false}}}
			switch variant {
			case "duplicate", "targeted_duplicate":
				if variant == "targeted_duplicate" {
					proof.TargetMessageID = message.ID
				}
				copy := message
				copy.ID = "duplicate-root"
				reader.pages[0].HasMore, reader.pages[0].Token = true, "second"
				reader.pages = append(reader.pages, feishu.CardMessagePage{Items: []feishu.CardMessage{copy}})
			case "incomplete":
				reader.pages[0].HasMore, reader.pages[0].Token = true, "second"
			case "became_child":
				other := "other-root"
				reader.message.RootID = &other
			case "parent_on_proof":
				proof.ParentMessageID = "input"
			}
			id, err := FindMarkedCard(context.Background(), reader, proof)
			if id != "" {
				t.Fatal("ambiguous/root-child result confirmed", id, err)
			}
			if variant == "duplicate" || variant == "targeted_duplicate" || variant == "parent_on_proof" {
				if !errors.Is(err, ErrReadbackIncomplete) {
					t.Fatal(err)
				}
			}
			if variant == "parent_on_proof" && (reader.listCalls != 0 || reader.getCalls != 0) {
				t.Fatal("invalid root proof performed reads")
			}
		})
	}
}
func TestRootCreationDoesNotRelaxOrdinaryGroupReadback(t *testing.T) {
	proof, message := rootReadbackFixture()
	proof.RootCreation = false
	if matchesCardReadback(proof, message) {
		t.Fatal("ordinary send accepted a topic root")
	}
	message.ThreadID = nil
	if matchesCardReadback(proof, message) {
		t.Fatal("ordinary send accepted a self root without explicit creation proof")
	}
}
