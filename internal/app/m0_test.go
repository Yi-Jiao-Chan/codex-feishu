package app

import (
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"testing"
)

func str(s string) *string { return &s }
func TestTextMentionRemovalRetainsOtherContent(t *testing.T) {
	msg := &im.EventMessage{MessageType: str("text"), Content: str(`{"text":"@_user_1  line one\n@_user_2 line two"}`), Mentions: []*im.MentionEvent{{Key: str("@_user_1"), Id: &im.UserId{OpenId: str("ou_bot")}}, {Key: str("@_user_2"), Id: &im.UserId{OpenId: str("ou_human")}}}}
	text, e := Text(msg, "ou_bot")
	if e != nil || text != "line one\n@_user_2 line two" {
		t.Fatalf("text corrupted: %q %v", text, e)
	}
}
func TestPlaintextSpoofCannotRemoveContent(t *testing.T) {
	msg := &im.EventMessage{MessageType: str("text"), Content: str(`{"text":"@机器人 literal text"}`)}
	text, e := Text(msg, "ou_bot")
	if e != nil || text != "@机器人 literal text" {
		t.Fatal("literal mention treated as metadata")
	}
}
func TestPostPreservesLinesAndRejectsUnsupportedResources(t *testing.T) {
	msg := &im.EventMessage{MessageType: str("post"), Content: str(`{"zh_cn":{"title":"Title","content":[[{"tag":"at","user_id":"ou_bot"},{"tag":"text","text":"hi"}],[{"tag":"text","text":"next"}]]}}`)}
	text, e := Text(msg, "ou_bot")
	if e != nil || text != "Title\nhi\nnext" {
		t.Fatalf("post corrupted: %q %v", text, e)
	}
	msg.Content = str(`{"content":[[{"tag":"img","image_key":"x"}]]}`)
	if _, e = Text(msg, "ou_bot"); e == nil {
		t.Fatal("unsupported resource silently dropped")
	}
}
