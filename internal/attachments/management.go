package attachments

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"codex-feishu/internal/store"
	"golang.org/x/sys/unix"
)

func privateCacheRoot(root string) (os.FileInfo, error) {
	if !filepath.IsAbs(root) {
		return nil, &cacheError{"disk-error"}
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil || !privateOwned(info, true) {
		return nil, &cacheError{"disk-error"}
	}
	return info, nil
}

// ExistingUsage never creates a cache directory. Missing caches have zero usage.
func (c *Cache) ExistingUsage() (int64, error) {
	if _, err := privateCacheRoot(c.Root); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	used, err := c.Usage()
	if err != nil {
		return 0, &cacheError{"disk-error"}
	}
	return used, nil
}

type cleanupFile struct {
	name string
	info os.FileInfo
}
type cleanupShard struct {
	name  string
	info  os.FileInfo
	files []cleanupFile
}

func cacheEntryInfoAt(dir *os.File, name string) (os.FileInfo, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &cacheError{"integrity"}
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, &cacheError{"integrity"}
	}
	return info, nil
}

func preflightCleanupShard(ctx context.Context, root *os.File, name string, info os.FileInfo, referenced map[string]bool) (cleanupShard, error) {
	shard := cleanupShard{name: name, info: info}
	dir, err := openCacheDirectoryAt(int(root.Fd()), name, filepath.Join(root.Name(), name))
	if err != nil {
		return shard, err
	}
	defer dir.Close()
	openedInfo, err := dir.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return shard, &cacheError{"integrity"}
	}
	children, err := dir.ReadDir(-1)
	if err != nil {
		return shard, &cacheError{"disk-error"}
	}
	for _, child := range children {
		if ctx.Err() != nil {
			return shard, ctx.Err()
		}
		childInfo, err := cacheEntryInfoAt(dir, child.Name())
		if err != nil {
			return shard, err
		}
		if childInfo.Mode()&os.ModeSymlink != 0 || !childInfo.IsDir() && !childInfo.Mode().IsRegular() {
			return shard, &cacheError{"integrity"}
		}
		candidate := generatedID(child.Name()) || strings.HasPrefix(child.Name(), ".partial-") && len(child.Name()) > len(".partial-")
		if !candidate {
			continue
		}
		if !childInfo.Mode().IsRegular() {
			return shard, &cacheError{"integrity"}
		}
		if referenced[name+"/"+child.Name()] {
			continue
		}
		shard.files = append(shard.files, cleanupFile{name: child.Name(), info: childInfo})
	}
	return shard, nil
}

func generatedID(s string) bool { return len(s) == 32 && strings.Trim(s, "0123456789abcdef") == "" }

func openCacheDirectoryAt(parent int, name, path string) (*os.File, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &cacheError{"integrity"}
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !privateOwned(info, true) {
		f.Close()
		return nil, &cacheError{"integrity"}
	}
	return f, nil
}

// Cleanup requires the deployment to be stopped and both deployment locks held
// by its caller. It retains paths derived from every supplied reference, even
// when a crash occurred after publication but before file_id was committed.
// Only generated final files and idle .partial-* files in generated shards are
// candidates. A complete preflight precedes deletion; directories are retained.
func (c *Cache) Cleanup(ctx context.Context, refs []store.AttachmentPart) (removedFiles int, removedBytes int64, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reserved != 0 {
		return 0, 0, &cacheError{"download-active"}
	}
	referenced := map[string]bool{}
	for _, p := range refs {
		if !generatedID(p.ID) || p.Scope.HostID == "" || p.Scope.AppID == "" || p.Scope.ThreadID == "" || p.FileID != "" && p.FileID != fileID(p) {
			return 0, 0, &cacheError{"integrity"}
		}
		referenced[fileID(p)] = true
	}
	rootInfo, err := privateCacheRoot(c.Root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	root, err := openCacheDirectoryAt(unix.AT_FDCWD, c.Root, c.Root)
	if err != nil {
		return 0, 0, err
	}
	defer root.Close()
	openedInfo, err := root.Stat()
	if err != nil || !os.SameFile(rootInfo, openedInfo) {
		return 0, 0, &cacheError{"integrity"}
	}
	entries, err := root.ReadDir(-1)
	if err != nil {
		return 0, 0, &cacheError{"disk-error"}
	}
	var shards []cleanupShard
	for _, entry := range entries {
		if ctx.Err() != nil {
			return 0, 0, ctx.Err()
		}
		info, err := cacheEntryInfoAt(root, entry.Name())
		if err != nil {
			return 0, 0, &cacheError{"disk-error"}
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
			return 0, 0, &cacheError{"integrity"}
		}
		if !generatedID(entry.Name()) || !info.IsDir() {
			continue
		}
		shard, err := preflightCleanupShard(ctx, root, entry.Name(), info, referenced)
		if err != nil {
			return 0, 0, err
		}
		shards = append(shards, shard)
	}
	for _, shard := range shards {
		dir, openErr := openCacheDirectoryAt(int(root.Fd()), shard.name, filepath.Join(c.Root, shard.name))
		if openErr != nil {
			return removedFiles, removedBytes, openErr
		}
		info, statErr := dir.Stat()
		if statErr != nil || !os.SameFile(shard.info, info) {
			dir.Close()
			return removedFiles, removedBytes, &cacheError{"integrity"}
		}
		for _, file := range shard.files {
			if ctx.Err() != nil {
				dir.Close()
				return removedFiles, removedBytes, ctx.Err()
			}
			var current unix.Stat_t
			original, ok := file.info.Sys().(*syscall.Stat_t)
			if !ok || unix.Fstatat(int(dir.Fd()), file.name, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || current.Mode&unix.S_IFMT != unix.S_IFREG || uint64(current.Ino) != original.Ino || uint64(current.Dev) != uint64(original.Dev) || current.Size != file.info.Size() {
				dir.Close()
				return removedFiles, removedBytes, &cacheError{"integrity"}
			}
			if unix.Unlinkat(int(dir.Fd()), file.name, 0) != nil {
				dir.Close()
				return removedFiles, removedBytes, &cacheError{"disk-error"}
			}
			removedFiles++
			removedBytes += current.Size
		}
		if len(shard.files) > 0 && dir.Sync() != nil {
			dir.Close()
			return removedFiles, removedBytes, &cacheError{"disk-error"}
		}
		dir.Close()
	}
	return removedFiles, removedBytes, nil
}
