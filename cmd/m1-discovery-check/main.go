// m1-discovery-check is a remote-read-only development probe. It records local
// discovery evidence in a separate database; it never resumes threads, creates
// groups, sends messages or answers native approval/question requests.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/discovery"
	"codex-feishu/internal/store"
)

func main() {
	syscall.Umask(0077)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:]))
}

func emit(state string, data any) {
	json.NewEncoder(os.Stdout).Encode(map[string]any{"state": state, "remote_read_only": true, "phase1_released": false, "data": data})
}

func run(ctx context.Context, args []string) int {
	f := flag.NewFlagSet("m1-discovery-check", flag.ContinueOnError)
	configPath := f.String("config", "", "absolute TOML config path")
	database := f.String("database", "", "separate absolute development database path")
	watchSeconds := f.Int("watch-seconds", 0, "after initial scan, observe for this many seconds (0..3600)")
	inspectID := f.String("inspect-thread", "", "optional explicit sample thread ID; prints classification only")
	if err := f.Parse(args); err != nil || len(f.Args()) != 0 || *watchSeconds < 0 || *watchSeconds > 3600 {
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		emit("invalid-config", nil)
		return 2
	}
	if !filepath.IsAbs(*database) || samePath(*database, cfg.Storage.DatabaseFile) {
		emit("separate-probe-database-required", nil)
		return 2
	}
	lock, err := store.AcquireListener(*database)
	if err != nil {
		emit("probe-lock-conflict", nil)
		return 6
	}
	defer lock.Close()
	s, err := store.Open(*database)
	if err != nil {
		emit("storage-unavailable", nil)
		return 7
	}
	defer s.Close()
	e := &discovery.Engine{Store: s, HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, AllowedRoots: cfg.Discovery.AllowedRoots}
	if _, err = e.Begin(ctx); err != nil {
		emit("baseline-conflict", nil)
		return 7
	}
	callCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	d, err := codexclient.Discover(callCtx, cfg.Codex.CLIPath)
	stop()
	if err != nil || d.AppServerVersion != "0.162.0" {
		emit("compatible-existing-daemon-required", nil)
		return 3
	}
	c, err := codexclient.Connect(ctx, d.SocketPath)
	if err != nil {
		emit("handshake-unavailable", nil)
		return 3
	}
	workCtx, workCancel := context.WithCancel(ctx)
	ctx = workCtx
	var workers sync.WaitGroup
	defer func() { workCancel(); c.Close(); workers.Wait() }()
	e.Native = c
	cycle := make(chan error, 1)
	startCycle := func() {
		workers.Add(1)
		go func() {
			defer workers.Done()
			cycleCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			if err := e.Snapshot(cycleCtx); err != nil {
				cycle <- err
				return
			}
			cycle <- e.Scan(cycleCtx)
		}()
	}
	startCycle()
	emit("discovery-check-started", map[string]any{"server_version": d.AppServerVersion, "fork_following": "deferred"})
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	var watchEnd <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	busy := true
	for {
		select {
		case event, open := <-c.Events():
			if !open {
				emit("native-connection-ended", nil)
				return 3
			}
			if event.Method != "thread/started" || len(event.RequestID) != 0 {
				continue
			}
			var p struct {
				Thread codexclient.Thread `json:"thread"`
			}
			if json.Unmarshal(event.Params, &p) != nil || p.Thread.ID == "" {
				emit("invalid-start-notification", nil)
				return 3
			}
			if err = e.ObserveBirth(ctx, p.Thread); err != nil {
				emit("birth-persistence-failed", nil)
				return 7
			}
		case err = <-cycle:
			busy = false
			if ctx.Err() != nil {
				return 0
			}
			fence, complete, dbErr := s.BaselineFence(ctx, cfg.Host.ID)
			if dbErr != nil {
				emit("baseline-read-failed", nil)
				return 7
			}
			if !complete {
				emit("baseline-incomplete", map[string]any{"retry_safe": true})
				return 3
			}
			var counts []map[string]any
			rows, dbErr := s.DB.QueryContext(ctx, "SELECT state,COUNT(*) FROM thread_observations WHERE host_id=? GROUP BY state ORDER BY state", cfg.Host.ID)
			if dbErr != nil {
				return 7
			}
			for rows.Next() {
				var state string
				var count int
				if dbErr = rows.Scan(&state, &count); dbErr != nil {
					rows.Close()
					return 7
				}
				counts = append(counts, map[string]any{"state": state, "count": count})
			}
			dbErr = rows.Err()
			rows.Close()
			if dbErr != nil {
				return 7
			}
			data := map[string]any{"baseline_complete": complete, "baseline_fence_ms": fence, "counts": counts, "scan_has_unavailable_candidates": err != nil, "groups_created": 0, "messages_sent": 0}
			if *inspectID != "" {
				o, readErr := s.Observation(ctx, cfg.Host.ID, *inspectID)
				if readErr == nil {
					data["sample"] = map[string]any{"thread_id": o.ThreadID, "origin": o.Origin, "state": o.State, "proof": o.Proof, "reason": o.ExclusionReason}
				}
			}
			emit("discovery-check-completed", data)
			if *watchSeconds == 0 {
				return 0
			}
			if timer == nil {
				timer = time.NewTimer(time.Duration(*watchSeconds) * time.Second)
				watchEnd = timer.C
			}
		case <-ticker.C:
			if !busy {
				busy = true
				startCycle()
			}
		case <-watchEnd:
			return 0
		case <-ctx.Done():
			return 0
		}
	}
}

func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	x, xe := os.Stat(a)
	y, ye := os.Stat(b)
	if xe == nil && ye == nil && os.SameFile(x, y) {
		return true
	}
	aParent, ae := filepath.EvalSymlinks(filepath.Dir(a))
	bParent, be := filepath.EvalSymlinks(filepath.Dir(b))
	return ae == nil && be == nil && aParent == bParent && filepath.Base(a) == filepath.Base(b)
}
