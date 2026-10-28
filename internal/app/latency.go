package app

import (
	"encoding/json"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/latency"
)

func (r *Runtime) observeLatency(stage, thread string, started time.Time, err error) {
	r.Latency.Record(stage, r.HostID, thread, "", "", time.Since(started), latency.Result(err))
}

// Only lifecycle checkpoints of public messages are recorded. In particular,
// deltas, reasoning, tool arguments and native error text never reach the sink.
func (r *Runtime) observeNativeLatency(event codexclient.Event) {
	if r.Latency == nil {
		return
	}
	switch event.Method {
	case "thread/started", "turn/started", "turn/completed", "item/started", "item/completed":
	default:
		return
	}
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Thread   struct {
			ID string `json:"id"`
		} `json:"thread"`
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
		Item struct {
			ID, Type, Phase string
		} `json:"item"`
	}
	if json.Unmarshal(event.Params, &p) != nil {
		return
	}
	stage := ""
	switch event.Method {
	case "thread/started":
		p.ThreadID, stage = p.Thread.ID, "native.thread.started"
	case "turn/started":
		p.TurnID, stage = p.Turn.ID, "native.turn.started"
	case "turn/completed":
		p.TurnID, stage = p.Turn.ID, "native.turn.completed"
	case "item/started", "item/completed":
		kind := ""
		switch p.Item.Type {
		case "userMessage":
			kind = "user"
		case "agentMessage":
			kind = "message"
			if p.Item.Phase == "final_answer" {
				kind = "answer"
			} else if p.Item.Phase == "commentary" {
				kind = "commentary"
			}
		}
		if kind != "" {
			suffix := "started"
			if event.Method == "item/completed" {
				suffix = "completed"
			}
			stage = "native." + kind + "." + suffix
		}
	}
	if stage != "" && p.ThreadID != "" {
		r.Latency.Record(stage, r.HostID, p.ThreadID, p.TurnID, p.Item.ID, 0, "observed")
	}
}
