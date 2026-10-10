package main

import (
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Read only the bound M0 lineage. Never print previews, names or item bodies.
func checkClassification(ctx context.Context, c *codexclient.Client, cfg config.Config, root string) {
	fail := func(err error) { reportError(err); os.Exit(4) }
	threads, err := c.List(ctx)
	if err != nil {
		fail(err)
	}
	archivedThreads, err := archivedClassificationThreads(ctx, c)
	if err != nil {
		fail(err)
	}
	archivedIDs := map[string]bool{}
	for _, thread := range archivedThreads {
		archivedIDs[thread.ID] = true
	}
	threads = append(threads, archivedThreads...)
	selected := map[string]bool{root: true}
	for changed := true; changed; {
		changed = false
		for _, thread := range threads {
			if thread.ForkedFromID != nil && selected[*thread.ForkedFromID] && !selected[thread.ID] {
				selected[thread.ID], changed = true, true
			}
		}
	}
	var records []map[string]any
	for _, thread := range threads {
		if !selected[thread.ID] {
			continue
		}
		turns, e := c.Turns(ctx, thread.ID)
		if e != nil {
			fail(e)
		}
		userItems := 0
		for _, turn := range turns {
			for _, item := range turn.Items {
				if item.Type == "userMessage" {
					userItems++
				}
			}
		}
		record := classificationRecord(cfg, thread)
		record["archived"] = archivedIDs[thread.ID]
		record["user_item_count"] = userItems
		record["git_common_dir"] = gitMetadata(ctx, thread.CWD, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if thread.ID != root {
			common := record["git_common_dir"].(string)
			record["same_allowed_repository"] = false
			for _, allowed := range cfg.Discovery.AllowedRoots {
				if common != "" && common == gitMetadata(ctx, allowed, "rev-parse", "--path-format=absolute", "--git-common-dir") {
					record["same_allowed_repository"] = true
				}
			}
		}
		records = append(records, record)
		for _, archived := range []bool{false, true} {
			cursor := ""
			seen := map[string]bool{}
			for {
				var page struct {
					Data       []codexclient.Thread `json:"data"`
					NextCursor *string              `json:"nextCursor"`
				}
				params := map[string]any{"ancestorThreadId": thread.ID, "sourceKinds": []string{"subAgent"}, "archived": archived, "limit": 100, "useStateDbOnly": true}
				if cursor != "" {
					params["cursor"] = cursor
				}
				if e = c.Call(ctx, "thread/list", params, &page); e != nil {
					fail(e)
				}
				for _, child := range page.Data {
					record := classificationRecord(cfg, child)
					record["archived"] = archived
					records = append(records, record)
				}
				if page.NextCursor == nil || *page.NextCursor == "" {
					break
				}
				cursor = *page.NextCursor
				if seen[cursor] {
					fail(errors.New("repeated descendants cursor"))
				}
				seen[cursor] = true
			}
		}
	}
	if err = json.NewEncoder(os.Stdout).Encode(map[string]any{"bound_test_thread": root, "metadata": records, "subagent_scope": "spawned descendants of bound test and its forks; active and archived", "app_ui_verified_by_metadata": false}); err != nil {
		os.Exit(4)
	}
}

// Fork lineage can be archived in App before the operator reports its ID.
// Read metadata through the existing daemon; never resume archived threads.
func archivedClassificationThreads(ctx context.Context, c *codexclient.Client) ([]codexclient.Thread, error) {
	return classificationPages(func(cursor string) ([]codexclient.Thread, *string, error) {
		var result struct {
			Data       []codexclient.Thread `json:"data"`
			NextCursor *string              `json:"nextCursor"`
		}
		params := map[string]any{"archived": true, "limit": 100, "useStateDbOnly": true}
		if cursor != "" {
			params["cursor"] = cursor
		}
		err := c.Call(ctx, "thread/list", params, &result)
		return result.Data, result.NextCursor, err
	})
}

func classificationPages(fetch func(string) ([]codexclient.Thread, *string, error)) ([]codexclient.Thread, error) {
	var threads []codexclient.Thread
	cursor := ""
	seen := map[string]bool{}
	for {
		data, next, err := fetch(cursor)
		if err != nil {
			return nil, err
		}
		threads = append(threads, data...)
		if next == nil || *next == "" {
			return threads, nil
		}
		if seen[*next] {
			return nil, errors.New("repeated archived classification cursor")
		}
		cursor, seen[*next] = *next, true
	}
}

func classificationRecord(cfg config.Config, thread codexclient.Thread) map[string]any {
	return map[string]any{"thread_id": thread.ID, "forked_from_id": thread.ForkedFromID, "parent_thread_id": thread.ParentThreadID, "source": thread.Source, "thread_source": thread.ThreadSource, "originator": thread.Originator, "cwd": thread.CWD, "persistent_interactive_candidate": thread.InteractiveMain(), "physical_allowed_root": cfg.Allows(thread.CWD)}
}

func gitMetadata(ctx context.Context, cwd string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if !strings.HasPrefix(key, "GIT_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(out))
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return path
}
