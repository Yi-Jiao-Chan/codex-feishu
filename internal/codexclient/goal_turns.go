package codexclient

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var ErrGoalTurnUnavailable = errors.New("native goal turn origin unavailable")

const goalRolloutMaxBytes = 256 << 20
const goalRolloutMaxLine = 32 << 20 // Match the native transport's maximum public item size.

// GoalTurnReader supplements v2 history, which omits turn_trigger. Only a
// daemon-selected, identity-checked rollout can prove an automatic goal start.
// A current goal or rootTurnId alone is not evidence of a turn's origin.
type GoalTurnReader struct {
	Metadata func(context.Context, string) (Thread, error)
	Roots    []string
	mu       sync.Mutex
	cache    map[string]goalTurnCache
}

type goalTurnCache struct {
	path           string
	size, modified int64
	device, inode  uint64
	digest         [sha256.Size]byte
	goals          map[string]bool
}

func NewGoalTurnReader(metadata func(context.Context, string) (Thread, error), codexHome string) *GoalTurnReader {
	r := &GoalTurnReader{Metadata: metadata}
	if filepath.IsAbs(codexHome) {
		r.Roots = []string{filepath.Join(codexHome, "sessions"), filepath.Join(codexHome, "archived_sessions")}
	}
	return r
}

func (r *GoalTurnReader) Read(ctx context.Context, threadID string, turnIDs []string) (map[string]bool, error) {
	if threadID == "" || r.Metadata == nil || len(r.Roots) == 0 {
		return nil, ErrGoalTurnUnavailable
	}
	requested := map[string]bool{}
	for _, id := range turnIDs {
		if id == "" || requested[id] {
			return nil, ErrGoalTurnUnavailable
		}
		requested[id] = true
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	thread, err := r.Metadata(call, threadID)
	cancel()
	if err != nil || thread.ID != threadID || thread.Path == nil {
		return nil, ErrGoalTurnUnavailable
	}
	f, err := openNativeRollout(*thread.Path, r.Roots)
	if err != nil {
		return nil, ErrGoalTurnUnavailable
	}
	defer f.Close()
	var before, after unix.Stat_t
	if unix.Fstat(int(f.Fd()), &before) != nil || before.Size > goalRolloutMaxBytes {
		return nil, ErrGoalTurnUnavailable
	}
	signature := goalTurnCache{path: *thread.Path, size: before.Size, modified: before.Mtim.Nano(), device: uint64(before.Dev), inode: before.Ino}
	r.mu.Lock()
	prior, exists := r.cache[threadID]
	r.mu.Unlock()
	goals := prior.goals
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
			return nil, ErrGoalTurnUnavailable
		}
		cached = signature.digest == prior.digest
		if !cached {
			digest.Reset()
		}
	}
	if !cached {
		goals, err = readGoalRollout(ctx, io.TeeReader(prefix(), digest), threadID)
		if err != nil {
			return nil, ErrGoalTurnUnavailable
		}
		copy(signature.digest[:], digest.Sum(nil))
	}
	if unix.Fstat(int(f.Fd()), &after) != nil || after.Size < before.Size || ctx.Err() != nil {
		return nil, ErrGoalTurnUnavailable
	}
	if before.Size != after.Size || before.Mtim != after.Mtim {
		// Appends are harmless only if the inspected prefix was not rewritten.
		digest.Reset()
		n, readErr := io.Copy(digest, prefix())
		var current [sha256.Size]byte
		copy(current[:], digest.Sum(nil))
		if readErr != nil || n != before.Size || current != signature.digest || ctx.Err() != nil {
			return nil, ErrGoalTurnUnavailable
		}
	}
	signature.goals = goals
	r.mu.Lock()
	if len(r.cache) >= 256 {
		r.cache = nil
	}
	if r.cache == nil {
		r.cache = map[string]goalTurnCache{}
	}
	r.cache[threadID] = signature
	r.mu.Unlock()
	result := map[string]bool{}
	for id := range requested {
		if goals[id] {
			result[id] = true
		}
	}
	return result, nil
}

func readGoalRollout(ctx context.Context, file io.Reader, threadID string) (map[string]bool, error) {
	limited := &io.LimitedReader{R: file, N: goalRolloutMaxBytes + 1}
	scan := bufio.NewScanner(limited)
	scan.Buffer(make([]byte, 64<<10), goalRolloutMaxLine)
	meta := false
	starts := map[string]int{}
	goals := map[string]bool{}
	for scan.Scan() {
		if ctx.Err() != nil || !receiptJSONUnique(scan.Bytes()) {
			return nil, ErrGoalTurnUnavailable
		}
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(scan.Bytes(), &record) != nil {
			return nil, ErrGoalTurnUnavailable
		}
		if record.Type == "session_meta" {
			var p struct {
				ID string `json:"id"`
			}
			if meta || json.Unmarshal(record.Payload, &p) != nil || p.ID != threadID {
				return nil, ErrGoalTurnUnavailable
			}
			meta = true
			continue
		}
		if !meta {
			return nil, ErrGoalTurnUnavailable
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
			return nil, ErrGoalTurnUnavailable
		}
		if p.Type == "task_started" || p.Type == "turn_started" {
			starts[p.TurnID]++
			goals[p.TurnID] = p.TurnID != "" && starts[p.TurnID] == 1 && p.Attribution != nil && p.Attribution.TurnID == p.TurnID && p.Attribution.Trigger == "goal"
		}
	}
	if scan.Err() != nil || !meta || limited.N <= 0 {
		return nil, ErrGoalTurnUnavailable
	}
	return goals, nil
}
