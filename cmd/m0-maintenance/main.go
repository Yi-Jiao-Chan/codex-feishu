// Development-only maintenance for recorded M0 test artifacts.
package main

import (
	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/config"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"
)

func main() {
	syscall.Umask(0077)
	replyProbe := len(os.Args) == 4 && os.Args[3] == "--reply-probe"
	checkOnly := len(os.Args) == 4 && os.Args[3] == "--check-only"
	uuidProbe := len(os.Args) == 4 && os.Args[3] == "--uuid-probe"
	scopeCheck := len(os.Args) == 4 && os.Args[3] == "--scope-check"
	classificationCheck := (len(os.Args) == 4 || len(os.Args) == 5) && os.Args[3] == "--classification-check"
	if len(os.Args) != 3 && !checkOnly && !replyProbe && !uuidProbe && !scopeCheck && !classificationCheck {
		fmt.Fprintln(os.Stderr, "usage: m0-maintenance CONFIG TEST_THREAD [--check-only|--reply-probe|--uuid-probe|--scope-check|--classification-check [APP_SAMPLE_THREAD]]")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cfg, e := config.Load(os.Args[1])
	if e != nil {
		panic(e)
	}
	secret, e := cfg.Secret()
	if e != nil {
		panic(e)
	}
	g := feishu.New(cfg.Feishu.AppID, secret)
	if scopeCheck {
		checkScopes(ctx, g)
		return
	}
	db, e := store.Open(cfg.Storage.DatabaseFile)
	if e != nil {
		panic(e)
	}
	defer db.Close()
	thread := os.Args[2]
	var chat string
	if e = db.DB.QueryRow("SELECT chat_id FROM m0_bindings WHERE thread_id=?", thread).Scan(&chat); e != nil {
		panic(e)
	}
	if replyProbe {
		probeReply(ctx, g, db, thread, chat)
		return
	}
	if uuidProbe {
		probeUUID(ctx, g, db, cfg, chat)
		return
	}
	d, e := codexclient.Discover(ctx, cfg.Codex.CLIPath)
	if e != nil {
		panic(e)
	}
	c, e := codexclient.Connect(ctx, d.SocketPath)
	if e != nil {
		panic(e)
	}
	defer c.Close()
	if classificationCheck {
		var samples []string
		if len(os.Args) == 5 {
			samples = append(samples, os.Args[4])
		}
		checkClassification(ctx, c, cfg, thread, samples...)
		return
	}
	turns, e := c.Turns(ctx, thread)
	if e != nil {
		panic(e)
	}
	updates := 0
	retired := 0
	if !checkOnly {
		for _, t := range turns {
			keptProgress := false
			lastAgent := ""
			for _, i := range t.Items {
				if i.Type == "agentMessage" {
					lastAgent = i.ID
				}
			}
			for _, i := range t.Items {
				body := i.PublicText()
				if body == "" {
					continue
				}
				key := fmt.Sprintf("m0.mirror.%s.%s.0", thread, i.ID)
				state, id, e := db.Operation(ctx, key)
				if e != nil || state != "confirmed" {
					continue
				}

				if i.Type == "agentMessage" && ((i.Phase != nil && *i.Phase != "final_answer") || (i.Phase == nil && i.ID != lastAgent)) {
					if t.Status == "inProgress" {
						continue
					}
					if !keptProgress {
						if e = g.UpdateCard(ctx, id, cards.Progress("已完成 · 结果见下方", "")); e != nil {
							fmt.Println(e.Error())
							os.Exit(4)
						}
						keptProgress = true
						updates++
					} else {
						if e = g.Recall(ctx, id); e != nil {
							fmt.Println(e.Error())
							os.Exit(4)
						}
						if e = db.Finish(ctx, key, "retired", id); e != nil {
							panic(e)
						}
						retired++
					}
					continue
				}
				role := "AI 回复"
				if i.Type == "userMessage" {
					role = "人类发送"
				}
				if e = g.UpdateCard(ctx, id, cards.Message(role, "Codex App", body)); e != nil {
					fmt.Println(e.Error())
					if r, ok := e.(*feishu.Rejected); ok {
						json.NewEncoder(os.Stdout).Encode(r)
					}
					os.Exit(4)
				}
				updates++
			}
		}
	}
	rs, e := db.Receipts(ctx, thread)
	if e != nil {
		panic(e)
	}
	removed := 0
	if !checkOnly {
		for _, r := range rs {
			if r.State == "consumed" && r.CardID != "" {
				if e = g.Recall(ctx, r.CardID); e != nil {
					fmt.Println(e.Error())
					os.Exit(4)
				}
				r.CardID = ""
				if e = db.SaveReceipt(ctx, r); e != nil {
					panic(e)
				}
				removed++
			}
		}
		rs, e = db.Receipts(ctx, thread)
		if e != nil {
			panic(e)
		}
		for _, r := range rs {
			if r.CardID != "" {
				if e = g.UpdateCard(ctx, r.CardID, cards.Queue(r.State, r.ActionToken)); e != nil {
					if v, ok := e.(*feishu.Rejected); ok {
						json.NewEncoder(os.Stdout).Encode(v)
					}
					os.Exit(4)
				}
			}
		}

	}
	if e = g.Chat(ctx, chat); e != nil {
		reportError(e)
		os.Exit(4)
	}
	members, e := g.MemberIDs(ctx, chat)
	if e != nil {
		reportError(e)
		os.Exit(4)
	}
	ownerPresent := false
	for _, id := range members {
		if id == cfg.Access.OwnerOpenID {
			ownerPresent = true
		}
	}
	botPresent, e := g.BotInChat(ctx, chat)
	if e != nil {
		reportError(e)
		os.Exit(4)
	}
	_, probeID, e := db.Operation(ctx, "m0-test-card")
	if e != nil {
		panic(e)
	}
	if e = g.ReadMessage(ctx, probeID); e != nil {
		reportError(e)
		os.Exit(4)
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"updated_cards": updates, "retired_progress_cards": retired, "removed_duplicate_feishu_input_cards": removed, "chat_readback": true, "owner_in_group": ownerPresent, "bot_in_group": botPresent, "own_message_readback": true, "thread_id": thread, "test_chat_id": chat})
}

