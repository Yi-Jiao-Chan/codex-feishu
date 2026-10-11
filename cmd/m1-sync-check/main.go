// m1-sync-check reads one explicitly selected existing daemon thread. Its
// isolated local eligibility fixture is not automatic-discovery evidence.
// No resume, native input, Feishu writes or native request answers are issued.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
	messagesync "codex-feishu/internal/sync"
)

func main() {
	syscall.Umask(0077)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:]))
}

func emit(state string, data any) {
	json.NewEncoder(os.Stdout).Encode(map[string]any{"state": state, "remote_read_only": true, "eligibility_fixture_only": true, "phase1_released": false, "groups_created": 0, "messages_sent": 0, "data": data})
}

func run(ctx context.Context, args []string) int {
	f := flag.NewFlagSet("m1-sync-check", flag.ContinueOnError)
	configPath := f.String("config", "", "absolute TOML config path")
	database := f.String("database", "", "separate absolute development database path")
	threadID := f.String("thread", "", "explicit existing interactive main thread")
	importReceipts := f.Bool("import-m0-receipts", false, "read-only import of matching receipt identity fields; no bodies")
	if err := f.Parse(args); err != nil || len(f.Args()) != 0 || *threadID == "" {
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
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	d, err := codexclient.Discover(ctx, cfg.Codex.CLIPath)
	if err != nil || d.AppServerVersion != "0.162.0" {
		emit("compatible-existing-daemon-required", nil)
		return 3
	}
	c, err := codexclient.Connect(ctx, d.SocketPath)
	if err != nil {
		emit("handshake-unavailable", nil)
		return 3
	}
	var reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case _, open := <-c.Events():
				if !open {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); c.Close(); reader.Wait() }()
	t, err := c.Metadata(ctx, *threadID)
	if err != nil || t.ID != *threadID || !t.InteractiveMain() || t.ForkedFromID != nil {
		emit("interactive-main-required-forks-deferred", nil)
		return 3
	}
	scope, err := rules.ResolveScope(ctx, t.CWD, cfg.Discovery.AllowedRoots)
	if err != nil || !scope.Allowed {
		emit("allowed-directory-required", nil)
		return 3
	}
	turns, err := c.Turns(ctx, *threadID)
	if err != nil || len(turns) == 0 {
		emit("complete-existing-history-required", nil)
		return 3
	}
	firstUser := ""
	for _, turn := range turns {
		for _, item := range turn.Items {
			if item.Type == "userMessage" && firstUser == "" {
				firstUser = item.ID
			}
		}
	}
	if firstUser == "" {
		emit("actual-input-required", nil)
		return 3
	}
	// This is deliberately a development fixture, isolated from discovery and
	// the configured live DB. It permits testing real content without a group.
	host := cfg.Host.ID + ":sync-probe"
	now := time.Now().UnixMilli()
	if _, err = s.BeginBaseline(ctx, host, cfg.Feishu.AppID, now); err == nil {
		err = s.ObserveNew(ctx, host, *threadID, scope.CWD, t.CreatedAt)
	}
	if err == nil {
		err = s.MarkEligible(ctx, host, *threadID, firstUser, scope.CWD, scope.ProjectRoot)
	}
	if err != nil {
		emit("fixture-persistence-failed", nil)
		return 7
	}
	copied := 0
	if *importReceipts {
		copied, err = copyReceiptIdentities(ctx, cfg.Storage.DatabaseFile, s, cfg.Feishu.AppID, *threadID)
		if err != nil {
			emit("read-only-receipt-import-failed", nil)
			return 7
		}
	}
	e := messagesync.Engine{Store: s, Native: c, HostID: host}
	if err = e.Apply(ctx, *threadID, turns, true, now); err != nil {
		emit("public-history-staging-failed", nil)
		return 7
	}
	before, err := s.Mirrors(ctx, host, *threadID)
	if err != nil {
		return 7
	}
	if err = e.Reconcile(ctx, *threadID, true, time.Now().UnixMilli()); err != nil {
		emit("second-native-scan-failed", nil)
		return 3
	}
	after, err := s.Mirrors(ctx, host, *threadID)
	if err != nil {
		return 7
	}
	counts := map[string]int{}
	maxBytes := 0
	for _, m := range after {
		counts[m.Kind]++
		if len(m.Card) > maxBytes {
			maxBytes = len(m.Card)
		}
	}
	links, err := s.InputLinks(ctx, host, *threadID)
	if err != nil {
		return 7
	}
	unchanged := sameDocuments(before, after)
	emit("sync-check-completed", map[string]any{"server_version": d.AppServerVersion, "turns": len(turns), "card_counts": counts, "receipt_identities_imported": copied, "exact_input_links": len(links), "serialized_max_card_bytes": maxBytes, "second_scan_unchanged": unchanged, "replay": true})
	if !unchanged {
		return 4 // A live turn may legitimately advance between the two scans.
	}
	return 0
}

func sameDocuments(a, b []store.Mirror) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	ap, ae := filepath.EvalSymlinks(filepath.Dir(a))
	bp, be := filepath.EvalSymlinks(filepath.Dir(b))
	return ae == nil && be == nil && ap == bp && filepath.Base(a) == filepath.Base(b)
}

func copyReceiptIdentities(ctx context.Context, source string, dest *store.Store, app, thread string) (int, error) {
	u := url.URL{Scheme: "file", Path: source, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `PRAGMA query_only=ON; PRAGMA busy_timeout=5000;`); err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id,message_id,chat_id,client_id,state FROM m0_receipts WHERE app_id=? AND thread_id=?`, app, thread)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, message, chat, client, state string
		if err := rows.Scan(&id, &message, &chat, &client, &state); err != nil {
			return count, err
		}
		_, err = dest.DB.ExecContext(ctx, `INSERT OR IGNORE INTO m0_receipts(id,app_id,message_id,thread_id,chat_id,body,client_id,state,action_token) VALUES(?,?,?,?,?,'',?,?,?)`, id, app, message, thread, chat, client, state, store.ID())
		if err != nil {
			return count, err
		}
		var matching int
		if err = dest.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM m0_receipts WHERE id=? AND app_id=? AND message_id=? AND thread_id=? AND chat_id=? AND client_id=? AND body=''`, id, app, message, thread, chat, client).Scan(&matching); err != nil {
			return count, err
		}
		if matching != 1 {
			return count, errors.New("receipt identity conflict")
		}
		count++
	}
	return count, rows.Err()
}
