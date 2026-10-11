package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func backupDigest(t *testing.T, s *Store) string {
	t.Helper()
	i, err := s.LegacyInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return i.Digest()
}
func backupDestination(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "backup # ' ?.sqlite")
}
func assertNoBackup(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("backup published unexpectedly", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".legacy-backup-") {
			t.Fatal("temporary backup retained")
		}
	}
}

func TestBackupLegacyIncludesCommittedLiveWALWithoutChangingSource(t *testing.T) {
	s, path := legacyStore(t)
	ctx := context.Background()
	if _, err := s.DB.Exec(`PRAGMA journal_mode=WAL;PRAGMA wal_autocheckpoint=0;UPDATE m0_receipts SET body='committed WAL fixture';`); err != nil {
		t.Fatal(err)
	}
	before, err := s.LegacyInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mainBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	walBefore, err := os.ReadFile(path + "-wal")
	if err != nil || len(walBefore) == 0 {
		t.Fatal("fixture did not retain WAL", err)
	}
	target := backupDestination(t)
	if err := s.BackupLegacy(ctx, target, before.Digest()); err != nil {
		t.Fatal(err)
	}
	backup, schema, err := OpenReadOnly(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	inventory, err := backup.LegacyInventory(ctx)
	if err != nil || schema != 1 || inventory.Digest() != before.Digest() || inventory.Receipts[0].Body != "committed WAL fixture" {
		t.Fatal("WAL snapshot incorrect", err)
	}
	after, err := s.LegacyInventory(ctx)
	if err != nil || after.Digest() != before.Digest() {
		t.Fatal("source changed", err)
	}
	mainAfter, _ := os.ReadFile(path)
	walAfter, _ := os.ReadFile(path + "-wal")
	if string(mainBefore) != string(mainAfter) || string(walBefore) != string(walAfter) {
		t.Fatal("source was modified/checkpointed")
	}
	for _, file := range []string{path, target} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("private mode changed", err)
		}
	}
	var mode string
	if err := s.DB.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatal("source journal mode changed", err)
	}
}

func TestBackupLegacyExistingArtifactIsVerifiedAndNeverReplaced(t *testing.T) {
	s, _ := legacyStore(t)
	ctx := context.Background()
	digest := backupDigest(t, s)
	target := backupDestination(t)
	if err := s.BackupLegacy(ctx, target, digest); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BackupLegacy(ctx, target, digest); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(target)
	current, _ := os.ReadFile(target)
	if !os.SameFile(before, after) || string(data) != string(current) {
		t.Fatal("idempotent backup replaced artifact")
	}
	// A new source inventory cannot turn an existing artifact into a replacement.
	if _, err := s.DB.Exec(`UPDATE m0_receipts SET body='different'`); err != nil {
		t.Fatal(err)
	}
	if err := s.BackupLegacy(ctx, target, backupDigest(t, s)); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting backup accepted", err)
	}
	after, _ = os.Stat(target)
	current, _ = os.ReadFile(target)
	if !os.SameFile(before, after) || string(data) != string(current) {
		t.Fatal("conflicting backup overwritten")
	}
}

func TestBackupLegacyRejectsUnsafePathsAndConflicts(t *testing.T) {
	s, source := legacyStore(t)
	digest := backupDigest(t, s)
	ctx := context.Background()
	cases := []string{"relative", "unclean", "source", "public directory", "directory symlink", "target symlink", "public target", "existing non-database", "hardlinked target", "fifo target"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			target := backupDestination(t)
			switch name {
			case "relative":
				target = "relative.db"
			case "unclean":
				target = filepath.Dir(target) + "/sub/../backup.db"
			case "source":
				target = source
			case "public directory":
				if err := os.Chmod(filepath.Dir(target), 0750); err != nil {
					t.Fatal(err)
				}
			case "directory symlink":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(filepath.Dir(target), link); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(link, "backup.db")
			case "target symlink":
				if err := os.Symlink(source, target); err != nil {
					t.Fatal(err)
				}
			case "public target":
				if err := os.WriteFile(target, []byte("existing"), 0644); err != nil {
					t.Fatal(err)
				}
			case "existing non-database":
				if err := os.WriteFile(target, []byte("private marker must not leak"), 0600); err != nil {
					t.Fatal(err)
				}
			case "fifo target":
				if err := unix.Mkfifo(target, 0600); err != nil {
					t.Fatal(err)
				}
			case "hardlinked target":
				if err := os.Link(source, target); err != nil {
					t.Fatal(err)
				}
			}
			err := s.BackupLegacy(ctx, target, digest)
			if err == nil || strings.Contains(err.Error(), "private marker") || strings.Contains(err.Error(), "private body") {
				t.Fatal("unsafe destination accepted or leaked", err)
			}
			if backupDigest(t, s) != digest {
				t.Fatal("unsafe path changed source")
			}
		})
	}
}

