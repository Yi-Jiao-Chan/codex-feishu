package codexclient

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var ErrUnanchoredTurnUnavailable = errors.New("native unanchored turn origin unavailable")

const turnOriginMaxBytes = 256 << 20
const turnOriginMaxLine = 32 << 20 // Match the native transport's maximum public item size.

// UnanchoredTurnReader supplements v2 history, which omits turn_trigger. Only a
// daemon-selected, identity-checked rollout can prove a goal start or manual
// capacity retry. Neither needs a new user message; neither borrows an anchor.
type UnanchoredTurnReader struct {
	Metadata func(context.Context, string) (Thread, error)
	Roots    []string
	mu       sync.Mutex
	cache    map[string]turnOriginCache
}

type turnOriginCache struct {
	path           string
	size, modified int64
	device, inode  uint64
	digest         [sha256.Size]byte
	origins        map[string]string
}

func NewUnanchoredTurnReader(metadata func(context.Context, string) (Thread, error), codexHome string) *UnanchoredTurnReader {
	r := &UnanchoredTurnReader{Metadata: metadata}
	if filepath.IsAbs(codexHome) {
		r.Roots = []string{filepath.Join(codexHome, "sessions"), filepath.Join(codexHome, "archived_sessions")}
	}
	return r
}

func (r *UnanchoredTurnReader) Read(ctx context.Context, threadID string, turnIDs []string) (map[string]bool, error) {
	requested := map[string]bool{}
	for _, id := range turnIDs {
		if id == "" || requested[id] {
			return nil, ErrUnanchoredTurnUnavailable
		}
		requested[id] = true
	}
	origins, err := r.readOrigins(ctx, threadID)
	if err != nil {
		return nil, err
	}
	result := map[string]bool{}
	for id := range requested {
		if unanchoredTrigger(origins[id]) {
			result[id] = true
		}
	}
	return result, nil
}

// RetryTurns proves a specific journaled attempt from native attribution. The
// nonce is correlation evidence, not a native idempotency key.
func (r *UnanchoredTurnReader) RetryTurns(ctx context.Context, threadID string, attempts []string) (map[string]string, error) {
	requested := map[string]bool{}
	for _, id := range attempts {
		if !retryAttemptID(id) || requested[id] {
			return nil, ErrUnanchoredTurnUnavailable
		}
		requested[id] = true
	}
	origins, err := r.readOrigins(ctx, threadID)
	if err != nil {
		return nil, err
	}
	result, counts := map[string]string{}, map[string]int{}
	for turn, trigger := range origins {
		if !strings.HasPrefix(trigger, ManualRetryTriggerPrefix) {
			continue
		}
		id := strings.TrimPrefix(trigger, ManualRetryTriggerPrefix)
		if !requested[id] {
			continue
		}
		counts[id]++
		result[id] = turn
	}
	for id, count := range counts {
		if count != 1 {
			delete(result, id)
		}
	}
	return result, nil
}

func unanchoredTrigger(trigger string) bool {
	return trigger == "goal" || trigger == "capacity_retry_manual" || strings.HasPrefix(trigger, ManualRetryTriggerPrefix) && retryAttemptID(strings.TrimPrefix(trigger, ManualRetryTriggerPrefix))
}

