package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// BackupLegacy creates a private, independently readable schema-1 snapshot,
// including committed WAL data. Caller holds the app and canonical database
// locks and has stopped the old bridge. The destination directory must already
// exist as a physical, current-user-owned 0700 directory. Linux directory FDs
// anchor temporary SQLite paths and publication across directory renames.
//
// VACUUM INTO never checkpoints or migrates the source. A brief final write-
// intent lock prevents SQLite writers changing the source during publication;
// it is rolled back without writing source rows or changing its schema.
func (s *Store) BackupLegacy(ctx context.Context, targetAbsolutePath, expectedInventoryDigest string) (err error) {
	if s == nil || s.DB == nil || !filepath.IsAbs(targetAbsolutePath) || filepath.Clean(targetAbsolutePath) != targetAbsolutePath || !legacyDigest(expectedInventoryDigest) {
		return ErrConflict
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	defer func() {
		if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				err = errors.New("legacy backup failed; no replacement was authorized")
			}
		}
	}()
	directory := filepath.Dir(targetAbsolutePath)
	physical, e := filepath.EvalSymlinks(directory)
	if e != nil || physical != directory {
		return ErrConflict
	}
	dirFD, e := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return e
	}
	defer unix.Close(dirFD)
	var dirStat unix.Stat_t
	if e = unix.Fstat(dirFD, &dirStat); e != nil {
		return e
	}
	if dirStat.Uid != uint32(os.Geteuid()) || dirStat.Mode&07777 != 0700 {
		return ErrConflict
	}
	targetName := filepath.Base(targetAbsolutePath)
	conn, e := s.DB.Conn(ctx)
	if e != nil {
		return e
	}
	defer conn.Close()
	sourcePath, e := legacyBackupSourcePath(ctx, conn)
	if e != nil {
		return e
	}
	if sourcePath == targetAbsolutePath {
		return ErrConflict
	}
	var dataVersion int64
	if e = conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&dataVersion); e != nil {
		return e
	}
	inventory, e := legacyBackupInventory(ctx, conn)
	if e != nil {
		return e
	}
	if inventory.Schema != 1 || inventory.Digest() != expectedInventoryDigest {
		return ErrConflict
	}
	// Replays verify the existing artifact, not merely its name or size.
	var targetStat unix.Stat_t
	if e = unix.Fstatat(dirFD, targetName, &targetStat, unix.AT_SYMLINK_NOFOLLOW); e == nil {
		if e = legacyVerifyBackup(ctx, dirFD, targetName, expectedInventoryDigest); e != nil {
			return e
		}
		if e = legacyBackupLockSource(ctx, conn, dataVersion); e != nil {
			return e
		}
		defer legacyBackupUnlockSource(conn)
		if e = legacyBackupDirectoryUnchanged(dirFD, directory); e != nil {
			return e
		}
		return ctx.Err()
	} else if !errors.Is(e, unix.ENOENT) {
		return e
	}
	tempName := ".legacy-backup-" + ID() + ".tmp"
	tempFD, e := unix.Openat(dirFD, tempName, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return e
	}
	tempFile := os.NewFile(uintptr(tempFD), tempName)
	defer tempFile.Close()
	defer func() {
		// Only names belonging to this invocation are removed; source sidecars and
		// any pre-existing target are never deleted or changed.
		unix.Unlinkat(dirFD, tempName, 0)
		unix.Unlinkat(dirFD, tempName+"-wal", 0)
		unix.Unlinkat(dirFD, tempName+"-shm", 0)
		unix.Unlinkat(dirFD, tempName+"-journal", 0)
	}()
	if e = tempFile.Chmod(0600); e != nil {
		return e
	}
	tempPath := legacyBackupFDPath(dirFD, tempName)
	if _, e = conn.ExecContext(ctx, "VACUUM INTO ?", tempPath); e != nil {
		return e
	}
	if e = legacyVerifyBackup(ctx, dirFD, tempName, expectedInventoryDigest); e != nil {
		return e
	}
	if e = tempFile.Sync(); e != nil {
		return e
	}
	if e = legacyBackupLockSource(ctx, conn, dataVersion); e != nil {
		return e
	}
	defer legacyBackupUnlockSource(conn)
	if e = legacyBackupDirectoryUnchanged(dirFD, directory); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	// Linkat is the atomic no-overwrite publish operation. Rename would replace
	// an existing artifact; an existence check followed by Rename is unsafe.
	if e = unix.Linkat(dirFD, tempName, dirFD, targetName, 0); e != nil {
		if errors.Is(e, unix.EEXIST) {
			return legacyVerifyBackup(ctx, dirFD, targetName, expectedInventoryDigest)
		}
		return e
	}
	// Until the directory sync succeeds, remove only our exact published inode
	// on failure. A concurrent conflicting target is never unlinked.
	published := true
	defer func() {
		if err != nil && published {
			var tmp, dst unix.Stat_t
			if unix.Fstat(int(tempFile.Fd()), &tmp) == nil && unix.Fstatat(dirFD, targetName, &dst, unix.AT_SYMLINK_NOFOLLOW) == nil && tmp.Dev == dst.Dev && tmp.Ino == dst.Ino {
				unix.Unlinkat(dirFD, targetName, 0)
				unix.Fsync(dirFD)
			}
		}
	}()
	if e = unix.Unlinkat(dirFD, tempName, 0); e != nil {
		return e
	}
	if e = unix.Fsync(dirFD); e != nil {
		return e
	}
	published = false
	return nil
}

