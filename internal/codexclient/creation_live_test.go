package codexclient

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This probe talks only to the already-running shared daemon. Its private,
// durable journal prevents an interrupted run from starting another thread.
// A normal go test never performs a live write.
func TestNativeCreationFeasibility(t *testing.T) {
	root := os.Getenv("CODEX_FEISHU_CREATION_PROBE_DIR")
	if root == "" {
		t.Skip("requires explicit shared-daemon creation probe directory")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("probe directory must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal("probe directory unavailable")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal("physical probe directory unavailable")
	}
	journal := filepath.Join(root, "creation-journal.json")
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatal("probe already claimed; inspect existing private journal, never redispatch")
	}
	record := map[string]any{"state": "prepared", "directory": root, "client_id": "cfb-creation-feasibility-" + filepath.Base(root)}
	save := func() {
		t.Helper()
		b, err := json.Marshal(record)
		if err != nil {
			t.Fatal("cannot encode private probe journal")
		}
		tmp := journal + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal("cannot create private probe journal")
		}
		_, err = f.Write(b)
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil || os.Rename(tmp, journal) != nil {
			t.Fatal("cannot persist private probe journal")
		}
		dir, err := os.Open(root)
		if err != nil {
			t.Fatal("cannot open private probe directory")
		}
		err = dir.Sync()
		dir.Close()
		if err != nil {
			t.Fatal("cannot sync private probe directory")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	daemon, err := Discover(ctx, "auto")
	if err != nil {
		t.Fatal("existing daemon unavailable")
	}
	client, err := Connect(ctx, daemon.SocketPath)
	if err != nil {
		t.Fatal("existing daemon connection unavailable")
	}
	defer client.Close()
	go func() {
		for range client.Events() {
		}
	}()
	projects, err := client.ListProjects(ctx)
	if err != nil {
		t.Fatal("native project listing unavailable")
	}
	record["project_count"] = len(projects)
	// The current deployed phase1 bridge already excludes this reserved label.
	// Exact per-intent labels are exercised by the new adapter after deployment.
	params := map[string]any{"cwd": root, "threadSource": "feishu.bridge.creation", "ephemeral": false}
	record["params"], record["state"] = params, "dispatching"
	save()
	var result CreationResult
	err = client.Call(ctx, "thread/start", params, &result)
	if err != nil {
		record["state"] = "unknown"
		var e *RPCError
		if errorsAsRPC(err, &e) {
			record["state"] = "rejected"
		}
		save()
		t.Fatal("native creation not confirmed; private journal retained")
	}
	if result.Thread.ID == "" || result.CWD != root || result.Thread.CWD != root || result.Thread.Ephemeral {
		record["state"] = "unknown"
		record["result"] = result
		save()
		t.Fatal("native creation identity mismatch")
	}
	record["result"], record["state"] = result, "thread_confirmed"
	save()
	clientID := record["client_id"].(string)
	record["state"] = "input_dispatching"
	save()
	queued, err := client.Add(ctx, result.Thread.ID, "这是飞书创建会话能力的独立验收。请只回复 FEISHU_CREATION_READY，不调用工具，不修改文件。", clientID)
	if err != nil {
		record["state"] = "input_unknown"
		save()
		t.Fatal("native initial input not confirmed; no retry")
	}
	record["queue_id"], record["state"] = queued.ID, "input_confirmed"
	save()
	deadline := time.NewTimer(55 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	found := false
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case <-deadline.C:
			break wait
		case <-tick.C:
			turns, err := client.Turns(ctx, result.Thread.ID)
			if err != nil {
				continue
			}
			for _, turn := range turns {
				for _, item := range turn.Items {
					if item.Type == "userMessage" && item.ClientID != nil && *item.ClientID == clientID {
						found = true
						record["turn_id"], record["item_id"], record["turn_status"] = turn.ID, item.ID, turn.Status
					}
				}
			}
			if found {
				break wait
			}
		}
	}
	if !found {
		record["state"] = "input_history_pending"
		save()
		t.Fatal("initial input not yet proven in complete history")
	}
	other, err := Connect(ctx, daemon.SocketPath)
	if err != nil {
		t.Fatal("second connection unavailable")
	}
	defer other.Close()
	go func() {
		for range other.Events() {
		}
	}()
	thread, err := other.Attach(ctx, result.Thread.ID)
	if err != nil || thread.ID != result.Thread.ID || thread.CWD != root {
		record["state"] = "resume_pending"
		save()
		t.Fatal("second connection cannot resume the same persistent thread")
	}
	listed, err := other.ListArchive(ctx, false)
	if err != nil {
		t.Fatal("persistent thread list unavailable")
	}
	listedSame, sourceSame := false, false
	for _, item := range listed {
		if item.ID == result.Thread.ID {
			listedSame = true
			sourceSame = string(item.ThreadSource) == `"feishu.bridge.creation"`
		}
	}
	meta, metaErr := other.Metadata(ctx, result.Thread.ID)
	metaSource := metaErr == nil && string(meta.ThreadSource) == `"feishu.bridge.creation"`
	record["listed"], record["list_source_persisted"], record["source_persisted"] = listedSame, sourceSame, metaSource
	record["state"] = "protocol_pending"
	if listedSame && metaSource {
		record["state"] = "protocol_confirmed"
	}
	save()
	if !listedSame || !metaSource {
		t.Fatal("persistent list/source evidence incomplete")
	}
	t.Log("shared daemon creation, initial history, persistent source and second-client resume confirmed; real App UI still requires observation")
}

