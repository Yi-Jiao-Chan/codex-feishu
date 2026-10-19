package filepreview

import (
	"codex-feishu/internal/cards"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteOnlyParsedInlineLocalLinks(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, "main.go", "package main\n")
	url := registeredURL(t, f.s, path)
	good := "[source](" + path + ")"
	unchanged := []string{
		"`" + good + "`",
		"``code ` " + good + "``",
		"```go\n" + good + "\n```",
		"~~~text\n" + good + "\n~~~",
		"    " + good,
		"\\" + good,
		"![image](" + path + ")",
		"![alt " + good + "](" + path + ")",
		"[website](https://example.invalid" + path + ")",
		"[website](http://example.invalid" + path + ")",
		"[relative](main.go)",
		"[reference][a]\n\n[a]: " + path,
		"<" + path + ">",
	}
	for _, body := range unchanged {
		got, err := f.s.Rewrite(context.Background(), body)
		if err != nil || got != body {
			t.Fatalf("non-inline-file syntax changed: %q (error %v)", body, err)
		}
	}
	for _, body := range []string{good, "> " + good, "# " + good, "- " + good, good + ` "literal"`, "[source](" + path + ` "title")`} {
		got, err := f.s.Rewrite(context.Background(), body)
		if err != nil || !strings.Contains(got, url) || strings.Contains(got, path) {
			t.Fatalf("valid inline file link was not rewritten: %q (error %v)", body, err)
		}
	}
	mixed := unchanged[2] + "\n\n" + good + "\n" + unchanged[0]
	got, err := f.s.Rewrite(context.Background(), mixed)
	if err != nil || strings.Count(got, url) != 1 || !strings.Contains(got, unchanged[2]) || !strings.Contains(got, unchanged[0]) {
		t.Fatal("mixed prose/code boundaries were not preserved")
	}
}

func TestRewriteBareMalformedLocalURLUsesParsedSourceBoundaries(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, "main.go", "package main\n")
	address := registeredURL(t, f.s, path)
	bad := "http://" + strings.TrimPrefix(path, "/")
	for _, body := range []string{bad, "报告：\n\n" + bad, "查看 " + bad + "。", "<" + bad + ">", "(" + bad + ")"} {
		got, err := f.s.Rewrite(context.Background(), body)
		if err != nil || !strings.Contains(got, "[main.go]("+address+")") || strings.Contains(got, bad) {
			t.Fatalf("bare malformed local URL not converted: %q (error %v)", got, err)
		}
	}
	for _, body := range []string{"`" + bad + "`", "```text\n" + bad + "\n```", "    " + bad, "http://example.invalid" + path} {
		if got, err := f.s.Rewrite(context.Background(), body); err != nil || got != body {
			t.Fatalf("code or real web URL was changed: %q (error %v)", got, err)
		}
	}
	mixed := "`" + bad + "`\n\n" + bad + ":12:4"
	got, err := f.s.Rewrite(context.Background(), mixed)
	if err != nil || !strings.HasPrefix(got, "`"+bad+"`") || !strings.HasSuffix(got, "[main.go · L12]("+address+"#L12)") {
		t.Fatalf("identical code/prose occurrences lost their boundaries: %q (error %v)", got, err)
	}
}

func TestRewriteUnicodeSpacesLocationsAndLocalURLRepair(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, "方案 目录/实现 (稿).md", "# 报告\n")
	base := registeredURL(t, f.s, path)
	cases := []struct{ body, want string }{
		{"[实现](<" + path + ":12:4>)", "[实现 · L12](" + base + "#L12)"},
		{"[实现 L12](<" + path + ":12>)", "[实现 L12](" + base + "#L12)"},
		{"[实现 L120](<" + path + ":12>)", "[实现 L120 · L12](" + base + "#L12)"},
		{"[实现](<" + path + "#L12C5>)", "[实现 · L12](" + base + "#L12)"},
		{"[实现](<file://localhost" + path + ">)", "[实现](" + base + ")"},
		{"[实现](<file://" + path + ">)", "[实现](" + base + ")"},
		{"[实现](<http://" + strings.TrimPrefix(path, "/") + ">)", "[实现](" + base + ")"},
	}
	for _, tc := range cases {
		got, err := f.s.Rewrite(context.Background(), tc.body)
		if err != nil || got != tc.want {
			t.Fatalf("rewrite = %q, want %q, error %v", got, tc.want, err)
		}
		if safe := cards.SafeMarkdown(got); strings.Contains(safe, "&lt;http") || !strings.Contains(safe, base) {
			t.Fatal("preview URL was damaged by native card Markdown escaping")
		}
	}
	for _, target := range []string{"file://remote.invalid" + path, "https://" + strings.TrimPrefix(path, "/"), "http://www." + strings.TrimPrefix(path, "/")} {
		body := "[remote](<" + target + ">)"
		got, err := f.s.Rewrite(context.Background(), body)
		if err != nil || got != body {
			t.Fatal("a remote URL was rewritten")
		}
	}
	paren := writeFixture(t, f.root, "part(a).go", "text")
	body := "[escaped](" + strings.ReplaceAll(strings.ReplaceAll(paren, "(", `\(`), ")", `\)`) + ")"
	if got, err := f.s.Rewrite(context.Background(), body); err != nil || !strings.Contains(got, "/f/") {
		t.Fatal("escaped destination punctuation was not parsed")
	}
}

