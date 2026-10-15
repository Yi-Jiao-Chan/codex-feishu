package cards

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func humanSegmentElements(t *testing.T, card string) []any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(card), &document); err != nil {
		t.Fatal(err)
	}
	body, ok := document["body"].(map[string]any)
	if !ok {
		t.Fatal("human card has no body")
	}
	elements, ok := body["elements"].([]any)
	if !ok {
		t.Fatal("human card has no elements")
	}
	return elements
}

func checkHumanSegments(t *testing.T, source, body string, attachments []AttachmentView, limit int) []Segment {
	t.Helper()
	parts, err := SplitHuman(source, body, attachments, limit)
	if err != nil {
		t.Fatal(err)
	}
	if limit == 0 {
		limit = DefaultMaxCardBytes
	}
	var raw strings.Builder
	for n, part := range parts {
		raw.WriteString(part.Raw)
		if !json.Valid([]byte(part.Card)) {
			t.Fatalf("segment %d is not valid JSON", n)
		}
		if len(part.Card) > limit {
			t.Fatalf("segment %d exceeds byte limit: %d > %d", n, len(part.Card), limit)
		}
	}
	if raw.String() != body {
		t.Fatalf("segments do not reconstruct original body: got %d bytes want %d", len(raw.String()), len(body))
	}
	return parts
}

func TestSplitHumanWithoutAttachmentsUsesSplitMessage(t *testing.T) {
	source := "Codex 端"
	body := strings.Repeat("原始正文😀\n", 160)
	limit := 700
	want, err := SplitMessage("人类发送", source, body, limit)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SplitHuman(source, body, nil, limit)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("attachment-free human splitting changed SplitMessage behavior")
	}
}

func TestSplitHumanPreservesLongFencedBodyAndCardBounds(t *testing.T) {
	body := "```go\n" + strings.Repeat("fmt.Println(\"中文😀\")\n", 260) + "```"
	parts := checkHumanSegments(t, "Codex 端", body, []AttachmentView{{Kind: "image", Name: "plot.png", ImageKey: "img_v3_plot"}}, 850)
	if len(parts) < 2 {
		t.Fatal("long fenced body did not split")
	}
	for n, part := range parts {
		if part.Raw != "" && (!strings.HasPrefix(part.Body, "```go\n") || !strings.HasSuffix(part.Body, "\n```")) {
			t.Fatalf("segment %d broke the Markdown fence: %q", n, part.Body[:minHumanSegment(80, len(part.Body))])
		}
	}
}

func TestSplitHumanKeepsImageKeyAndFilenameDetails(t *testing.T) {
	name := "plot.png"
	key := "img_v3_plot"
	parts := checkHumanSegments(t, "Codex 端", "正文", []AttachmentView{{Kind: "image", Name: name, Path: "/metadata/" + name, ImageKey: key}}, 0)
	if len(parts) != 1 || !strings.Contains(parts[0].Card, name) || !strings.Contains(parts[0].Card, key) {
		t.Fatalf("image preview or filename detail was lost: parts=%d card=%s", len(parts), parts[0].Card)
	}
}

func TestSplitHumanBoundsAttachmentGroupsAtThirtyTwo(t *testing.T) {
	attachments := make([]AttachmentView, 40)
	for n := range attachments {
		attachments[n] = AttachmentView{Kind: "file", Name: fmt.Sprintf("attachment-%02d.txt", n), Path: "/metadata/" + fmt.Sprintf("attachment-%02d.txt", n)}
	}
	parts := checkHumanSegments(t, "Codex 端", "", attachments, 0)
	if len(parts) < 2 {
		t.Fatal("forty attachments were not split")
	}
	seen := make(map[string]int, len(attachments))
	for n, part := range parts {
		count := 0
		for _, attachment := range attachments {
			if strings.Contains(part.Card, attachment.Name) {
				seen[attachment.Name]++
				count++
			}
		}
		if count > 32 {
			t.Fatalf("segment %d contains %d attachment details", n, count)
		}
	}
	for _, attachment := range attachments {
		if seen[attachment.Name] != 1 {
			t.Fatalf("attachment detail missing or duplicated: %s count=%d", attachment.Name, seen[attachment.Name])
		}
	}
}

