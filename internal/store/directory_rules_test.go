package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"codex-feishu/internal/rules"
)

func directoryStoreFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "directory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.BeginBaseline(context.Background(), "host", "app", 1); err != nil {
		t.Fatal(err)
	}
	return s, t.TempDir()
}

func TestDirectoryRulesExactRecursiveOverlapAndSymlink(t *testing.T) {
	s, project := directoryStoreFixture(t)
	ctx := context.Background()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"/mute-dir " + alias, "/mute-dir " + alias + " --recursive", "/mute-dir " + project} {
		cmd, err := rules.ParseDirectoryCommand(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ApplyDirectoryCommand(ctx, "host", "app", cmd); err != nil {
			t.Fatal(err)
		}
	}
	want := []rules.DirectoryRule{{Path: project}, {Path: project, Recursive: true}}
	got, err := s.DirectoryRules(ctx, "host", "app")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("overlap/dedup: %+v %v", got, err)
	}
	unmute, err := rules.ParseDirectoryCommand("/unmute-dir " + alias)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.ApplyDirectoryCommand(ctx, "host", "app", unmute); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.DirectoryRules(ctx, "host", "app")
	if err != nil || !reflect.DeepEqual(got, want[1:]) {
		t.Fatalf("exact removal changed recursive rule: %+v %v", got, err)
	}
	if _, muted := rules.Muted(rules.Scope{CWD: filepath.Join(project, "child")}, got); !muted {
		t.Fatal("remaining recursive rule stopped matching")
	}
	if _, muted := rules.Muted(rules.Scope{CWD: project + "suffix"}, got); muted {
		t.Fatal("persisted recursive rule widened to a prefix collision")
	}
	unmute.Rule.Recursive = true
	if err := s.ApplyDirectoryCommand(ctx, "host", "app", unmute); err != nil {
		t.Fatal(err)
	}
	got, err = s.DirectoryRules(ctx, "host", "app")
	if err != nil || len(got) != 0 {
		t.Fatalf("recursive removal: %+v %v", got, err)
	}
}

func TestDirectoryRulesVerifyDeploymentAndIsolateHosts(t *testing.T) {
	s, project := directoryStoreFixture(t)
	ctx := context.Background()
	if _, err := s.BeginBaseline(ctx, "other-host", "other-app", 1); err != nil {
		t.Fatal(err)
	}
	cmd := rules.DirectoryCommand{Action: rules.DirectoryMute, Rule: rules.DirectoryRule{Path: project}}
	for _, key := range [][2]string{{"host", "wrong-app"}, {"other-host", "app"}, {"missing", "app"}} {
		if err := s.ApplyDirectoryCommand(ctx, key[0], key[1], cmd); !errors.Is(err, ErrConflict) {
			t.Errorf("invalid deployment mutation %v: %v", key, err)
		}
		if _, err := s.DirectoryRules(ctx, key[0], key[1]); !errors.Is(err, ErrConflict) {
			t.Errorf("invalid deployment read %v: %v", key, err)
		}
	}
	if err := s.ApplyDirectoryCommand(ctx, "host", "app", cmd); err != nil {
		t.Fatal(err)
	}
	other, err := s.DirectoryRules(ctx, "other-host", "other-app")
	if err != nil || len(other) != 0 {
		t.Fatalf("host rules leaked: %+v %v", other, err)
	}
	cmd.Rule.Recursive = true
	if err := s.ApplyDirectoryCommand(ctx, "other-host", "other-app", cmd); err != nil {
		t.Fatal(err)
	}
	got, err := s.DirectoryRules(ctx, "host", "app")
	if err != nil || !reflect.DeepEqual(got, []rules.DirectoryRule{{Path: project}}) {
		t.Fatalf("other deployment altered original: %+v %v", got, err)
	}
}

func TestDirectoryRulesSurviveReopenAndMissingDirectory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "directory.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	if _, err := s.BeginBaseline(ctx, "host", "app", 1); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	cmd := rules.DirectoryCommand{Action: rules.DirectoryMute, Rule: rules.DirectoryRule{Path: project, Recursive: true}}
	if err := s.ApplyDirectoryCommand(ctx, "host", "app", cmd); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(project); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.DirectoryRules(ctx, "host", "app")
	if err != nil || !reflect.DeepEqual(got, []rules.DirectoryRule{cmd.Rule}) {
		t.Fatalf("reopen or removed directory erased rule: %+v %v", got, err)
	}
	cmd.Action = rules.DirectoryUnmute
	if err := s.ApplyDirectoryCommand(ctx, "host", "app", cmd); err == nil {
		t.Fatal("missing directory mutation accepted")
	}
}

func TestDirectoryRulesInvalidCommandsDoNotWrite(t *testing.T) {
	s, project := directoryStoreFixture(t)
	ctx := context.Background()
	file := filepath.Join(project, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(project, "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []rules.DirectoryCommand{
		{Action: "unknown", Rule: rules.DirectoryRule{Path: project}},
		{Action: rules.DirectoryMute},
		{Action: rules.DirectoryMute, Rule: rules.DirectoryRule{Path: "relative"}},
		{Action: rules.DirectoryMute, Rule: rules.DirectoryRule{Path: file}},
		{Action: rules.DirectoryMute, Rule: rules.DirectoryRule{Path: alias}},
		{Action: rules.DirectoryUnmute, Rule: rules.DirectoryRule{Path: filepath.Join(project, "missing")}},
		{Action: rules.DirectoryList, Rule: rules.DirectoryRule{Path: project}},
	} {
		if err := s.ApplyDirectoryCommand(ctx, "host", "app", cmd); err == nil {
			t.Errorf("invalid command accepted: %+v", cmd)
		}
	}
	valid := rules.DirectoryCommand{Action: rules.DirectoryMute, Rule: rules.DirectoryRule{Path: project}}
	for _, key := range [][2]string{{"", "app"}, {"host", ""}, {" ", "app"}} {
		if err := s.ApplyDirectoryCommand(ctx, key[0], key[1], valid); err == nil {
			t.Errorf("empty deployment mutation accepted: %q", key)
		}
		if _, err := s.DirectoryRules(ctx, key[0], key[1]); err == nil {
			t.Errorf("empty deployment read accepted: %q", key)
		}
	}
	if err := s.ApplyDirectoryCommand(ctx, "host", "app", rules.DirectoryCommand{Action: rules.DirectoryList}); err != nil {
		t.Fatal(err)
	}
	got, err := s.DirectoryRules(ctx, "host", "app")
	if err != nil || len(got) != 0 {
		t.Fatalf("invalid/list commands wrote rules: %+v %v", got, err)
	}
}

func TestDirectoryCommandRejectsRetargetedCanonicalPath(t *testing.T) {
	s, base := directoryStoreFixture(t)
	ctx := context.Background()
	project := filepath.Join(base, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	cmd, err := rules.ParseDirectoryCommand("/mute-dir " + project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(project); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), project); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyDirectoryCommand(ctx, "host", "app", cmd); err == nil {
		t.Fatal("storage silently widened the previously validated path")
	}
	got, err := s.DirectoryRules(ctx, "host", "app")
	if err != nil || len(got) != 0 {
		t.Fatalf("retargeted path wrote a rule: %+v %v", got, err)
	}
}
