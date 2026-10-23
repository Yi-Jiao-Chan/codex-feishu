package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/store"
	"golang.org/x/sys/unix"
)

func taskIconsFixture() cards.TaskIcons {
	return cards.TaskIcons{Toolbox: "img_fixture_toolbox", Command: "img_fixture_command", Read: "img_fixture_read", Edit: "img_fixture_edit", Search: "img_fixture_search", Generic: "img_fixture_generic"}
}

func taskIconsManifest(t *testing.T, appID string, icons cards.TaskIcons) []byte {
	t.Helper()
	manifest := taskIconManifest{Version: 1, AppID: appID}
	manifest.Icons.Toolbox, manifest.Icons.Command = icons.Toolbox, icons.Command
	manifest.Icons.Read, manifest.Icons.Edit = icons.Read, icons.Edit
	manifest.Icons.Search, manifest.Icons.Generic = icons.Search, icons.Generic
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func taskIconsDatabase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(dir, "bridge.db")
	if err := os.WriteFile(database, nil, 0600); err != nil {
		t.Fatal(err)
	}
	return database
}

func writeTaskIconsManifest(t *testing.T, database, appID string, icons cards.TaskIcons) string {
	t.Helper()
	path := database + ".task-icons.json"
	if err := os.WriteFile(path, taskIconsManifest(t, appID, icons), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadTaskIconsMissingAndCanonicalDatabasePaths(t *testing.T) {
	database := taskIconsDatabase(t)
	if icons, err := loadTaskIcons(database, "cli_fixture"); err != nil || icons != (cards.TaskIcons{}) {
		t.Fatal("missing optional manifest did not silently retain native icons", err)
	}
	want := taskIconsFixture()
	writeTaskIconsManifest(t, database, "cli_fixture", want)
	alias := filepath.Join(t.TempDir(), "database-alias")
	if err := os.Symlink(database, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{database, alias} {
		if icons, err := loadTaskIcons(path, "cli_fixture"); err != nil || icons != want {
			t.Fatal("database aliases did not use the same app-scoped manifest", err)
		}
	}
	if err := os.Remove(database); err != nil {
		t.Fatal(err)
	}
	if icons, err := loadTaskIcons(database, "cli_fixture"); err != nil || icons != want {
		t.Fatal("fresh database did not resolve its existing private deployment directory", err)
	}
}

func TestLoadTaskIconsRejectsInvalidManifestAtomically(t *testing.T) {
	valid := string(taskIconsManifest(t, "cli_fixture", taskIconsFixture()))
	cases := map[string]string{
		"bad_json":       "{private-fixture-key",
		"trailing_json":  valid + "{}",
		"unknown_field":  strings.Replace(valid, `"version":1`, `"version":1,"private_field":"private-value"`, 1),
		"unknown_icon":   strings.Replace(valid, `"toolbox":`, `"other":"img_fixture_other","toolbox":`, 1),
		"wrong_version":  strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"foreign_app":    strings.Replace(valid, "cli_fixture", "cli_other_app", 1),
		"missing_icons":  `{"version":1,"app_id":"cli_fixture"}`,
		"null_manifest":  "null",
		"oversized_file": valid + strings.Repeat(" ", maxTaskIconsBytes),
	}
	for _, role := range []string{"toolbox", "command", "read", "edit", "search", "generic"} {
		for name, invalid := range map[string]string{"empty": "", "prefix_only": "img_", "bad_prefix": "file_fixture", "url": "https://example.invalid/icon", "path": "img_../fixture", "unicode": "img_图片", "space": "img_fixture key", "too_long": "img_" + strings.Repeat("a", 509)} {
			cases[role+"_"+name] = strings.Replace(valid, "img_fixture_"+role, invalid, 1)
		}
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			database := taskIconsDatabase(t)
			if err := os.WriteFile(database+".task-icons.json", []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			icons, err := loadTaskIcons(database, "cli_fixture")
			if !errors.Is(err, errTaskIconsUnavailable) || err.Error() != "task icons unavailable" || icons != (cards.TaskIcons{}) {
				t.Fatal("invalid manifest retained partial icons or exposed unsafe diagnostics")
			}
		})
	}
}

func TestLoadTaskIconsAcceptsMaximumKeyAndManifestSizes(t *testing.T) {
	database := taskIconsDatabase(t)
	want := taskIconsFixture()
	want.Toolbox = "img_" + strings.Repeat("A_0-", 127)
	body := taskIconsManifest(t, "cli_fixture", want)
	body = append(body, []byte(strings.Repeat(" ", maxTaskIconsBytes-len(body)))...)
	if err := os.WriteFile(database+".task-icons.json", body, 0600); err != nil {
		t.Fatal(err)
	}
	if icons, err := loadTaskIcons(database, "cli_fixture"); err != nil || icons != want {
		t.Fatal("valid boundary-sized manifest or image key was rejected", err)
	}
}

func TestLoadTaskIconsRejectsUnsafeFilesAndDirectories(t *testing.T) {
	cases := map[string]func(*testing.T, string){
		"mode_0644":   func(t *testing.T, path string) { taskIconsChmod(t, path, 0644) },
		"mode_0400":   func(t *testing.T, path string) { taskIconsChmod(t, path, 0400) },
		"parent_0755": func(t *testing.T, path string) { taskIconsChmod(t, filepath.Dir(path), 0755) },
		"directory": func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, path string) {
			if err := os.Rename(path, path+".private-target"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+".private-target", path); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"foreign_owner": func(t *testing.T, path string) {
			if err := os.Chown(path, os.Geteuid()+1, -1); errors.Is(err, os.ErrPermission) {
				t.Skip("changing file ownership requires privilege")
			} else if err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			database := taskIconsDatabase(t)
			path := writeTaskIconsManifest(t, database, "cli_fixture", taskIconsFixture())
			mutate(t, path)
			if icons, err := loadTaskIcons(database, "cli_fixture"); !errors.Is(err, errTaskIconsUnavailable) || icons != (cards.TaskIcons{}) {
				t.Fatal("unsafe manifest or directory did not fall back atomically")
			}
		})
	}
}

func taskIconsChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func taskIconsProgress() cards.ProgressView {
	return cards.ProgressView{Records: []cards.ProgressRecord{{ItemID: "tools", Tools: &cards.ProgressTools{Completed: 5, Items: []cards.ProgressTool{
		{ItemID: "read", Kind: "read", Status: "completed", Title: "Read fixture"},
		{ItemID: "edit", Kind: "edit", Status: "completed", Title: "Edit fixture"},
		{ItemID: "search", Kind: "search", Status: "completed", Title: "Search fixture"},
		{ItemID: "command", Kind: "command", Status: "completed", Title: "Run fixture"},
		{ItemID: "generic", Kind: "other", Status: "completed", Title: "Other fixture"},
	}}}}}
}

func assertTaskIconsRendered(t *testing.T, card string, icons cards.TaskIcons) {
	t.Helper()
	if !json.Valid([]byte(card)) {
		t.Fatal("progress card is not valid JSON")
	}
	for _, key := range []string{icons.Toolbox, icons.Command, icons.Read, icons.Edit, icons.Search, icons.Generic} {
		if !strings.Contains(card, `"img_key":"`+key+`"`) {
			t.Fatal("progress card lost a configured icon")
		}
	}
}

func TestTaskControlsInjectsIconsIntoPreparedAndFallbackRender(t *testing.T) {
	icons := taskIconsFixture()
	controls := &TaskControls{Icons: icons}
	progress := taskIconsProgress()
	assertTaskIconsRendered(t, controls.Render("thread", "turn", "inProgress", progress), icons)
	controls.cache = map[taskRenderKey]store.TaskControl{{"thread", "turn"}: {NativeStatus: "inProgress", Kind: "stop", State: "available", Token: "fixture-token", Revision: 1}}
	prepared := controls.Render("thread", "turn", "inProgress", progress)
	assertTaskIconsRendered(t, prepared, icons)
	if !strings.Contains(prepared, `"action":"stop"`) {
		t.Fatal("adding icons disabled prepared task controls")
	}
	if card := controls.Render("thread", "missing", "interrupted", progress); card != "" {
		t.Fatal("icons created an unrelated interrupted task card")
	}
}

func TestServiceLoadsTaskIconsBeforeWorkersAndReusesAcrossGenerations(t *testing.T) {
	db, cfg, root := serviceFixture(t)
	taskIconsChmod(t, filepath.Dir(cfg.Storage.DatabaseFile), 0700)
	want := taskIconsFixture()
	path := writeTaskIconsManifest(t, cfg.Storage.DatabaseFile, cfg.Feishu.AppID, want)
	var notices []string
	s := &Service{Config: cfg, Store: db, Gateway: &serviceGateway{}, BotID: "ou_bot", Notice: func(code string) { notices = append(notices, code) }, Connect: func(context.Context) (ServiceNative, error) {
		t.Fatal("startup test attempted a native connection")
		return nil, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); !errors.Is(err, context.Canceled) || s.taskIcons != want || len(notices) != 0 {
		t.Fatalf("service did not load optional icons before its workers: error=%v, icons_match=%t, notices=%v", err, s.taskIcons == want, notices)
	}
	first := s.generation(serviceTestNative(root))
	assertTaskIconsRendered(t, first.RenderProgress("thread", "turn", "inProgress", taskIconsProgress()), want)
	if err := os.WriteFile(path, []byte("invalid replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	second := s.generation(serviceTestNative(root))
	assertTaskIconsRendered(t, second.RenderProgress("thread", "turn", "inProgress", taskIconsProgress()), want)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertTaskIconsRendered(t, second.RenderProgress("thread", "turn", "inProgress", taskIconsProgress()), want)
	if len(notices) != 0 {
		t.Fatal("rendering or native reconnection reloaded the manifest")
	}
}

func TestServiceTaskIconFailureUsesSafeNoticeAndNativeFallback(t *testing.T) {
	database := taskIconsDatabase(t)
	var notices []string
	s := &Service{Notice: func(code string) { notices = append(notices, code) }}
	s.Config.Storage.DatabaseFile, s.Config.Feishu.AppID = database, "cli_fixture"
	s.loadTaskIcons()
	if len(notices) != 0 || s.taskIcons != (cards.TaskIcons{}) {
		t.Fatal("missing optional icons emitted a notice")
	}
	writeTaskIconsManifest(t, database, "cli_other_app", taskIconsFixture())
	s.taskIcons = taskIconsFixture()
	s.loadTaskIcons()
	if len(notices) != 1 || notices[0] != "task_icons.unavailable" || s.taskIcons != (cards.TaskIcons{}) {
		t.Fatal("invalid optional icons retained stale keys or exposed unsafe diagnostics")
	}
	render := (&TaskControls{Icons: s.taskIcons}).Render("thread", "turn", "inProgress", taskIconsProgress())
	if strings.Contains(render, `"tag":"custom_icon"`) || !strings.Contains(render, `"tag":"standard_icon"`) {
		t.Fatal("invalid manifest did not retain native icon rendering")
	}
}
