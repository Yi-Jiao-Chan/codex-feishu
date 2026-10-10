package main

import (
	"codex-feishu/internal/codexclient"
	"errors"
	"testing"
)

func TestArchivedClassificationDoesNotStopAtEmptyPage(t *testing.T) {
	next := "opaque-next"
	var cursors []string
	threads, err := classificationPages(func(cursor string) ([]codexclient.Thread, *string, error) {
		cursors = append(cursors, cursor)
		if cursor == "" {
			return nil, &next, nil
		}
		return []codexclient.Thread{{ID: "archived-human-fork"}}, nil, nil
	})
	if err != nil || len(threads) != 1 || threads[0].ID != "archived-human-fork" || len(cursors) != 2 || cursors[1] != next {
		t.Fatalf("archived metadata truncated: %v", err)
	}
}

func TestArchivedClassificationFailsOnCursorLoopAndReadError(t *testing.T) {
	next := "same"
	_, err := classificationPages(func(string) ([]codexclient.Thread, *string, error) { return nil, &next, nil })
	if err == nil {
		t.Fatal("cursor loop accepted")
	}
	rejected := errors.New("fixture read rejected")
	_, err = classificationPages(func(string) ([]codexclient.Thread, *string, error) { return nil, nil, rejected })
	if !errors.Is(err, rejected) {
		t.Fatal("read failure swallowed")
	}
}
