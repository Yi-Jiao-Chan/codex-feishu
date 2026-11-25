package filepreview

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	maxArtifactBytes int64 = 2 << 30
	artifactTTL            = 48 * time.Hour
	artifactRetry          = 15 * time.Second
	artifactSweep          = 15 * time.Minute
	artifactWorkers        = 2
	artifactEntries        = 4096
	artifactBuffer         = 128 << 10
)

var (
	errArtifactPending = errors.New("file preview: preparing artifact")
	errArtifactQuota   = errors.New("file preview: artifact cache quota exhausted")
	errArtifactBusy    = errors.New("file preview: artifact service busy")
)

type artifactEntry struct {
	size    int64
	copied  atomic.Int64
	last    time.Time
	readers int
	pending bool
	err     error
}

type artifactPreparation struct {
	copied, size int64
}

func (artifactPreparation) Error() string { return errArtifactPending.Error() }
func (artifactPreparation) Unwrap() error { return errArtifactPending }

type artifactProgressWriter struct {
	io.Writer
	copied *atomic.Int64
}

func (w artifactProgressWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.copied.Add(int64(n))
	return n, err
}

type artifactCache struct {
	dir            *os.Root
	lock           *os.File
	now            func() time.Time
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	mu             sync.Mutex
	entries        map[string]*artifactEntry
	used, quota    int64
	workers        int
	closed, broken bool
}

// StartArtifactCache is called by the HTTP server only. A CLI can register files
// with the same service configuration without taking the cache writer lock.
func (s *Service) StartArtifactCache() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	if s.artifactQuota == 0 || s.artifacts != nil {
		return nil
	}
	if err := s.directory.Mkdir("artifacts", 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return errRegistry
	}
	info, err := s.directory.Lstat("artifacts")
	if err != nil || !info.IsDir() || !ownedPrivate(info, 0700) {
		return errRegistry
	}
	dir, err := s.directory.OpenRoot("artifacts")
	if err != nil {
		return errRegistry
	}
	lock, err := dir.Open(".")
	if err != nil {
		_ = dir.Close()
		return errRegistry
	}
	opened, err := lock.Stat()
	if err != nil || !os.SameFile(info, opened) || unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		_ = lock.Close()
		_ = dir.Close()
		return errors.New("file preview: private artifact cache is unavailable or already in use")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &artifactCache{dir: dir, lock: lock, now: s.now, ctx: ctx, cancel: cancel,
		entries: make(map[string]*artifactEntry), quota: s.artifactQuota}
	if err := c.restore(); err != nil {
		c.close()
		return err
	}
	c.wg.Add(1)
	go c.sweep()
	s.artifacts = c
	return nil
}

func (c *artifactCache) restore() error {
	names, err := c.lock.Readdirnames(-1)
	if err != nil {
		return errRegistry
	}
	for _, name := range names {
		id := strings.TrimPrefix(name, ".pending-")
		if id != name && validID(id) {
			if err := c.dir.Remove(name); err != nil {
				return errRegistry
			}
			continue
		}
		id, ok := strings.CutSuffix(name, ".artifact")
		if !ok || !validID(id) {
			continue
		}
		info, err := c.dir.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || !ownedPrivate(info, 0600) || info.Size() > maxArtifactBytes {
			return errRegistry
		}
		c.entries[id] = &artifactEntry{size: info.Size(), last: info.ModTime()}
		c.used += info.Size()
	}
	c.purge(c.now())
	if c.broken {
		return errRegistry
	}
	return nil
}

func (c *artifactCache) sweep() {
	defer c.wg.Done()
	ticker := time.NewTicker(artifactSweep)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			c.purge(c.now())
			c.mu.Unlock()
		}
	}
}

func (c *artifactCache) close() {
	c.mu.Lock()
	c.closed = true
	c.cancel()
	c.mu.Unlock()
	c.wg.Wait()
	_ = c.dir.Close()
	_ = c.lock.Close()
}

// purge runs under mu, or during startup before publication.
func (c *artifactCache) purge(now time.Time) {
	for version, entry := range c.entries {
		lifetime := artifactTTL
		if entry.err != nil {
			lifetime = artifactRetry
		}
		if !entry.pending && entry.readers == 0 && !now.Before(entry.last.Add(lifetime)) {
			c.remove(version, entry)
		}
	}
}

func (c *artifactCache) remove(version string, entry *artifactEntry) {
	if entry.err == nil {
		if err := c.dir.Remove(version + ".artifact"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.broken = true
			return
		}
	}
	delete(c.entries, version)
	c.used -= entry.size
}

func (s *Service) artifactVersion(id string, info fs.FileInfo) string {
	stat := info.Sys().(*syscall.Stat_t)
	return s.mac("artifact", fmt.Sprintf("%s:%d:%d:%d:%d:%d:%d:%d",
		id, stat.Dev, stat.Ino, stat.Size, stat.Mtim.Sec, stat.Mtim.Nsec, stat.Ctim.Sec, stat.Ctim.Nsec))
}

