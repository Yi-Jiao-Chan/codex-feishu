package store

import (
	"context"
	"reflect"
	"sync"
	"testing"
)

func TestCommandGuardCandidatesReadOnlyPreservesDispatchFilters(t *testing.T) {
	ctx := context.Background()
	s, req, now := commandStoreRequest(t)
	req.Kind = "help"
	var replies []CommandReply
	for _, message := range []string{"throttled", "unknown", "dispatching", "first-ready", "second-ready"} {
		req.MessageID = message
		r, _, err := s.ApplyLocalCommand(ctx, req, now)
		if err != nil {
			t.Fatal(err)
		}
		replies = append(replies, r)
	}
	at := now.UnixMilli()
	first, claimed, err := s.ClaimCommandReply(ctx, req.HostID, at)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err := s.DeferCommandReplyAttempt(ctx, first.ID, at, 5000); err != nil {
		t.Fatal(err)
	}
	second, claimed, err := s.ClaimCommandReply(ctx, req.HostID, at)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err := s.FinishCommandReply(ctx, second.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimCommandReply(ctx, req.HostID, at); err != nil || !claimed {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err := s.DB.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	reader, _, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, limit := range []int{1, 64} {
		candidates, err := reader.CommandReplyCandidates(ctx, req.HostID, at, limit)
		want := replies[3:]
		if limit == 1 {
			want = want[:1]
		}
		if err != nil || !reflect.DeepEqual(candidates, want) {
			t.Fatalf("read-only candidates: %+v %v", candidates, err)
		}
	}
	if candidates, err := reader.CommandReplyCandidates(ctx, "different-host", at, 64); err != nil || len(candidates) != 0 {
		t.Fatal("candidate query escaped host", err)
	}
	if candidates, err := reader.CommandReplyCandidates(ctx, req.HostID, at+5000, 64); err != nil || len(candidates) != 3 || candidates[0].MessageID != "throttled" {
		t.Fatal("due throttle filtering changed", err)
	}
	var attempts int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_reply_attempts`).Scan(&attempts); err != nil || attempts != 3 {
		t.Fatalf("query wrote attempts: %d %v", attempts, err)
	}
	for _, r := range replies[3:] {
		saved, err := s.CommandReply(ctx, r.HostID, r.AppID, r.MessageID)
		if err != nil || saved != r {
			t.Fatal("query mutated snapshot", err)
		}
	}
}

func TestCommandGuardCandidateClaimsExactOriginalAcrossCompetition(t *testing.T) {
	ctx := context.Background()
	s, req, now := commandStoreRequest(t)
	req.Kind = "help"
	for _, message := range []string{"first", "second"} {
		req.MessageID = message
		if _, _, err := s.ApplyLocalCommand(ctx, req, now); err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := s.CommandReplyCandidates(ctx, req.HostID, now.UnixMilli(), 64)
	if err != nil || len(candidates) != 2 {
		t.Fatal(err)
	}
	type result struct {
		reply   CommandReply
		claimed bool
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			r, claimed, err := s.ClaimCommandReplyCandidate(ctx, candidates[0], now.UnixMilli())
			results <- result{r, claimed, err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	claims := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.claimed {
			claims++
			if result.reply.ID != candidates[0].ID || result.reply.MessageID != "first" || result.reply.State != "dispatching" {
				t.Fatal("competing claim selected another candidate")
			}
		}
	}
	if claims != 1 {
		t.Fatalf("claims=%d", claims)
	}
	second, err := s.CommandReply(ctx, req.HostID, req.AppID, "second")
	if err != nil || second != candidates[1] {
		t.Fatal("second candidate was partially claimed", err)
	}
	var attempts int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_reply_attempts`).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatal("extra attempt", attempts, err)
	}
}

func TestCommandGuardCandidateRejectsChangedSnapshotAndDeferral(t *testing.T) {
	for _, field := range []string{"chat_id", "user_id", "thread_id", "card_json", "deferral"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			s, req, now := commandStoreRequest(t)
			req.Kind = "help"
			original, _, err := s.ApplyLocalCommand(ctx, req, now)
			if err != nil {
				t.Fatal(err)
			}
			if field == "deferral" {
				r, claimed, err := s.ClaimCommandReplyCandidate(ctx, original, now.UnixMilli())
				if err != nil || !claimed {
					t.Fatal(err)
				}
				if err := s.DeferCommandReplyAttempt(ctx, r.ID, now.UnixMilli(), 1000); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.DB.Exec(`UPDATE command_replies SET `+field+`='changed' WHERE attempt_id=?`, original.ID); err != nil {
				t.Fatal(err)
			}
			if r, claimed, err := s.ClaimCommandReplyCandidate(ctx, original, now.UnixMilli()+2000); err != nil || claimed || r != (CommandReply{}) {
				t.Fatal("stale grant acquired a changed candidate", claimed, err)
			}
			saved, err := s.CommandReply(ctx, original.HostID, original.AppID, original.MessageID)
			if err != nil || saved.State != "ready" {
				t.Fatal("stale candidate changed state", err)
			}
			var attempts int
			want := 0
			if field == "deferral" {
				want = 1
			}
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM command_reply_attempts`).Scan(&attempts); err != nil || attempts != want {
				t.Fatal("stale candidate wrote attempt", attempts, err)
			}
		})
	}
}
