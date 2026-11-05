package interactions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func TestApprovalPathsAreShortenedWithoutRewritingCommandOrDiff(t *testing.T) {
	const home = "/synthetic/operator"
	t.Setenv("HOME", home)
	c := &Controller{EnabledTypes: []string{"command", "file"}}
	for _, kind := range []string{"command", "file"} {
		r := codexclient.InteractiveRequest{Kind: kind, CWD: home + "/project", Command: "cat " + home + "/project/file.go"}
		raw, _ := json.Marshal(r)
		row := store.InteractionRequest{Payload: string(raw), TurnID: "turn", ItemID: "item"}
		diff := "-" + home + "/old\n+" + home + "/new"
		turns := []codexclient.Turn{{ID: "turn", Items: []codexclient.Item{{ID: "item", Changes: []codexclient.FileChange{{Path: home + "/project/file.go", Kind: json.RawMessage(`{"type":"update","move_path":"/synthetic/operator/project/moved.go"}`), Diff: diff}}}}}}
		v, err := c.view(context.Background(), row, turns)
		if err != nil || v.CWD != "$HOME/project" || v.Command != r.Command || row.Payload != string(raw) {
			t.Fatal("approval changed execution data instead of its path display", err)
		}
		if kind == "file" && (len(v.Details) != 1 || v.Details[0].Title != "$HOME/project/file.go" || !strings.Contains(v.Details[0].Content, "移动到：$HOME/project/moved.go") || !strings.HasSuffix(v.Details[0].Content, diff)) {
			t.Fatal("file labels were not shortened independently of the original diff")
		}
	}
}
