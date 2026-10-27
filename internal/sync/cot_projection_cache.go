package sync

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"hash"
	"io"
	"strings"
	"sync"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
)

const (
	defaultCOTProjectionCacheBytes   = 32 << 20
	defaultCOTProjectionCacheEntries = 8192
)

// COTProjectionCache retains only bounded, already-redacted tool projections.
// A Runtime shares one cache across its actors, so dormant routes can resume
// without reprocessing every old tool result. Native payloads are represented
// only by digests and never enter the cache.
type COTProjectionCache struct {
	mu         sync.Mutex
	maxBytes   int
	maxEntries int
	bytes      int
	entries    map[[32]byte]*list.Element
	lru        list.List
	hits       uint64
	misses     uint64
	evictions  uint64
}

type cotProjectionCacheEntry struct {
	key        [32]byte
	sourceHash [32]byte
	item       cot.Item
	bytes      int
}

type cotProjectionCacheStats struct {
	bytes, entries          int
	hits, misses, evictions uint64
}

// NewCOTProjectionCache returns the process-local cache used by one native
// Runtime generation. Restarting the Runtime deliberately starts cold.
func NewCOTProjectionCache() *COTProjectionCache {
	return newCOTProjectionCache(defaultCOTProjectionCacheBytes, defaultCOTProjectionCacheEntries)
}

func newCOTProjectionCache(maxBytes, maxEntries int) *COTProjectionCache {
	if maxBytes < 1 {
		maxBytes = 1
	}
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &COTProjectionCache{maxBytes: maxBytes, maxEntries: maxEntries, entries: make(map[[32]byte]*list.Element)}
}

func (c *COTProjectionCache) stats() cotProjectionCacheStats {
	if c == nil {
		return cotProjectionCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cotProjectionCacheStats{bytes: c.bytes, entries: len(c.entries), hits: c.hits, misses: c.misses, evictions: c.evictions}
}

func cotToolProjectionType(typ string) bool {
	switch typ {
	case "commandExecution", "mcpToolCall", "dynamicToolCall", "fileChange", "webSearch", "imageView", "imageGeneration", "collabAgentToolCall":
		return true
	default:
		return false
	}
}

func writeCOTCachePart(h hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = io.WriteString(h, value)
}

func cotProjectionCacheKey(thread, turn, item string, version int) [32]byte {
	h := sha256.New()
	var encodedVersion [8]byte
	binary.BigEndian.PutUint64(encodedVersion[:], uint64(version))
	_, _ = h.Write(encodedVersion[:])
	writeCOTCachePart(h, thread)
	writeCOTCachePart(h, turn)
	writeCOTCachePart(h, item)
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func cotProjectionSourceHash(item codexclient.Item) ([32]byte, error) {
	h := sha256.New()
	if err := json.NewEncoder(h).Encode(item); err != nil {
		return [32]byte{}, err
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result, nil
}

func copyCOTProjection(item cot.Item) cot.Item {
	result := item
	result.TimestampMS = 0
	result.Details = append([]string(nil), item.Details...)
	if item.ExitCode != nil {
		value := *item.ExitCode
		result.ExitCode = &value
	}
	if item.DurationMS != nil {
		value := *item.DurationMS
		result.DurationMS = &value
	}
	return result
}

func retainCOTProjection(item cot.Item) cot.Item {
	result := copyCOTProjection(item)
	result.ID = strings.Clone(result.ID)
	result.Kind = strings.Clone(result.Kind)
	result.Text = strings.Clone(result.Text)
	result.Title = strings.Clone(result.Title)
	result.Status = strings.Clone(result.Status)
	result.Icon = strings.Clone(result.Icon)
	result.ToolName = strings.Clone(result.ToolName)
	result.ResultSummary = strings.Clone(result.ResultSummary)
	result.InputPreview = strings.Clone(result.InputPreview)
	result.OutputPreview = strings.Clone(result.OutputPreview)
	for n := range result.Details {
		result.Details[n] = strings.Clone(result.Details[n])
	}
	return result
}

// cotProjectionRetainedBytes charges the retained strings plus a conservative
// allowance for the entry, map and list bookkeeping. The separate entry limit
// also bounds fixed allocator overhead.
func cotProjectionRetainedBytes(item cot.Item) int {
	bytes := 384 + len(item.ID) + len(item.Kind) + len(item.Text) + len(item.Title) + len(item.Status) + len(item.Icon) +
		len(item.ToolName) + len(item.ResultSummary) + len(item.InputPreview) + len(item.OutputPreview)
	for _, detail := range item.Details {
		bytes += 24 + len(detail)
	}
	return bytes
}

func (c *COTProjectionCache) get(key, sourceHash [32]byte) (cot.Item, bool) {
	if c == nil {
		return cot.Item{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element := c.entries[key]
	if element == nil {
		c.misses++
		return cot.Item{}, false
	}
	entry := element.Value.(*cotProjectionCacheEntry)
	if entry.sourceHash != sourceHash {
		c.remove(element)
		c.misses++
		return cot.Item{}, false
	}
	c.lru.MoveToFront(element)
	c.hits++
	return copyCOTProjection(entry.item), true
}

func (c *COTProjectionCache) put(key, sourceHash [32]byte, item cot.Item) {
	if c == nil {
		return
	}
	item = retainCOTProjection(item)
	entryBytes := cotProjectionRetainedBytes(item)
	if entryBytes > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.entries[key]; old != nil {
		c.remove(old)
	}
	entry := &cotProjectionCacheEntry{key: key, sourceHash: sourceHash, item: item, bytes: entryBytes}
	c.entries[key] = c.lru.PushFront(entry)
	c.bytes += entryBytes
	for c.bytes > c.maxBytes || len(c.entries) > c.maxEntries {
		c.remove(c.lru.Back())
		c.evictions++
	}
}

func (c *COTProjectionCache) remove(element *list.Element) {
	if element == nil {
		return
	}
	entry := element.Value.(*cotProjectionCacheEntry)
	delete(c.entries, entry.key)
	c.bytes -= entry.bytes
	c.lru.Remove(element)
}

func (c *COTProjectionCache) projectTool(thread, turn string, version int, item codexclient.Item) (cot.Item, bool) {
	if !cotToolProjectionType(item.Type) {
		return cot.Item{}, false
	}
	if c == nil {
		return projectCOTTool(version, item)
	}
	key := cotProjectionCacheKey(thread, turn, item.ID, version)
	sourceHash, err := cotProjectionSourceHash(item)
	if err == nil {
		if projected, ok := c.get(key, sourceHash); ok {
			return projected, true
		}
	}
	projected, ok := projectCOTTool(version, item)
	if !ok {
		return cot.Item{}, false
	}
	if err == nil {
		c.put(key, sourceHash, projected)
	}
	return projected, true
}

func projectCOTTool(version int, item codexclient.Item) (cot.Item, bool) {
	tool, ok := ProgressToolSummary(item)
	if !ok {
		return cot.Item{}, false
	}
	projected := cot.Item{ID: item.ID, Kind: "tool", Title: cards.Safe(tool.Title), Status: tool.Status, Icon: cotToolIcon(tool.Kind)}
	if version == cot.Version {
		projected = cotToolDetails(item, tool)
	}
	return projected, true
}
