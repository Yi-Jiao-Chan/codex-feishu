package app

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type pinnedNative interface {
	Sections(context.Context) ([]codexclient.ThreadSection, error)
	ListSection(context.Context, string, bool) ([]codexclient.Thread, error)
}

func (r *Runtime) preparePinning(ctx context.Context) (pinnedNative, store.PinnedSnapshot, bool) {
	native, supported := r.Native.(pinnedNative)
	if !supported || r.SyncGroupName == nil {
		return nil, store.PinnedSnapshot{}, false
	}
	previous, err := r.Store.PinnedSnapshot(ctx, r.HostID, r.AppID)
	if err != nil {
		previous = store.PinnedSnapshot{}
	}
	r.mu.Lock()
	r.pinSupported = true
	r.pinKnown = err == nil
	r.pinSectionID = previous.SectionID
	r.mu.Unlock()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.notice("pins.checkpoint_pending")
	}
	return native, previous, true
}

// Only the complete catalog can discover a section identity. A previously
// confirmed identity takes precedence over a mutable display name.
func pinnedSectionID(sections []codexclient.ThreadSection, previous string) (string, error) {
	seen := make(map[string]bool, len(sections))
	var matching []string
	for _, section := range sections {
		if strings.TrimSpace(section.ID) == "" || seen[section.ID] {
			return "", errors.New("invalid native section catalog")
		}
		seen[section.ID] = true
		if section.Name == "Pinned" {
			matching = append(matching, section.ID)
		}
	}
	if previous != "" && seen[previous] {
		return previous, nil
	}
	if len(matching) > 1 {
		return "", errors.New("pinned section identity is ambiguous")
	}
	if len(matching) == 1 {
		return matching[0], nil
	}
	return "", nil
}

func readPinnedSnapshot(ctx context.Context, native pinnedNative, previous string, at int64) (store.PinnedSnapshot, error) {
	sections, err := native.Sections(ctx)
	if err != nil {
		return store.PinnedSnapshot{}, err
	}
	id, err := pinnedSectionID(sections, previous)
	if err != nil {
		return store.PinnedSnapshot{}, err
	}
	p := store.PinnedSnapshot{SectionID: id, ObservedAtMS: at}
	if id == "" {
		return p, nil
	}
	seen := map[string]bool{}
	for _, archived := range []bool{false, true} {
		threads, err := native.ListSection(ctx, id, archived)
		if err != nil {
			return store.PinnedSnapshot{}, err
		}
		for _, thread := range threads {
			if thread.ID == "" || thread.Section == nil || thread.Section.ID != id {
				return store.PinnedSnapshot{}, errors.New("invalid pinned section page")
			}
			seen[thread.ID] = true
		}
	}
	for thread := range seen {
		p.ThreadIDs = append(p.ThreadIDs, thread)
	}
	sort.Strings(p.ThreadIDs)
	return p, nil
}

func pinChanges(before, after []string) map[string]bool {
	changed := make(map[string]bool, len(before)+len(after))
	for _, id := range before {
		changed[id] = true
	}
	for _, id := range after {
		if changed[id] {
			delete(changed, id)
		} else {
			changed[id] = true
		}
	}
	return changed
}

// Poll one section per host, never every historical thread. An incomplete read
// or checkpoint failure cannot replace the last successful complete snapshot.
func (r *Runtime) runPins(ctx context.Context, native pinnedNative, previous store.PinnedSnapshot) {
	interval := r.PinScanInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		current, err := readPinnedSnapshot(readCtx, native, previous.SectionID, r.now().UnixMilli())
		if err == nil {
			err = r.Store.SavePinnedSnapshot(readCtx, r.HostID, r.AppID, current)
		}
		cancel()
		if err != nil {
			r.notice("pins.scan_pending")
		} else {
			changed := pinChanges(previous.ThreadIDs, current.ThreadIDs)
			if previous.SectionID != current.SectionID {
				for _, id := range append(append([]string(nil), previous.ThreadIDs...), current.ThreadIDs...) {
					changed[id] = true
				}
			}
			r.mu.Lock()
			first := !r.pinKnown
			r.pinKnown, r.pinSectionID = true, current.SectionID
			if first {
				for id := range r.actors {
					changed[id] = true
				}
			}
			for id := range changed {
				if actor := r.actors[id]; actor != nil {
					select {
					case actor.wake <- struct{}{}:
					default:
					}
				}
			}
			r.mu.Unlock()
			previous = current
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (r *Runtime) groupTitle(meta codexclient.Thread, turns []codexclient.Turn) (string, bool) {
	r.mu.Lock()
	supported, known, id := r.pinSupported, r.pinKnown, r.pinSectionID
	r.mu.Unlock()
	pinned := supported && known && id != "" && meta.Section != nil && meta.Section.ID == id
	return threadGroupTitleWithPin(meta, turns, pinned), !supported || known
}
