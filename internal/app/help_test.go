package app

import (
	"context"
	"strings"
	"testing"
)

func TestCommandsHelpDetailsAreReadOnly(t *testing.T) {
	ctx := context.Background()
	c, s, in, _ := appCommandFixture(t)
	in.ChatID, in.ChatType, in.ThreadID = "chat", "group", "thread"
	for _, tc := range []struct {
		name, text, user, kind, want string
	}{
		{"stop", "/help stop", "ou_operator", "help", "**/stop**"},
		{"continue", "/help continue", "ou_operator", "help", "**/continue**"},
		{"model", "/help model", "ou_operator", "help", "**/model**"},
		{"slash alias for recipient", " /help   /model ", "ou_notify", "help", "**/model**"},
		{"notifications", "/help notify", "ou_notify", "help", "/notify at quiet off"},
		{"directory", "/help mute-dir", "ou_operator", "help", "/mute-dir list"},
		{"disabled creation", "/help new", "ou_operator", "help", "飞书创建功能：未启用"},
		{"disabled preview", "/help /preview", "ou_operator", "help", "文件网页预览：未启用"},
		{"unknown", "/help unknown-help-token", "ou_operator", "help", "未找到该指令的帮助"},
		{"empty slash", "/help /", "ou_operator", "help", "未找到该指令的帮助"},
		{"extra arguments", "/help model high", "ou_operator", "feedback", "用法：/help"},
		{"removed status falls back to help", "/status", "ou_operator", "help", "飞书指令"},
		{"removed reasoning falls back to help", "/reasoning high", "ou_operator", "help", "飞书指令"},
		{"removed fast falls back to help", "/fast on", "ou_operator", "help", "飞书指令"},
		{"removed help topic", "/help status", "ou_operator", "help", "未找到该指令的帮助"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in.MessageID, in.Text, in.UserID = tc.name, tc.text, tc.user
			if err := c.Handle(ctx, in); err != nil {
				t.Fatal(err)
			}
			reply, err := s.CommandReply(ctx, "host", "app", in.MessageID)
			if err != nil || reply.Kind != tc.kind || !strings.Contains(reply.Card, tc.want) || strings.Contains(reply.Card, "unknown-help-token") {
				t.Fatalf("help was not routed to the expected explanation: kind=%q err=%v", reply.Kind, err)
			}
		})
	}
	var actions, settings, directories int
	if err := s.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM submission_actions),(SELECT COUNT(*) FROM session_settings_requests),(SELECT COUNT(*) FROM directory_rules)`).Scan(&actions, &settings, &directories); err != nil || actions != 0 || settings != 0 || directories != 0 {
		t.Fatalf("help changed task or settings state: actions=%d settings=%d directories=%d err=%v", actions, settings, directories, err)
	}
	p, err := s.NotificationPolicy(ctx, "app", "ou_notify")
	if err != nil || !p.Enabled || p.Revision != 0 {
		t.Fatal("help changed notification policy", err)
	}
	receipts, err := s.Receipts(ctx, "thread")
	if err != nil || len(receipts) != 0 {
		t.Fatal("help entered the native input queue", err)
	}
}
