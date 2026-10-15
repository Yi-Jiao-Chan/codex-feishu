package store

import (
	"context"
	"testing"
)

func TestGroupDescriptionMigrationPreservesAttemptedRequest(t *testing.T) {
	ctx := context.Background()
	s, path := discoveryStore(t)
	ids := map[string]string{}
	for _, thread := range []string{"unsent", "limited", "unknown"} {
		eligible(t, s, thread)
		i, err := s.PrepareGroup(ctx, "host", thread, "owner", "name", []string{"owner"})
		if err != nil {
			t.Fatal(err)
		}
		ids[thread] = i.ID
		if thread == "unsent" {
			continue
		}
		a, claimed, err := s.ClaimGroupAttempt(ctx, i.ID, 1000)
		if err != nil || !claimed {
			t.Fatal("no dispatch", err)
		}
		if thread == "limited" {
			err = s.DeferGroupAttempt(ctx, a, 1000, 1000)
		} else {
			err = s.FinishGroupDispatch(ctx, a, "unknown", "")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	// Reproduce schema 22's absence of a description column, including a
	// ready request with a definite rate-limit rejection and an unknown one.
	if _, err := s.DB.Exec(`ALTER TABLE create_intents DROP COLUMN description; PRAGMA user_version=22;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, thread := range []string{"unsent", "limited", "unknown"} {
		i, err := reopened.GroupIntent(ctx, ids[thread])
		if err != nil {
			t.Fatal(err)
		}
		want := "跟进同一个 Codex 会话的任务与公开回复"
		if thread == "unsent" {
			want = ""
		}
		if i.Description != want {
			t.Fatalf("migration changed %s request: %+v", thread, i)
		}
	}
	for _, tc := range []struct{ thread, want string }{
		{"unsent", "工作目录：/actual/worktree"},
		{"limited", "跟进同一个 Codex 会话的任务与公开回复"},
	} {
		for _, candidate := range []string{"工作目录：/actual/worktree", "工作目录：/changed"} {
			got, err := reopened.FreezeGroupDescription(ctx, ids[tc.thread], candidate)
			if err != nil || got != tc.want {
				t.Fatalf("description not frozen for %s: %q %v", tc.thread, got, err)
			}
		}
	}
	unknown, err := reopened.GroupIntent(ctx, ids["unknown"])
	if err != nil || unknown.State != "unknown" {
		t.Fatal("migration reopened unknown request", err)
	}
}