func reportError(err error) {
	if r, ok := err.(*feishu.Rejected); ok {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"code": r.Code, "request_id": r.RequestID, "required_permissions": r.RequiredPermissions})
		return
	}
	fmt.Println(err.Error())
}

func probeReply(ctx context.Context, g *feishu.Gateway, db *store.Store, thread, chat string) {
	receipts, e := db.Receipts(ctx, thread)
	if e != nil {
		panic(e)
	}
	parent := ""
	for _, r := range receipts {
		if r.ChatID == chat && r.State == "steered" {
			parent = r.MessageID
			break
		}
	}
	if parent == "" {
		fmt.Fprintln(os.Stderr, "requires confirmed M0 steer input")
		os.Exit(2)
	}
	key := "m0-reply-reference-v1"
	fresh, e := db.Begin(ctx, key, "feishu.reply_probe")
	if e != nil {
		panic(e)
	}
	message := ""
	if fresh {
		message, e = g.ReplyCard(ctx, parent, cards.Progress("引用回复已启用", "正在核验原消息的回复关系。"), key)
		if e != nil {
			db.Finish(ctx, key, "unknown", "")
			reportError(e)
			os.Exit(4)
		}
		if e = db.Finish(ctx, key, "confirmed", message); e != nil {
			panic(e)
		}
	} else {
		state, id, e := db.Operation(ctx, key)
		if e != nil {
			panic(e)
		}
		if state != "confirmed" {
			fmt.Fprintln(os.Stderr, "prior reply result unknown; not retrying")
			os.Exit(5)
		}
		message = id
	}
	if e = g.UpdateCard(ctx, message, cards.Progress("引用回复已验证", "后续过程卡、最终结果和队列操作卡会引用对应输入。")); e != nil {
		reportError(e)
		os.Exit(4)
	}
	actual, e := g.MessageParent(ctx, message)
	if e != nil {
		reportError(e)
		os.Exit(4)
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"reply_message_id": message, "expected_parent": parent, "actual_parent": actual, "reply_relation_verified": actual == parent, "test_chat_id": chat})
	if actual != parent {
		os.Exit(5)
	}
}
