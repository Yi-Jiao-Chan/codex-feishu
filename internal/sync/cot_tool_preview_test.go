package sync

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
)

func TestCOTCommandPreviewPreservesScriptAndObservedOutput(t *testing.T) {
	command := "python3 - <<'PY'\nready = 65\nprint({'bindings': 110, 'ready': ready})\nPY"
	output := `{"bindings":110,"ready":65}`
	item := codexclient.Item{ID: "tool", Type: "commandExecution", Status: "completed", Command: command, AggregatedOutput: &output}
	public := projectedTool(t, item)
	if public.InputPreview != cards.Safe(command) || !strings.Contains(public.OutputPreview, `"ready": 65`) || !strings.Contains(public.OutputPreview, `"bindings": 110`) {
		t.Fatalf("script/result disappeared behind generic metadata: %+v", public)
	}
	legacy, err := cotDocumentVersion("thread", codexclient.Turn{ID: "turn", Status: "inProgress", Items: []codexclient.Item{item}}, nil, 1000, cot.LegacyVersion)
	if err != nil || strings.Contains(legacy, "ready") || strings.Contains(legacy, "input_preview") || strings.Contains(legacy, "output_preview") {
		t.Fatal("legacy display started copying native execution content")
	}
}

func TestCOTToolPreviewRedactsCredentialsBeforeClipping(t *testing.T) {
	for _, tc := range []struct {
		name, text, secret, want string
	}{
		{"env", `FIXTURE_TOKEN=SYNTHETIC_CREDENTIAL git status --short`, "SYNTHETIC_CREDENTIAL", "git status --short"},
		{"quoted-flag", `inspect --password 'synthetic credential with spaces' fixture.go`, "synthetic credential with spaces", "fixture.go"},
		{"script", "api_key = 'SYNTHETIC_CREDENTIAL'\nprint('checked 3 files')", "SYNTHETIC_CREDENTIAL", "checked 3 files"},
		{"triple-double", "password = \"\"\"SYNTHETIC_CREDENTIAL\nwith spaces\"\"\"\nprint('checked 3 files')", "SYNTHETIC_CREDENTIAL", "checked 3 files"},
		{"triple-single", "password = '''SYNTHETIC_CREDENTIAL\nwith spaces'''\nprint('checked 3 files')", "SYNTHETIC_CREDENTIAL", "checked 3 files"},
		{"template-literal", "const password = `first part SYNTHETIC_CREDENTIAL`;\nconsole.log('checked 3 files')", "SYNTHETIC_CREDENTIAL", "checked 3 files"},
		{"raw-python", `password = r"first part SYNTHETIC_CREDENTIAL"` + "\nprint('checked 3 files')", "SYNTHETIC_CREDENTIAL", "checked 3 files"},
		{"adjacent-shell-quotes", `PASSWORD='first part'"SYNTHETIC_CREDENTIAL" inspect fixture.go`, "SYNTHETIC_CREDENTIAL", "fixture.go"},
		{"escaped-shell-quotes", `python3 -c "password=\"first part SYNTHETIC_CREDENTIAL\"; print('checked 3 files')"`, "SYNTHETIC_CREDENTIAL", "checked 3 files"},
		{"escaped-env", `PASSWORD=first\ SYNTHETIC_CREDENTIAL inspect fixture.go`, "SYNTHETIC_CREDENTIAL", "fixture.go"},
		{"yaml-block", "config:\n  password: |\n    first part\n    SYNTHETIC_CREDENTIAL\n  matches: 3\n", "SYNTHETIC_CREDENTIAL", "matches: 3"},
		{"header", "found 3 files\nAuthorization: Bearer SYNTHETIC_CREDENTIAL", "SYNTHETIC_CREDENTIAL", "found 3 files"},
		{"curl-header", `curl -H 'Authorization: Bearer SYNTHETIC_CREDENTIAL' https://example.invalid`, "SYNTHETIC_CREDENTIAL", "curl"},
		{"cookie", "status: 200\nSet-Cookie: session=SYNTHETIC_CREDENTIAL; HttpOnly", "SYNTHETIC_CREDENTIAL", "status: 200"},
		{"url-userinfo", "connected https://alice:SYNTHETIC_CREDENTIAL@example.invalid/path", "SYNTHETIC_CREDENTIAL", "example.invalid/path"},
		{"query", "https://example.invalid/?access_token=SYNTHETIC_CREDENTIAL&q=fixture", "SYNTHETIC_CREDENTIAL", "q=fixture"},
		{"basic-auth-short", "curl -u user:SYNTHETIC_CREDENTIAL https://example.invalid", "SYNTHETIC_CREDENTIAL", "example.invalid"},
		{"basic-auth-compact", "curl -uuser:SYNTHETIC_CREDENTIAL https://example.invalid", "SYNTHETIC_CREDENTIAL", "example.invalid"},
		{"basic-auth-long", "curl --user 'user:SYNTHETIC_CREDENTIAL' https://example.invalid", "SYNTHETIC_CREDENTIAL", "example.invalid"},
		{"temporary-aws", "ASIASYNTHETICKEY12345\n3 matches", "ASIASYNTHETICKEY12345", "3 matches"},
		{"account-token", `{"account_token":"SYNTHETIC_CREDENTIAL","matches":3}`, "SYNTHETIC_CREDENTIAL", `"matches": 3`},
		{"json-escaped-key", `{"api\u005fkey":"SYNTHETIC_CREDENTIAL","matches":3}`, "SYNTHETIC_CREDENTIAL", `"matches": 3`},
		{"nested-json", `{"payload":"{\"secret\":\"SYNTHETIC_CREDENTIAL\",\"count\":3}","ok":true}`, "SYNTHETIC_CREDENTIAL", `"ok": true`},
		{"nested-json-array", `{"payload":"[{\"pa\\u0073sword\":\"SYNTHETIC_CREDENTIAL\"}]","ok":true}`, "SYNTHETIC_CREDENTIAL", `"ok": true`},
		{"token-prefix", "key sk-synthetickey0123456789abcdef\n3 matches", "sk-synthetickey0123456789abcdef", "3 matches"},
		{"pem", "before\n-----BEGIN PRIVATE KEY-----\nSYNTHETIC_CREDENTIAL\n-----END PRIVATE KEY-----\nafter", "SYNTHETIC_CREDENTIAL", "after"},
		{"ansi", "\x1b[31mAPI_KEY=SYNTHETIC_CREDENTIAL\x1b[0m\n3 matches", "SYNTHETIC_CREDENTIAL", "3 matches"},
		{"long", "password='" + strings.Repeat("SYNTHETIC_CREDENTIAL", 300) + "'\n3 matches", "SYNTHETIC_CREDENTIAL", "3 matches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cotSafePreview(tc.text, "", cot.MaxToolOutputRunes)
			if strings.Contains(got, tc.secret) || !strings.Contains(got, cotRedacted) || !strings.Contains(got, tc.want) {
				t.Fatalf("credential masking lost its boundary or useful content: %q", got)
			}
		})
	}
	got := cotSafePreview(`{"token_count":123,"max_output_tokens":2000,"api_key":"synthetic secret"}`, "", 1000)
	if !strings.Contains(got, "123") || !strings.Contains(got, "2000") || strings.Contains(got, "synthetic secret") {
		t.Fatal("credential masking confused numeric token accounting with keys")
	}
	got = cotSafePreview(`{"script":"password=synthetic_secret\nprint('3 matches')","Authorization":"Bearer synthetic_secret","query":"fixture"}`, "", 1000)
	if !json.Valid([]byte(got)) || strings.Contains(got, "synthetic_secret") || !strings.Contains(got, "3 matches") || cotSafePreview(got, "", 1000) != got {
		t.Fatalf("repeated redaction corrupted a structured text preview: %q", got)
	}
}