func (r *UnanchoredTurnReader) readOrigins(ctx context.Context, threadID string) (map[string]string, error) {
	if threadID == "" || r.Metadata == nil || len(r.Roots) == 0 {
		return nil, ErrUnanchoredTurnUnavailable
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	thread, err := r.Metadata(call, threadID)
	cancel()
	if err != nil || thread.ID != threadID || thread.Path == nil {
		return nil, ErrUnanchoredTurnUnavailable
	}
	f, err := openNativeRollout(*thread.Path, r.Roots)
	if err != nil {
		return nil, ErrUnanchoredTurnUnavailable
	}
	defer f.Close()
	var before, after unix.Stat_t
	if unix.Fstat(int(f.Fd()), &before) != nil || before.Size > turnOriginMaxBytes {
		return nil, ErrUnanchoredTurnUnavailable
	}
	signature := turnOriginCache{path: *thread.Path, size: before.Size, modified: before.Mtim.Nano(), device: uint64(before.Dev), inode: before.Ino}
	r.mu.Lock()
	prior, exists := r.cache[threadID]
	r.mu.Unlock()
	origins := prior.origins
	digest := sha256.New()
	// Freeze a complete prefix. An active task can append new tool output
	// while this reader checks the turn's already-written start record.
	prefix := func() io.Reader { return io.NewSectionReader(f, 0, before.Size) }
	cached := exists && prior.path == signature.path && prior.size == signature.size && prior.modified == signature.modified && prior.device == signature.device && prior.inode == signature.inode
	if cached {
		// Some filesystems expose a coarse mtime. Replacing same-size content
		// within one clock tick must not reuse another origin classification.
		n, readErr := io.Copy(digest, prefix())
		copy(signature.digest[:], digest.Sum(nil))
		if readErr != nil || n != before.Size {
			return nil, ErrUnanchoredTurnUnavailable
		}
		cached = signature.digest == prior.digest
		if !cached {
			digest.Reset()
		}
	}
	if !cached {
		origins, err = readTurnOriginRollout(ctx, io.TeeReader(prefix(), digest), threadID)
		if err != nil {
			return nil, ErrUnanchoredTurnUnavailable
		}
		copy(signature.digest[:], digest.Sum(nil))
	}
	if unix.Fstat(int(f.Fd()), &after) != nil || after.Size < before.Size || ctx.Err() != nil {
		return nil, ErrUnanchoredTurnUnavailable
	}
	if before.Size != after.Size || before.Mtim != after.Mtim {
		// Appends are harmless only if the inspected prefix was not rewritten.
		digest.Reset()
		n, readErr := io.Copy(digest, prefix())
		var current [sha256.Size]byte
		copy(current[:], digest.Sum(nil))
		if readErr != nil || n != before.Size || current != signature.digest || ctx.Err() != nil {
			return nil, ErrUnanchoredTurnUnavailable
		}
	}
	signature.origins = origins
	r.mu.Lock()
	if len(r.cache) >= 256 {
		r.cache = nil
	}
	if r.cache == nil {
		r.cache = map[string]turnOriginCache{}
	}
	r.cache[threadID] = signature
	r.mu.Unlock()
	return origins, nil
}

func readUnanchoredRollout(ctx context.Context, file io.Reader, threadID string) (map[string]bool, error) {
	origins, err := readTurnOriginRollout(ctx, file, threadID)
	if err != nil {
		return nil, err
	}
	result := map[string]bool{}
	for id, trigger := range origins {
		if unanchoredTrigger(trigger) {
			result[id] = true
		}
	}
	return result, nil
}

func readTurnOriginRollout(ctx context.Context, file io.Reader, threadID string) (map[string]string, error) {
	limited := &io.LimitedReader{R: file, N: turnOriginMaxBytes + 1}
	scan := bufio.NewScanner(limited)
	scan.Buffer(make([]byte, 64<<10), turnOriginMaxLine)
	meta := false
	starts := map[string]int{}
	origins := map[string]string{}
	for scan.Scan() {
		if ctx.Err() != nil || !receiptJSONUnique(scan.Bytes()) {
			return nil, ErrUnanchoredTurnUnavailable
		}
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(scan.Bytes(), &record) != nil {
			return nil, ErrUnanchoredTurnUnavailable
		}
		if record.Type == "session_meta" {
			var p struct {
				ID string `json:"id"`
			}
			if meta || json.Unmarshal(record.Payload, &p) != nil || p.ID != threadID {
				return nil, ErrUnanchoredTurnUnavailable
			}
			meta = true
			continue
		}
		if !meta {
			return nil, ErrUnanchoredTurnUnavailable
		}
		if record.Type != "event_msg" {
			continue
		}
		var p struct {
			Type        string `json:"type"`
			TurnID      string `json:"turn_id"`
			Attribution *struct {
				TurnID  string `json:"turn_id"`
				Trigger string `json:"turn_trigger"`
			} `json:"turn_attribution"`
		}
		if json.Unmarshal(record.Payload, &p) != nil {
			return nil, ErrUnanchoredTurnUnavailable
		}
		if p.Type == "task_started" || p.Type == "turn_started" {
			starts[p.TurnID]++
			delete(origins, p.TurnID)
			if p.TurnID != "" && starts[p.TurnID] == 1 && p.Attribution != nil && p.Attribution.TurnID == p.TurnID {
				origins[p.TurnID] = p.Attribution.Trigger
			}
		}
	}
	if scan.Err() != nil || !meta || limited.N <= 0 {
		return nil, ErrUnanchoredTurnUnavailable
	}
	return origins, nil
}