func TestRewriteLocalLineFragments(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, "main.go", "package main\n")
	base := registeredURL(t, f.s, path)
	for _, target := range []string{path, "file://" + path, "http://" + strings.TrimPrefix(path, "/")} {
		for _, fragment := range []string{"#L12", "#L12C5", "#L0012C05"} {
			body := "[source](" + target + fragment + ")"
			got, err := f.s.Rewrite(context.Background(), body)
			if want := "[source · L12](" + base + "#L12)"; err != nil || got != want {
				t.Fatalf("rewrite = %q, want %q, error %v", got, want, err)
			}
		}
	}
	local := "[source L12](" + path + "#L12C5)"
	if got, err := f.s.Rewrite(context.Background(), local); err != nil || got != "[source L12]("+base+"#L12)" {
		t.Fatalf("existing line label changed: %q (error %v)", got, err)
	}
	unchanged := []string{
		"`" + local + "`", "```text\n" + local + "\n```", "    " + local,
		"![source](" + path + "#L12)",
		"[website](https://example.invalid" + path + "#L12)",
		"[website](http://example.invalid" + path + "#L12C5)",
		"[outside](" + writeFixture(t, t.TempDir(), "outside.go", "outside") + "#L12)",
	}
	for _, fragment := range []string{"#section", "#L0", "#L-1", "#L12C0", "#L12C", "#L12C5C6", "#L4294967296", "#L12C4294967296"} {
		unchanged = append(unchanged, "[source]("+path+fragment+")")
	}
	// A destination with two different location syntaxes is ambiguous.
	unchanged = append(unchanged, "[source]("+path+":8#L12)")
	for _, body := range unchanged {
		if got, err := f.s.Rewrite(context.Background(), body); err != nil || got != body {
			t.Fatalf("ineligible fragment link changed: %q (error %v)", got, err)
		}
	}
}

func TestUnpreviewableLinksRemainUnchanged(t *testing.T) {
	f := newFixture(t)
	f.s.maxBytes = 64
	outside := writeFixture(t, t.TempDir(), "outside.go", "outside")
	paths := []string{
		outside,
		filepath.Join(f.root, "missing.go"),
		writeFixture(t, f.root, ".private/source.go", "private"),
		writeFixture(t, f.root, "credentials.json", "credential fixture"),
		writeFixture(t, f.root, "sub/secrets.json", "secret fixture"),
		writeFixture(t, f.root, "normal.png", "not an allowed extension"),
		writeFixture(t, f.root, "binary.go", "binary\x00data"),
		writeFixture(t, f.root, "invalid.go", "invalid\xffutf8"),
		writeFixture(t, f.root, "large.go", strings.Repeat("x", 65)),
		f.root,
	}
	paths = append(paths, f.root+"/../workspace/binary.go")
	if err := os.Symlink(outside, filepath.Join(f.root, "escape.go")); err != nil {
		t.Fatal(err)
	}
	paths = append(paths, filepath.Join(f.root, "escape.go"))
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(f.root, "escape")); err != nil {
		t.Fatal(err)
	}
	paths = append(paths, filepath.Join(f.root, "escape", "outside.go"))
	for _, path := range paths {
		body := "[file](<" + path + ">)"
		got, err := f.s.Rewrite(context.Background(), body)
		if err != nil || got != body {
			t.Fatalf("ineligible path did not remain unchanged (error %v)", err)
		}
	}
}

func TestRewriteRegistryFailureDoesNotReturnURL(t *testing.T) {
	f := newFixture(t)
	path := writeFixture(t, f.root, "main.go", "package main")
	id := f.s.recordID(fileRecord{Version: 1, Root: f.root, Relative: "main.go"})
	if err := os.Mkdir(filepath.Join(f.opts.Directory, id+".json"), 0700); err != nil {
		t.Fatal(err)
	}
	body := "[file](" + path + ")"
	got, err := f.s.Rewrite(context.Background(), body)
	if err == nil || got != body || strings.Contains(err.Error(), f.root) || strings.Contains(err.Error(), f.opts.Directory) {
		t.Fatal("registry failure was hidden, returned a URL, or leaked a path")
	}
	if err := os.Remove(filepath.Join(f.opts.Directory, id+".json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.opts.Directory, 0755); err != nil {
		t.Fatal(err)
	}
	if got, err = f.s.Rewrite(context.Background(), body); err == nil || got != body {
		t.Fatal("insecure registry permissions were ignored")
	}
	if err := os.Chmod(f.opts.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err = f.s.Rewrite(ctx, body); err != context.Canceled || got != body {
		t.Fatal("cancelled rewrite unexpectedly registered a link")
	}
}
