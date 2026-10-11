package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"codex-feishu/internal/store"
)

func TestProbeCannotAliasConfiguredDatabase(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "source.db")
	if err := os.WriteFile(original, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hardlink", "symlink"} {
		alias := filepath.Join(dir, name)
		var err error
		if name == "hardlink" {
			err = os.Link(original, alias)
		} else {
			err = os.Symlink(original, alias)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !samePath(alias, original) {
			t.Fatalf("database alias not recognized: %s", name)
		}
	}
	parentAlias := filepath.Join(t.TempDir(), "parent-alias")
	if err := os.Symlink(dir, parentAlias); err != nil {
		t.Fatal(err)
	}
	if !samePath(filepath.Join(dir, "not-created.db"), filepath.Join(parentAlias, "not-created.db")) {
		t.Fatal("future database through aliased parent not recognized")
	}
}

func TestReceiptIdentityImportDoesNotReadBodiesOrMigrateSource(t *testing.T) {
	ctx := context.Background()
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	source, err := store.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	r, _, err := source.Claim(ctx, store.Receipt{AppID: "app", ThreadID: "thread", MessageID: "original", ChatID: "chat", Body: "body-must-not-be-copied"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.Claim(ctx, store.Receipt{AppID: "other-app", ThreadID: "thread", MessageID: "other", Body: "foreign-body"}); err != nil {
		t.Fatal(err)
	}
	// The import is a raw read-only connection, not Store.Open on this source.
	if _, err := source.DB.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	dest, err := store.Open(filepath.Join(t.TempDir(), "dest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	for i := 0; i < 2; i++ {
		copied, err := copyReceiptIdentities(ctx, sourcePath, dest, "app", "thread")
		if err != nil || copied != 1 {
			t.Fatalf("import: %d %v", copied, err)
		}
	}
	var version int
	var body string
	if err := source.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("source database migrated by read-only probe: %d %v", version, err)
	}
	if err := source.DB.QueryRow(`SELECT body FROM m0_receipts WHERE id=?`, r.ID).Scan(&body); err != nil || body != "body-must-not-be-copied" {
		t.Fatal("source receipt changed")
	}
	got, err := dest.Receipts(ctx, "thread")
	if err != nil || len(got) != 1 || got[0].Body != "" || got[0].ID != r.ID || got[0].ClientID != r.ClientID {
		t.Fatalf("body/foreign input copied or identity altered: %+v %v", got, err)
	}
}

func TestReceiptImportMissingSourceNeverCreatesDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	dest, err := store.Open(filepath.Join(t.TempDir(), "dest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if _, err := copyReceiptIdentities(context.Background(), missing, dest, "app", "thread"); err == nil {
		t.Fatal("missing read-only source opened")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("read-only import created a source file")
	}
}