// request takes ownership of source. Publication, reservations, expiry and
// transfers all share the same entry lock. The source copy runs outside it.
func (c *artifactCache) request(s *Service, id, version string, source *os.File, size int64, start bool) (*os.File, func(), error) {
	defer func() {
		if source != nil {
			_ = source.Close()
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.broken {
		return nil, nil, errRegistry
	}
	now := c.now()
	entry := c.entries[version]
	if entry != nil {
		switch {
		case entry.pending:
			return nil, nil, artifactPreparation{copied: entry.copied.Load(), size: entry.size}
		case entry.err != nil && now.Before(entry.last.Add(artifactRetry)):
			return nil, nil, entry.err
		case entry.err == nil && (entry.readers != 0 || now.Before(entry.last.Add(artifactTTL))):
			name := version + ".artifact"
			f, err := c.dir.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				return nil, nil, errRegistry
			}
			info, err := f.Stat()
			if err != nil || !info.Mode().IsRegular() || !ownedPrivate(info, 0600) || info.Size() != size ||
				c.dir.Chtimes(name, now, now) != nil {
				_ = f.Close()
				return nil, nil, errRegistry
			}
			entry.last = now
			entry.readers++
			return f, func() {
				_ = f.Close()
				c.mu.Lock()
				entry.readers--
				c.mu.Unlock()
			}, nil
		}
	}
	if !start {
		return nil, nil, artifactPreparation{size: size}
	}
	c.purge(now)
	if c.broken {
		return nil, nil, errRegistry
	}
	if size > c.quota-c.used || len(c.entries) >= artifactEntries {
		return nil, nil, errArtifactQuota
	}
	if c.workers >= artifactWorkers {
		return nil, nil, errArtifactBusy
	}
	entry = &artifactEntry{size: size, pending: true}
	c.entries[version] = entry
	c.used += size
	c.workers++
	c.wg.Add(1)
	go c.generate(s, id, version, entry, source)
	source = nil
	return nil, nil, artifactPreparation{size: size}
}

func (c *artifactCache) generate(s *Service, id, version string, entry *artifactEntry, source *os.File) {
	defer c.wg.Done()
	defer source.Close()
	tmp := ".pending-" + version
	dst, err := c.dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err == nil {
		writer := artifactProgressWriter{Writer: dst, copied: &entry.copied}
		err = copyArtifact(c.ctx, writer, source, entry.size, s.key)
		info, statErr := source.Stat()
		if err == nil && (statErr != nil || s.artifactVersion(id, info) != version) {
			err = errArtifactBusy
		}
		if err == nil {
			err = dst.Sync()
		}
		if closeErr := dst.Close(); err == nil {
			err = closeErr
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.workers--
	entry.pending = false
	entry.last = c.now()
	if err == nil {
		err = c.dir.Chtimes(tmp, entry.last, entry.last)
	}
	if err == nil {
		err = c.dir.Rename(tmp, version+".artifact")
	}
	if err != nil {
		if removeErr := c.dir.Remove(tmp); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			c.broken = true
			return
		}
		c.used -= entry.size
		entry.size = 0
		entry.err = errRegistry
		if errors.Is(err, errNotPreviewable) || errors.Is(err, errArtifactBusy) {
			entry.err = err
		}
	}
}

// copyArtifact validates the same UTF-8 text policy as existing previews, while
// keeping only a fixed-size chunk and a possible short credential in memory.
func copyArtifact(ctx context.Context, dst io.Writer, src io.Reader, size int64, secret [32]byte) error {
	reader := bufio.NewReaderSize(io.LimitReader(src, size), artifactBuffer)
	var candidate []byte
	possibleSecret, trailing := true, false
	for remaining := size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int(min(remaining, artifactBuffer))
		chunk, err := reader.Peek(n)
		if err != nil {
			return errArtifactBusy
		}
		if remaining > int64(n) {
			// Leave an incomplete final rune for the next chunk.
			last := len(chunk) - 1
			for last > len(chunk)-utf8.UTFMax && !utf8.RuneStart(chunk[last]) {
				last--
			}
			if !utf8.FullRune(chunk[last:]) {
				chunk = chunk[:last]
			}
		}
		if !utf8.Valid(chunk) {
			return errNotPreviewable
		}
		for _, b := range chunk {
			if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
				return errNotPreviewable
			}
		}
		if possibleSecret {
			part := chunk
			if len(candidate) == 0 {
				part = bytes.TrimLeftFunc(part, unicode.IsSpace)
			}
			text := bytes.TrimRightFunc(part, unicode.IsSpace)
			if len(text) > 0 {
				possibleSecret = !trailing && len(candidate)+len(text) <= 256
				if possibleSecret {
					candidate = append(candidate, text...)
				}
			}
			trailing = trailing || len(part) != len(text)
		}
		if n, err := dst.Write(chunk); err != nil {
			return err
		} else if n != len(chunk) {
			return io.ErrShortWrite
		}
		_, _ = reader.Discard(len(chunk))
		remaining -= int64(len(chunk))
	}
	if possibleSecret && sha256.Sum256(candidate) == secret {
		return errNotPreviewable
	}
	return nil
}