func TestCOTToolPreviewLimitsMediaControlsAndHostPaths(t *testing.T) {
	got := cotSafePreview("cat /home/synthetic/repo/internal/fixture.go\n/home/synthetic/other.go\ncontact alice@example.invalid\n<at id=all>hello</at>\x00\u202e", "/home/synthetic/repo", 1000)
	for _, want := range []string{"cat internal/fixture.go", "~/other.go", "alice<A>example.invalid", "&lt;at"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing display-safe content %q", want)
		}
	}
	for _, unwanted := range []string{"/home/synthetic", "@example", "<at", "\x00", "\u202e"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("unsafe display data %q survived", unwanted)
		}
	}
	got = cotSafePreview("data:image/png;base64,"+strings.Repeat("a", 300)+"\nsummary: 3 files", "", 1000)
	if strings.Contains(got, "base64") || !strings.Contains(got, "summary: 3 files") {
		t.Fatal("binary payload was displayed or displaced the useful result")
	}
	got = cotSafePreview("start\n"+strings.Repeat("界\n", 3000)+"\nFAIL: expected 2, got 3", "", cot.MaxToolOutputRunes)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) > cot.MaxToolOutputRunes || !strings.HasPrefix(got, "start") || !strings.HasSuffix(got, "FAIL: expected 2, got 3") || !strings.Contains(got, cotPreviewOmitted) {
		t.Fatal("bounded output lost Unicode, head, failure tail or truncation marker")
	}
	got = cotSafePreview(strings.Repeat("x", cotPreviewReadLimit+1), "", cot.MaxToolOutputRunes)
	if got != "内容过长，已省略" {
		t.Fatal("oversized opaque text was copied without complete redaction")
	}
}

