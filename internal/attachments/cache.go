// Package attachments stores inbound task materials without interpreting or
// executing them. Native submissions remain in the existing submissions actor.
package attachments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	_ "golang.org/x/image/webp"
	"golang.org/x/sys/unix"
)

type ResourceGateway interface {
	OpenMessageResource(context.Context, string, string, string) (io.ReadCloser, feishu.ResourceMetadata, error)
}

type Cache struct {
	Root     string
	Limits   config.AttachmentConfig
	mu       sync.Mutex
	reserved int64
}

type cacheError struct{ code string }

func (e *cacheError) Error() string { return "attachment resource unavailable: " + e.code }

func NewCache(root string, limits config.AttachmentConfig) (*Cache, error) {
	if !filepath.IsAbs(root) {
		return nil, &cacheError{"disk-error"}
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, &cacheError{"disk-error"}
	}
	info, err := os.Lstat(root)
	if err != nil || !privateOwned(info, true) {
		return nil, &cacheError{"disk-error"}
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, &cacheError{"disk-error"}
	}
	return &Cache{Root: root, Limits: limits}, nil
}

func fileID(p store.AttachmentPart) string {
	key := sha256.Sum256([]byte(p.Scope.HostID + "\x00" + p.Scope.AppID + "\x00" + p.Scope.ThreadID))
	return hex.EncodeToString(key[:16]) + "/" + p.ID
}

func (c *Cache) path(p store.AttachmentPart) (string, error) {
	if len(p.ID) != 32 || strings.Trim(p.ID, "0123456789abcdef") != "" || p.Scope.HostID == "" || p.Scope.AppID == "" || p.Scope.ThreadID == "" || p.FileID != "" && p.FileID != fileID(p) {
		return "", &cacheError{"integrity"}
	}
	dir := filepath.Join(c.Root, filepath.Dir(fileID(p)))
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", &cacheError{"disk-error"}
	}
	info, err := os.Lstat(dir)
	if err != nil || !privateOwned(info, true) {
		return "", &cacheError{"disk-error"}
	}
	return filepath.Join(c.Root, fileID(p)), nil
}

func openPrivate(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !privateOwned(info, false) {
		f.Close()
		return nil, &cacheError{"integrity"}
	}
	return f, nil
}

func privateOwned(info os.FileInfo, directory bool) bool {
	if info == nil || info.Mode().Perm()&0077 != 0 || directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func (c *Cache) inspect(path string, p store.AttachmentPart) (store.AttachmentPart, error) {
	f, err := openPrivate(path)
	if err != nil {
		return p, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return p, &cacheError{"disk-error"}
	}
	limit := c.Limits.MaxFileBytes
	if p.Kind == "image" {
		limit = c.Limits.MaxImageBytes
	}
	if info.Size() <= 0 || info.Size() > limit {
		return p, &cacheError{"size-limit"}
	}
	head := make([]byte, 512)
	n, err := f.Read(head)
	if err != nil && err != io.EOF {
		return p, &cacheError{"disk-error"}
	}
	actual := http.DetectContentType(head[:n])
	if p.Kind == "image" {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return p, &cacheError{"disk-error"}
		}
		cfg, format, err := image.DecodeConfig(f)
		if err != nil || format != "png" && format != "jpeg" && format != "webp" {
			return p, &cacheError{"image-type"}
		}
		if cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width) > c.Limits.MaxImagePixels/int64(cfg.Height) {
			return p, &cacheError{"pixel-limit"}
		}
		declared, _, _ := mime.ParseMediaType(p.ContentType)
		if declared != "" && declared != "application/octet-stream" && declared != actual {
			return p, &cacheError{"image-type"}
		}
		// Both formats can carry animation beyond the first image header.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return p, &cacheError{"disk-error"}
		}
		if err := staticImage(f, format, info.Size()); err != nil {
			return p, err
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return p, &cacheError{"disk-error"}
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return p, &cacheError{"disk-error"}
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if p.Digest != "" && p.Digest != digest || p.Size != 0 && p.Size != info.Size() {
		return p, &cacheError{"integrity"}
	}
	p.Size, p.Digest, p.FileID, p.ContentType = info.Size(), digest, fileID(p), actual
	return p, nil
}

