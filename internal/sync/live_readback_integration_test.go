package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
)

// This opt-in probe reads three already-delivered cards. It never sends,
// updates, recalls, starts a listener, or opens the live database for writing.
// The input contains private references, not credentials or message text.
func TestLiveFeishuCardReadback(t *testing.T) {
	if os.Getenv("CODEX_FEISHU_LIVE_READBACK_TEST") != "1" {
		t.Skip("explicit live readback opt-in required")
	}
	var input struct {
		Config   string   `json:"config"`
		Attempts []string `json:"attempts"`
	}
	f, err := os.Open(os.Getenv("CODEX_FEISHU_LIVE_READBACK_INPUT"))
	if err != nil {
		t.Fatal("private readback input unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("readback input must be a private regular file")
	}
	d := json.NewDecoder(io.LimitReader(f, 8193))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil || d.Decode(new(any)) != io.EOF || len(input.Attempts) != 3 {
		t.Fatal("invalid private readback input")
	}
	cfg, err := config.Load(input.Config)
	if err != nil {
		t.Fatal("readback configuration unavailable")
	}
	secret, err := cfg.Secret()
	if err != nil {
		t.Fatal("protected credential reference unavailable")
	}
	g := feishu.New(cfg.Feishu.AppID, secret)
	u := url.URL{Scheme: "file", Path: cfg.Storage.DatabaseFile}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal("read-only database unavailable")
	}
	defer db.Close()
	ctx := context.Background()
	seen := map[string]bool{}
	for _, id := range input.Attempts {
		var operation, remote string
		var proof CardReadbackProof
		err := db.QueryRowContext(ctx, `SELECT a.operation, a.remote_id, a.chat_id,
			a.parent_message_id, a.target_message_id, a.card_json, r.marker
			FROM outbound_attempts a JOIN mirrors m ON m.host_id=a.host_id
			AND m.thread_id=a.thread_id AND m.mirror_key=a.mirror_key
			JOIN bindings b ON b.host_id=a.host_id AND b.thread_id=a.thread_id
			JOIN create_intents i ON i.intent_id=b.intent_id
			JOIN card_request_markers r ON r.attempt_id=a.attempt_id AND r.kind='mirror'
			WHERE a.attempt_id=? AND a.host_id=? AND i.owner_id=?
			AND a.state='confirmed' AND a.revision=m.revision
			AND m.delivered_revision=m.revision AND m.remote_id=a.remote_id
			AND b.chat_id=a.chat_id`, id, cfg.Host.ID, cfg.Access.OwnerOpenID).
			Scan(&operation, &remote, &proof.ChatID, &proof.ParentMessageID,
				&proof.TargetMessageID, &proof.Card, &proof.Marker)
		if err != nil || seen[operation] || operation != "send" && operation != "reply" && operation != "update" {
			t.Fatal("selected attempt is not a unique current owner-bound delivery")
		}
		seen[operation] = true
		proof.AppID = cfg.Feishu.AppID
		t.Run(operation, func(t *testing.T) {
			members, err := g.MemberIDs(ctx, proof.ChatID)
			if err != nil || len(members) != 1 || members[0] != cfg.Access.OwnerOpenID {
				t.Fatal("readback scope is not an owner-only test group")
			}
			inChat, err := g.BotInChat(ctx, proof.ChatID)
			if err != nil || !inChat {
				t.Fatal("test-group bot membership not proven")
			}
			message, err := g.ReadCardMessage(ctx, remote)
			if err != nil {
				t.Fatal("real card GET failed")
			}
			if directory := os.Getenv("CODEX_FEISHU_LIVE_READBACK_EVIDENCE_DIR"); directory != "" {
				info, err := os.Lstat(directory)
				if err != nil || !filepath.IsAbs(directory) || !info.IsDir() || info.Mode().Perm() != 0700 {
					t.Fatal("readback evidence directory must be private and physical")
				}
				body, err := json.Marshal(map[string]any{"operation": operation, "expected_card": proof.Card, "observed_card": message.Content, "marker_retained": markerCount(message.Content, proof.Marker) == 1, "exact_card_and_relationship_match": matchesCardReadback(proof, message)})
				if err != nil || os.WriteFile(filepath.Join(directory, operation+".json"), body, 0600) != nil {
					t.Fatal("private readback evidence could not be saved")
				}
			}
			if markerCount(message.Content, proof.Marker) != 1 {
				t.Fatal("platform did not retain the frozen attempt marker")
			}
			if !matchesCardReadback(proof, message) {
				// Report only which structural paths differ; neither card content
				// nor remote identities may appear in test output.
				t.Errorf("real GET did not prove the full frozen card and relationship")
				t.Logf("metadata: sender=%t chat=%t deleted=%t parent=%t topic=%t", message.SenderID == proof.AppID && message.SenderIDType == "app_id" && message.SenderType == "app", message.ChatID == proof.ChatID, message.Deleted != nil && !*message.Deleted, proof.ParentMessageID == "" && (message.ParentID == nil || *message.ParentID == "") || message.ParentID != nil && *message.ParentID == proof.ParentMessageID, message.ThreadID == nil || *message.ThreadID == "")
				t.Logf("wire field differences: %s", strings.Join(liveCardDifferencePaths(proof.Card, message.Content), ","))
				return
			}
			found, err := FindMarkedCard(ctx, g, proof)
			if err != nil || found != remote {
				t.Fatal("real GET/list proof was inconclusive; no write or retry performed")
			}
		})
	}
}

func liveCardDifferencePaths(want, got string) []string {
	var a, b any
	if json.Unmarshal([]byte(want), &a) != nil || json.Unmarshal([]byte(got), &b) != nil {
		return []string{"invalid-json"}
	}
	var paths []string
	var walk func(any, any, string)
	walk = func(a, b any, path string) {
		switch a := a.(type) {
		case map[string]any:
			other, ok := b.(map[string]any)
			if !ok {
				paths = append(paths, path+":type")
				return
			}
			for k, value := range a {
				v, exists := other[k]
				if !exists {
					paths = append(paths, path+"."+k+":missing")
				} else {
					walk(value, v, path+"."+k)
				}
			}
			for k := range other {
				if _, exists := a[k]; !exists {
					paths = append(paths, path+"."+k+":extra")
				}
			}
		case []any:
			other, ok := b.([]any)
			if !ok || len(a) != len(other) {
				paths = append(paths, path+":array")
				return
			}
			for i := range a {
				walk(a[i], other[i], path+"[]")
			}
		default:
			if !reflect.DeepEqual(a, b) {
				paths = append(paths, path+":value")
			}
		}
	}
	walk(a, b, "card")
	return paths
}