func TestCOTToolPreviewUsesTypedTextAndStructuredFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		item codexclient.Item
		want []string
	}{
		{"mcp-structured", codexclient.Item{Type: "mcpToolCall", Status: "completed", Arguments: json.RawMessage(`{"query":"fixture","api_key":"PRIVATE_KEY"}`), Result: &codexclient.MCPToolResult{Content: []codexclient.ToolOutputContent{{Type: "image", Text: "PRIVATE_MEDIA"}}, StructuredContent: json.RawMessage(`{"matches":3,"files":["one.go","two.go"],"token":"PRIVATE_TOKEN"}`)}}, []string{"fixture", `"matches": 3`, "one.go", "two.go"}},
		{"mcp-structured-media", codexclient.Item{Type: "mcpToolCall", Status: "completed", Result: &codexclient.MCPToolResult{StructuredContent: json.RawMessage(`{"matches":3,"preview":{"type":"image","mimeType":"image/png","data":"PRIVATE_SHORT_IMAGE"},"resource":{"type":"resource","text":"PRIVATE_RESOURCE"},"image_url":"PRIVATE_IMAGE_URL"}`)}}, []string{`"matches": 3`, "非文本内容已省略"}},
		{"mcp-nested-media-array", codexclient.Item{Type: "mcpToolCall", Status: "completed", Result: &codexclient.MCPToolResult{StructuredContent: json.RawMessage(`{"matches":3,"nested":"[{\"type\":\"image\",\"mimeType\":\"image/png\",\"data\":\"PRIVATE_SHORT_IMAGE\"}]"}`)}}, []string{`"matches": 3`, "非文本内容已省略"}},
		{"dynamic-text", codexclient.Item{Type: "dynamicToolCall", Status: "completed", Arguments: json.RawMessage(`{"path":"fixture.go"}`), ContentItems: []codexclient.ToolOutputContent{{Type: "inputText", Text: "2 tests passed"}, {Type: "inputAudio", Text: "PRIVATE_MEDIA"}}}, []string{"fixture.go", "2 tests passed"}},
		{"file-diff", codexclient.Item{Type: "fileChange", Status: "completed", Changes: []codexclient.FileChange{{Path: "/private/fixture.go", Diff: "@@ -1 +1 @@\n-oldValue\n+newValue\n+token=PRIVATE_TOKEN"}}}, []string{"fixture.go", "-oldValue", "+newValue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			public := projectedTool(t, tc.item)
			encoded, _ := json.Marshal(public)
			if strings.Contains(string(encoded), "PRIVATE_") {
				t.Fatal("credentials or unsupported media text escaped")
			}
			for _, want := range tc.want {
				if !strings.Contains(public.InputPreview+public.OutputPreview, want) {
					t.Fatalf("missing actual tool detail %q", want)
				}
			}
		})
	}
}

func TestCOTToolPreviewDistinguishesMissingAndEmptyOutput(t *testing.T) {
	empty := ""
	for _, status := range []string{"completed", "failed", "inProgress", "unknown"} {
		for _, output := range []*string{nil, &empty} {
			public := projectedTool(t, codexclient.Item{Type: "commandExecution", Status: status, Command: "git status --short", AggregatedOutput: output})
			want := ""
			if status == "completed" && output != nil {
				want = "命令未返回输出"
			}
			if public.ResultSummary != want || public.InputPreview != "git status --short" {
				t.Fatal("missing output became an invented execution result")
			}
		}
	}
	for _, output := range []string{"[]", "{}", "null", "false", "0", " M fixture.go"} {
		public := projectedTool(t, codexclient.Item{Type: "commandExecution", Status: "completed", Command: "inspect", AggregatedOutput: &output})
		if public.OutputPreview != output || public.ResultSummary != "" {
			t.Fatalf("observed empty/false/zero JSON or Git whitespace was lost: %q", output)
		}
	}
}