func TestNativeCreationReadback(t *testing.T) {
	root := os.Getenv("CODEX_FEISHU_CREATION_PROBE_DIR")
	if root == "" {
		t.Skip("explicit private probe journal required")
	}
	b, err := os.ReadFile(filepath.Join(root, "creation-journal.json"))
	if err != nil {
		t.Fatal("private journal unavailable")
	}
	var record struct {
		Result CreationResult `json:"result"`
	}
	if json.Unmarshal(b, &record) != nil || record.Result.Thread.ID == "" {
		t.Fatal("private thread identity unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := Discover(ctx, "auto")
	if err != nil {
		t.Fatal("existing daemon unavailable")
	}
	c, err := Connect(ctx, d.SocketPath)
	if err != nil {
		t.Fatal("cannot connect")
	}
	defer c.Close()
	go func() {
		for range c.Events() {
		}
	}()
	meta, err := c.Metadata(ctx, record.Result.Thread.ID)
	if err != nil {
		t.Fatal("metadata unavailable")
	}
	t.Logf("read source=%s id_matches=%t cwd_matches=%t path_present=%t", string(meta.ThreadSource), meta.ID == record.Result.Thread.ID, meta.CWD == root, meta.Path != nil)
	turns, err := c.Turns(ctx, meta.ID)
	if err != nil {
		t.Fatal("same-thread history unavailable")
	}
	appInput, appReply := false, false
	for _, turn := range turns {
		for _, item := range turn.Items {
			if strings.Contains(item.PublicText(), "APP_CREATION_CONTINUE_OK") {
				if item.Type == "userMessage" {
					appInput = true
				}
				if item.Type == "agentMessage" {
					appReply = true
				}
			}
		}
	}
	t.Logf("App continuation in original thread: user_input=%t assistant_reply=%t", appInput, appReply)
	list, err := c.ListArchive(ctx, false)
	if err != nil {
		t.Fatal("list unavailable")
	}
	for _, item := range list {
		if item.ID == meta.ID {
			t.Logf("list source=%s", string(item.ThreadSource))
			return
		}
	}
	t.Fatal("thread not listed")
}

func errorsAsRPC(err error, out **RPCError) bool {
	for err != nil {
		if e, ok := err.(*RPCError); ok {
			*out = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
