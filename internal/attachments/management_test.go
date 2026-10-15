package attachments

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-feishu/internal/config"
	"codex-feishu/internal/store"
	"golang.org/x/sys/unix"
)

func managementCache(t *testing.T) *Cache {
	t.Helper()
	cache, err := NewCache(filepath.Join(t.TempDir(), "cache"), config.AttachmentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func managementFile(t *testing.T, cache *Cache, p store.AttachmentPart, data string) string {
	t.Helper()
	path, err := cache.path(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCleanupRetainsProspectiveReferencedFinalFilesAndSkipsUnknownFiles(t *testing.T) {
	cache := managementCache(t)
	scope := store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}
	var refs []store.AttachmentPart
	var keep []string
	for i, state := range []string{"pending", "downloading", "ready", "failed", "unknown", "queued"} {
		p := store.AttachmentPart{ID: strings.Repeat(string(rune('a'+i)), 32), Scope: scope, State: state}
		// Empty FileID is intentional, including ready-before-DB recovery.
		keep = append(keep, managementFile(t, cache, p, state))
		refs = append(refs, p)
	}
	orphan := store.AttachmentPart{ID: strings.Repeat("1", 32), Scope: scope}
	orphanPath := managementFile(t, cache, orphan, "unused")
	partial := filepath.Join(filepath.Dir(orphanPath), ".partial-12345")
	if err := os.WriteFile(partial, []byte("part"), 0600); err != nil {
		t.Fatal(err)
	}
	unknown := []string{filepath.Join(cache.Root, "notes.txt"), filepath.Join(filepath.Dir(orphanPath), "original-name.txt"), filepath.Join(cache.Root, ".partial-root"), filepath.Join(cache.Root, strings.Repeat("9", 32))}
	for _, path := range unknown {
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	unknownDir := filepath.Join(cache.Root, "not-a-generated-shard")
	if err := os.Mkdir(unknownDir, 0700); err != nil {
		t.Fatal(err)
	}
	unknown = append(unknown, filepath.Join(unknownDir, strings.Repeat("2", 32)))
	if err := os.WriteFile(unknown[len(unknown)-1], []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	files, bytes, err := cache.Cleanup(context.Background(), refs)
	if err != nil || files != 2 || bytes != 10 {
		t.Fatalf("cleanup files=%d bytes=%d err=%v", files, bytes, err)
	}
	for _, path := range append(keep, unknown...) {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained file unavailable: %v", err)
		}
	}
	for _, path := range []string{orphanPath, partial} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan remained: %v", err)
		}
	}
	if info, err := os.Stat(filepath.Dir(orphanPath)); err != nil || !info.IsDir() {
		t.Fatal("cleanup deleted shard directory")
	}
}

func TestCleanupRejectsUnsafeEntriesBeforeAnyDeletion(t *testing.T) {
	for _, kind := range []string{"root-symlink", "shard-symlink", "file-symlink", "unknown-symlink", "fifo", "recognized-directory", "public-root"} {
		t.Run(kind, func(t *testing.T) {
			cache := managementCache(t)
			p := store.AttachmentPart{ID: strings.Repeat("a", 32), Scope: store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}}
			orphan := managementFile(t, cache, p, "keep-on-error")
			external := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(external, []byte("outside"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "root-symlink":
				alias := cache.Root + "-alias"
				if err := os.Symlink(cache.Root, alias); err != nil {
					t.Fatal(err)
				}
				cache.Root = alias
			case "shard-symlink":
				if err := os.Symlink(filepath.Dir(external), filepath.Join(cache.Root, strings.Repeat("f", 32))); err != nil {
					t.Fatal(err)
				}
			case "file-symlink":
				if err := os.Symlink(external, filepath.Join(filepath.Dir(orphan), strings.Repeat("f", 32))); err != nil {
					t.Fatal(err)
				}
			case "unknown-symlink":
				if err := os.Symlink(external, filepath.Join(cache.Root, "unknown")); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(filepath.Join(filepath.Dir(orphan), "unknown-pipe"), 0600); err != nil {
					t.Fatal(err)
				}
			case "recognized-directory":
				if err := os.Mkdir(filepath.Join(filepath.Dir(orphan), strings.Repeat("f", 32)), 0700); err != nil {
					t.Fatal(err)
				}
			case "public-root":
				if err := os.Chmod(cache.Root, 0755); err != nil {
					t.Fatal(err)
				}
			}
			files, bytes, err := cache.Cleanup(context.Background(), nil)
			if err == nil || files != 0 || bytes != 0 {
				t.Fatalf("unsafe cleanup files=%d bytes=%d err=%v", files, bytes, err)
			}
			if _, err := os.Stat(orphan); err != nil {
				t.Fatal("cleanup deleted a file before unsafe-entry preflight completed")
			}
			if got, err := os.ReadFile(external); err != nil || string(got) != "outside" {
				t.Fatal("external file changed")
			}
		})
	}
}

func TestCleanupRejectsInvalidReferencesAndActiveDownloads(t *testing.T) {
	cache := managementCache(t)
	p := store.AttachmentPart{ID: strings.Repeat("a", 32), Scope: store.SubmissionScope{HostID: "host", AppID: "app", ThreadID: "thread"}}
	path := managementFile(t, cache, p, "keep")
	bad := p
	bad.FileID = "different/path"
	if _, _, err := cache.Cleanup(context.Background(), []store.AttachmentPart{bad}); err == nil {
		t.Fatal("accepted inconsistent reference")
	}
	cache.reserved = 1
	if _, _, err := cache.Cleanup(context.Background(), nil); err == nil {
		t.Fatal("cleaned active download")
	}
	cache.reserved = 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := cache.Cleanup(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("deleted file on failed cleanup")
	}
}

func TestExistingUsageDoesNotCreateMissingRootAndRequiresPrivateDirectory(t *testing.T) {
	cache := &Cache{Root: filepath.Join(t.TempDir(), "missing")}
	if used, err := cache.ExistingUsage(); err != nil || used != 0 {
		t.Fatalf("missing usage=%d err=%v", used, err)
	}
	if _, err := os.Stat(cache.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("status created cache")
	}
	if files, bytes, err := cache.Cleanup(context.Background(), nil); err != nil || files != 0 || bytes != 0 {
		t.Fatalf("missing cleanup: %d %d %v", files, bytes, err)
	}
	if _, err := os.Stat(cache.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup created cache")
	}
	if err := os.Mkdir(cache.Root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.ExistingUsage(); err == nil {
		t.Fatal("accepted public cache")
	}
}
