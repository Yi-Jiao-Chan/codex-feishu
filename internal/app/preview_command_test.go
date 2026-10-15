package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/store"
)

func previewCommandFixture(t *testing.T) (Commands, *store.Store, InboundCommand, string) {
	t.Helper()
	c, s, in, _ := appCommandFixture(t)
	c.Config.Access.OwnerOpenID = "ou_operator"
	c.Config.FilePreview.Enabled = true
	c.Config.FilePreview.SecretFile = filepath.Join(t.TempDir(), "preview.key")
	secret := strings.Repeat("fixture-preview-", 4)
	if err := os.WriteFile(c.Config.FilePreview.SecretFile, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	in.UserID, in.Text = "ou_operator", "/preview"
	return c, s, in, secret
}

func TestPreviewCommandOnlyRevealsToOwnerInPrivateChat(t *testing.T) {
	for _, variant := range []string{"owner", "bound_group", "unbound_group", "other_operator", "notification_recipient", "stranger", "disabled", "unavailable", "public_file", "arguments", "unsupported"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			c, s, in, secret := previewCommandFixture(t)
			want := ""
			switch variant {
			case "bound_group":
				in.ChatID, in.ChatType, in.ThreadID = "chat", "group", "thread"
				want = "私聊机器人"
			case "unbound_group":
				in.ChatType = "group"
				want = "私聊机器人"
			case "other_operator":
				in.UserID = "ou_second_operator"
				c.Config.Access.OperatorOpenIDs = append(c.Config.Access.OperatorOpenIDs, in.UserID)
				want = "部署所有者"
			case "notification_recipient":
				in.UserID = "ou_notify"
				want = "部署所有者"
			case "stranger":
				in.UserID = "ou_stranger"
			case "disabled":
				c.Config.FilePreview.Enabled = false
				want = "尚未启用"
			case "unavailable":
				if err := os.Remove(c.Config.FilePreview.SecretFile); err != nil {
					t.Fatal(err)
				}
				want = "不可读取"
			case "public_file":
				if err := os.Chmod(c.Config.FilePreview.SecretFile, 0644); err != nil {
					t.Fatal(err)
				}
				want = "不可读取"
			case "arguments":
				in.Text += " extra"
				want = "不需要参数"
			case "unsupported":
				in.Unsupported = true
				want = "仅支持文字"
			}
			if err := c.Handle(ctx, in); err != nil {
				t.Fatal(err)
			}
			r, err := s.CommandReply(ctx, "host", "app", in.MessageID)
			if variant == "stranger" {
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("stranger received a reply")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if variant == "owner" {
				if r.Kind != "preview" || !strings.Contains(r.Card, secret) || !strings.Contains(r.Card, "仅本人私聊") {
					t.Fatal("owner did not receive the preview credential")
				}
			} else if strings.Contains(r.Card, secret) || !strings.Contains(r.Card, want) {
				t.Fatal("ineligible request exposed credential or lost guidance")
			}
			if strings.Contains(r.Card, c.Config.FilePreview.SecretFile) {
				t.Fatal("reply exposed the private credential file location")
			}
			if err := c.Handle(ctx, in); err != nil {
				t.Fatal(err)
			}
			again, err := s.CommandReply(ctx, "host", "app", in.MessageID)
			if err != nil || again != r {
				t.Fatal("replayed command changed its frozen reply")
			}
			for _, table := range []string{"m0_receipts", "submission_actions"} {
				var count int
				if err := s.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("preview command entered the native task queue", err)
				}
			}
		})
	}
}

func TestPreviewCommandJournalsDeliveryAndNeverRetriesUnknown(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := previewCommandFixture(t)
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	r, err := s.CommandReply(ctx, "host", "app", in.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	g := &commandTestGateway{t: t, s: s, want: r, err: errors.New("transport ambiguous")}
	if processed, err := c.DispatchOne(ctx, g); err != nil || !processed || g.calls != 1 {
		t.Fatal("private credential delivery did not use the journal", err)
	}
	if err := c.Handle(ctx, in); err != nil {
		t.Fatal(err)
	}
	if processed, err := c.DispatchOne(ctx, g); err != nil || processed || g.calls != 1 {
		t.Fatal("ambiguous credential delivery was retried", err)
	}
	r, err = s.CommandReply(ctx, "host", "app", in.MessageID)
	if err != nil || r.State != "unknown" {
		t.Fatal("ambiguous result was not preserved", err)
	}
}

func TestPreviewCommandRechecksAuthorityBeforeSending(t *testing.T) {
	for _, revoke := range []string{"disabled", "owner", "operator"} {
		t.Run(revoke, func(t *testing.T) {
			ctx := context.Background()
			c, s, in, _ := previewCommandFixture(t)
			if err := c.Handle(ctx, in); err != nil {
				t.Fatal(err)
			}
			r, err := s.CommandReply(ctx, "host", "app", in.MessageID)
			if err != nil {
				t.Fatal(err)
			}
			switch revoke {
			case "disabled":
				c.Config.FilePreview.Enabled = false
			case "owner":
				c.Config.Access.OwnerOpenID = "ou_new_owner"
			case "operator":
				c.Config.Access.OperatorOpenIDs = nil
			}
			g := &commandTestGateway{t: t, s: s, want: r}
			if processed, err := c.DispatchOne(ctx, g); err != nil || !processed || g.calls != 0 {
				t.Fatal("revoked credential reply was sent", err)
			}
			r, err = s.CommandReply(ctx, "host", "app", in.MessageID)
			if err != nil || r.State != "rejected" {
				t.Fatal("revoked reply was not closed", err)
			}
		})
	}
}