// Revalidate the selected directory before publishing through its pinned FD.
// A renamed/replaced directory or changed permissions cannot silently redirect
// the requested artifact into an unexpected or non-private location.
func legacyBackupDirectoryUnchanged(dirFD int, directory string) error {
	physical, err := filepath.EvalSymlinks(directory)
	if err != nil || physical != directory {
		return ErrConflict
	}
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrConflict
	}
	defer unix.Close(fd)
	var pinned, current unix.Stat_t
	if unix.Fstat(dirFD, &pinned) != nil || unix.Fstat(fd, &current) != nil {
		return ErrConflict
	}
	if pinned.Dev != current.Dev || pinned.Ino != current.Ino || current.Uid != uint32(os.Geteuid()) || current.Mode&07777 != 0700 {
		return ErrConflict
	}
	return nil
}

func legacyBackupFDPath(dirFD int, name string) string {
	return fmt.Sprintf("/proc/self/fd/%d/%s", dirFD, name)
}

func legacyBackupSourcePath(ctx context.Context, conn *sql.Conn) (string, error) {
	rows, err := conn.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var source string
	for rows.Next() {
		var seq int
		var name, path string
		if err = rows.Scan(&seq, &name, &path); err != nil {
			return "", err
		}
		if name == "main" {
			source = path
		}
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(source) {
		return "", ErrConflict
	}
	physical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", err
	}
	return physical, nil
}

func legacyBackupInventory(ctx context.Context, conn *sql.Conn) (LegacyInventory, error) {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return LegacyInventory{}, err
	}
	defer tx.Rollback()
	inventory, err := readLegacyInventory(ctx, tx)
	if err != nil {
		return inventory, err
	}
	return inventory, tx.Commit()
}

func legacyBackupLockSource(ctx context.Context, conn *sql.Conn, expectedVersion int64) error {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	var version, schema int64
	err := conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version)
	if err == nil {
		err = conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema)
	}
	if err == nil && (version != expectedVersion || schema != 1) {
		err = ErrConflict
	}
	if err != nil {
		legacyBackupUnlockSource(conn)
	}
	return err
}

func legacyBackupUnlockSource(conn *sql.Conn) {
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		// A connection whose manual transaction cannot be released must not return
		// to the pool with its write lock still held.
		conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

func legacyVerifyBackup(ctx context.Context, dirFD int, name, expectedDigest string) error {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrConflict
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err != nil {
		return err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&07777 != 0600 || info.Uid != uint32(os.Geteuid()) || info.Nlink != 1 {
		return ErrConflict
	}
	path := legacyBackupFDPath(dirFD, name)
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	inventory, err := legacyBackupInventory(ctx, conn)
	if err != nil {
		return err
	}
	if inventory.Schema != 1 || inventory.Digest() != expectedDigest {
		return ErrConflict
	}
	// Refuse a path swap between the no-follow open and SQLite's own open.
	var current unix.Stat_t
	if err = unix.Fstatat(dirFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if current.Dev != info.Dev || current.Ino != info.Ino {
		return ErrConflict
	}
	return nil
}