func TestBackupLegacyRejectsForeignOwners(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing ownership requires root")
	}
	s, _ := legacyStore(t)
	digest := backupDigest(t, s)
	for _, name := range []string{"directory", "file"} {
		t.Run(name, func(t *testing.T) {
			target := backupDestination(t)
			owned := filepath.Dir(target)
			if name == "file" {
				if err := os.WriteFile(target, []byte("unowned"), 0600); err != nil {
					t.Fatal(err)
				}
				owned = target
			}
			if err := os.Chown(owned, 1, -1); err != nil {
				t.Fatal(err)
			}
			defer os.Chown(owned, 0, -1)
			if err := s.BackupLegacy(context.Background(), target, digest); !errors.Is(err, ErrConflict) {
				t.Fatal("foreign owner accepted", err)
			}
		})
	}
}

func TestBackupLegacyWrongDigestAndCanceledContextNeverPublish(t *testing.T) {
	s, _ := legacyStore(t)
	target := backupDestination(t)
	if err := s.BackupLegacy(context.Background(), target, strings.Repeat("b", 64)); !errors.Is(err, ErrConflict) {
		t.Fatal("wrong source accepted", err)
	}
	assertNoBackup(t, target)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.BackupLegacy(ctx, target, backupDigest(t, s)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	assertNoBackup(t, target)
}

func backupLargeFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	s, path := legacyStore(t)
	if _, err := s.DB.Exec(`PRAGMA journal_mode=WAL;PRAGMA wal_autocheckpoint=0;CREATE TABLE backup_bulk(blob BLOB);INSERT INTO backup_bulk VALUES(zeroblob(32*1024*1024));`); err != nil {
		t.Fatal(err)
	}
	return s, path, backupDigest(t, s)
}
func waitBackupTemporary(t *testing.T, target string, done <-chan error) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("backup finished before race injection: %v", err)
		case <-deadline.C:
			t.Fatal("backup did not prepare temporary file")
		case <-ticker.C:
			entries, err := os.ReadDir(filepath.Dir(target))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".legacy-backup-") {
					return
				}
			}
		}
	}
}

func TestBackupLegacyConcurrentSourceChangeRejectsPublication(t *testing.T) {
	s, path, digest := backupLargeFixture(t)
	target := backupDestination(t)
	ctx := context.Background()
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`PRAGMA busy_timeout=5000;PRAGMA wal_autocheckpoint=0;`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.BackupLegacy(ctx, target, digest) }()
	waitBackupTemporary(t, target, done)
	if _, err := writer.Exec(`UPDATE m0_receipts SET body='changed during backup'`); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrConflict) {
		t.Fatal("source race published backup", err)
	}
	assertNoBackup(t, target)
}

func TestBackupLegacyCancelDuringVacuumCleansPrivateTemporary(t *testing.T) {
	s, _, digest := backupLargeFixture(t)
	target := backupDestination(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.BackupLegacy(ctx, target, digest) }()
	waitBackupTemporary(t, target, done)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("vacuum cancellation lost", err)
	}
	assertNoBackup(t, target)
	if backupDigest(t, s) != digest {
		t.Fatal("canceled backup changed source")
	}
}

func TestBackupLegacyPublicationRaceNeverOverwritesConflict(t *testing.T) {
	s, _, digest := backupLargeFixture(t)
	target := backupDestination(t)
	done := make(chan error, 1)
	go func() { done <- s.BackupLegacy(context.Background(), target, digest) }()
	waitBackupTemporary(t, target, done)
	marker := []byte("existing conflicting target")
	if err := os.WriteFile(target, marker, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("conflicting target accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != string(marker) {
		t.Fatal("publication replaced conflict", err)
	}
}

func TestBackupLegacyConcurrentSameArtifactIsIdempotent(t *testing.T) {
	fixture, path := legacyStore(t)
	fixture.Close()
	ctx := context.Background()
	s, err := OpenExisting(ctx, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := OpenExisting(ctx, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	target := backupDestination(t)
	digest := backupDigest(t, s)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, source := range []*Store{s, other} {
		wg.Add(1)
		go func(source *Store) { defer wg.Done(); results <- source.BackupLegacy(ctx, target, digest) }(source)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("same-digest race failed", err)
		}
	}
	if err := s.BackupLegacy(ctx, target, digest); err != nil {
		t.Fatal("race artifact invalid", err)
	}
}

func TestBackupLegacyRejectsDirectoryChangeDuringSnapshot(t *testing.T) {
	for _, name := range []string{"permissions", "replacement"} {
		t.Run(name, func(t *testing.T) {
			s, _, digest := backupLargeFixture(t)
			target := backupDestination(t)
			done := make(chan error, 1)
			go func() { done <- s.BackupLegacy(context.Background(), target, digest) }()
			waitBackupTemporary(t, target, done)
			dir := filepath.Dir(target)
			if name == "permissions" {
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				moved := dir + "-moved"
				if err := os.Rename(dir, moved); err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(moved)
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := <-done; err == nil {
				t.Fatal("changed destination directory accepted")
			}
			assertNoBackup(t, target)
		})
	}
}