func TestSplitHumanDowngradesOversizedAttachmentMetadata(t *testing.T) {
	attachment := AttachmentView{
		Kind:     "image",
		Name:     strings.Repeat("very-long-name-", 30) + ".png",
		Path:     "/private/" + strings.Repeat("path-segment-", 30) + ".png",
		ImageKey: "img_v3_private",
	}
	parts := checkHumanSegments(t, "Codex 端", "正文", []AttachmentView{attachment}, 900)
	foundNotice := false
	for _, part := range parts {
		if strings.Contains(part.Card, "详情请回 Codex App 查看") {
			foundNotice = true
		}
		if strings.Contains(part.Card, attachment.ImageKey) || strings.Contains(part.Card, "/private/") {
			t.Fatalf("oversized attachment leaked the original resource metadata: card=%s", part.Card)
		}
	}
	if !foundNotice {
		t.Fatalf("oversized attachment was not downgraded to an explicit notice: parts=%d", len(parts))
	}
}

func TestSplitHumanRejectsUnsupportedTinyLimit(t *testing.T) {
	_, err := SplitHuman("Codex 端", "正文", []AttachmentView{{Kind: "file", Name: "a.txt", Path: "/metadata/a.txt"}}, 100)
	if err == nil {
		t.Fatal("tiny limit silently dropped an attachment")
	}
}

func TestSplitHumanImageOnlyCardHasFooterAtBottom(t *testing.T) {
	parts := checkHumanSegments(t, "Codex 端", "", []AttachmentView{{Kind: "image", Name: "only.png", ImageKey: "img_v3_only"}}, 0)
	if len(parts) != 1 {
		t.Fatalf("image-only input unexpectedly split into %d cards", len(parts))
	}
	elements := humanSegmentElements(t, parts[0].Card)
	last, ok := elements[len(elements)-1].(map[string]any)
	if !ok || last["tag"] != "markdown" || last["content"] != "> 人类发送 · Codex 端" {
		t.Fatalf("footer is not the bottom element: %#v", elements[len(elements)-1])
	}
}

func TestSplitHumanNativeBlocksAndAttachmentsRespectActualBytes(t *testing.T) {
	body := strings.Repeat("first paragraph 😀\n\n- list item\n- continued\n\n> quote\n> continuation\n\n", 70)
	attachments := []AttachmentView{
		{Kind: "image", Name: "unique-preview.png", Path: "/metadata/unique-preview.png", ImageKey: "img_v3_unique_preview"},
		{Kind: "file", Name: "unique-file.txt", Path: "/metadata/unique-file.txt"},
	}
	parts := checkHumanSegments(t, "Codex 端", body, attachments, 1400)
	if len(parts) < 2 {
		t.Fatal("many native blocks should split with attachment metadata")
	}
	for _, attachment := range attachments {
		count := 0
		for _, part := range parts {
			if strings.Contains(part.Card, attachment.Name) {
				count++
			}
			if strings.Contains(part.Card, "]("+attachment.Path) {
				t.Fatal("attachment path became a fake download link")
			}
		}
		if count != 1 {
			t.Fatalf("attachment metadata appears in %d cards, want one", count)
		}
	}
}

func TestSplitHumanMinimumAttachmentBudgetFallsBackWithoutLosingText(t *testing.T) {
	attachments := []AttachmentView{{Kind: "file", Name: "file.txt", Path: "/metadata/file.txt"}}
	limit := len(Human("Codex 端", "", attachments))
	body := strings.Repeat("正文😀\n\n", 100)
	parts := checkHumanSegments(t, "Codex 端", body, attachments, limit)
	foundAttachment := false
	for _, part := range parts {
		if strings.Contains(part.Card, "file.txt") {
			foundAttachment = true
			if part.Raw != "" {
				t.Fatal("minimum attachment-only budget unexpectedly included request text")
			}
		}
	}
	if !foundAttachment {
		t.Fatal("attachment-only fallback lost the attachment")
	}
}

func minHumanSegment(a, b int) int {
	if a < b {
		return a
	}
	return b
}