// Resolve verifies persisted ready bytes before publishing their path to Codex.
func (c *Cache) Resolve(p store.AttachmentPart) (string, error) {
	path, err := c.path(p)
	if err != nil {
		return "", err
	}
	if _, err := c.inspect(path, p); err != nil {
		return "", err
	}
	return path, nil
}

func (c *Cache) Usage() (int64, error) {
	var size int64
	err := filepath.WalkDir(c.Root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type()&os.ModeSymlink != 0 {
			return &cacheError{"integrity"}
		}
		if !e.IsDir() {
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() {
				return &cacheError{"disk-error"}
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func (c *Cache) Download(ctx context.Context, gateway ResourceGateway, p store.AttachmentPart) (store.AttachmentPart, error) {
	path, err := c.path(p)
	if err != nil {
		return p, err
	}
	// A crash after atomic publication and before the ready transaction can be
	// recovered without another GET. Partial files never use the final ID.
	if info, err := os.Lstat(path); err == nil {
		checked, inspectErr := c.inspect(path, p)
		if inspectErr == nil {
			return checked, nil
		}
		// Only an active, pre-submission download may replace its own corrupt
		// cache entry. Keep expected size/digest so the GET must restore the
		// original resource. Resolve never mutates bytes already given to Codex.
		if p.State != "downloading" || !privateOwned(info, false) {
			return p, inspectErr
		}
		current, statErr := os.Lstat(path)
		if statErr != nil || !os.SameFile(info, current) || !privateOwned(current, false) {
			return p, &cacheError{"integrity"}
		}
		if err := os.Remove(path); err != nil {
			return p, &cacheError{"disk-error"}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return p, &cacheError{"disk-error"}
	}
	limit := c.Limits.MaxFileBytes
	if p.Kind == "image" {
		limit = c.Limits.MaxImageBytes
	}
	c.mu.Lock()
	used, err := c.Usage()
	if err != nil || used > c.Limits.DiskQuotaBytes-c.reserved-limit {
		c.mu.Unlock()
		return p, &cacheError{"disk-quota"}
	}
	c.reserved += limit
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.reserved -= limit; c.mu.Unlock() }()
	stream, meta, err := gateway.OpenMessageResource(ctx, p.MessageID, p.ResourceKey, p.Kind)
	if err != nil {
		return p, err
	}
	defer stream.Close()
	if meta.ContentLength > limit {
		return p, &cacheError{"size-limit"}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".partial-")
	if err != nil {
		return p, &cacheError{"disk-error"}
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	buf := make([]byte, 32<<10)
	written, err := io.CopyBuffer(f, io.LimitReader(stream, limit+1), buf)
	if err != nil {
		var writeErr *os.PathError
		if errors.As(err, &writeErr) {
			return p, &cacheError{"disk-error"}
		}
		return p, &cacheError{"download-interrupted"}
	}
	if ctx.Err() != nil {
		return p, &cacheError{"download-interrupted"}
	}
	if written > limit {
		return p, &cacheError{"size-limit"}
	}
	if meta.ContentLength >= 0 && written != meta.ContentLength {
		return p, &cacheError{"download-interrupted"}
	}
	p.ContentType = meta.ContentType
	checked, err := c.inspect(f.Name(), p)
	if err != nil {
		return p, err
	}
	if err := f.Sync(); err != nil {
		return p, &cacheError{"disk-error"}
	}
	if err := f.Close(); err != nil {
		return p, &cacheError{"disk-error"}
	}
	// Link is an atomic no-replace publish. Never overwrite a completed asset.
	if err := os.Link(f.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return c.inspect(path, checked)
		}
		return p, &cacheError{"disk-error"}
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return p, &cacheError{"disk-error"}
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return p, &cacheError{"disk-error"}
	}
	return checked, nil
}
