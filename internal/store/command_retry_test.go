package store

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/rules"
)

func retryCommandClaim(t *testing.T, s *Store, at int64) CommandReply {
	t.Helper()
	a, claimed, err := s.ClaimCommandReply(context.Background(), "host", at)
	if err != nil || !claimed {
		t.Fatal("command not claimed", err)
	}
	return a
}

func TestCommandRetryKeepsCommandEffectAndOriginalReplyAcrossRestart(t *testing.T) {
	ctx := context.Background()
	s, req, now := commandStoreRequest(t)
	original, _, err := s.ApplyLocalCommand(ctx, req, now)
	if err != nil {
		t.Fatal(err)
	}
	a := retryCommandClaim(t, s, now.UnixMilli())
	if err := s.DeferCommandReplyAttempt(ctx, a.ID, now.UnixMilli()+1000, 7000); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferCommandReplyAttempt(ctx, a.ID, now.UnixMilli()+9000, 99000); err != nil {
		t.Fatal(err)
	}
	h, err := s.Health(ctx, SchemaVersion, "host", "app")
	if err != nil || h.CommandReplies["deferred"] != 1 || h.CommandAttempts["rejected"] != 1 {
		t.Fatal("waiting omitted from health", err)
	}
	remove := req
	remove.MessageID = "unmute"
	remove.Directory.Action = rules.DirectoryUnmute
	if _, _, err := s.ApplyLocalCommand(ctx, remove, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	later := retryCommandClaim(t, s, now.UnixMilli()+2000)
	if later.MessageID != "unmute" {
		t.Fatal("blocked first reply starved later command")
	}
	if err := s.FinishCommandReply(ctx, later.ID, "confirmed", "unmute-result"); err != nil {
		t.Fatal(err)
	}
	var sequence int
	var name, path string
	if err := s.DB.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverCommandDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimCommandReply(ctx, "host", now.UnixMilli()+7999); err != nil || claimed {
		t.Fatal("restart ignored server floor", err)
	}
	replay, fresh, err := s.ApplyLocalCommand(ctx, req, now.Add(time.Hour))
	if err != nil || fresh || replay.Card != original.Card {
		t.Fatal("replay changed result", err)
	}
	remaining, err := s.DirectoryRules(ctx, "host", "app")
	if err != nil || len(remaining) != 0 {
		t.Fatal("reply retry reapplied original mute", err)
	}
	b := retryCommandClaim(t, s, now.UnixMilli()+8000)
	if b.ID == a.ID || b.Card != a.Card || b.MessageID != a.MessageID || b.ChatID != a.ChatID || b.UserID != a.UserID || b.CreatedAtMS != a.CreatedAtMS {
		t.Fatal("retry lost frozen reply identity")
	}
	if err := s.FinishCommandReply(ctx, a.ID, "confirmed", "stale-result"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale result finished retry", err)
	}
	if err := s.FinishCommandReply(ctx, b.ID, "confirmed", "correct-result"); err != nil {
		t.Fatal(err)
	}
	var state, card string
	if err := s.DB.QueryRow("SELECT state,card_json FROM command_reply_attempts WHERE attempt_id=?", a.ID).Scan(&state, &card); err != nil || state != "rejected" || card != a.Card {
		t.Fatal("rejected attempt overwritten", err)
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM command_reply_throttles").Scan(&count); err != nil || count != 0 {
		t.Fatal("confirmed reply retained retry", err)
	}
}

func TestCommandRetryAtomicDeferralConfirmationAndRecovery(t *testing.T) {
	for _, stage := range []string{"defer", "confirm"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			s, req, now := commandStoreRequest(t)
			req.Kind = "help"
			if _, _, err := s.ApplyLocalCommand(ctx, req, now); err != nil {
				t.Fatal(err)
			}
			a := retryCommandClaim(t, s, now.UnixMilli())
			if _, err := s.DB.Exec(`CREATE TRIGGER fail_reply_outcome BEFORE UPDATE ON command_replies BEGIN SELECT RAISE(FAIL,'fixture'); END;`); err != nil {
				t.Fatal(err)
			}
			var err error
			if stage == "defer" {
				err = s.DeferCommandReplyAttempt(ctx, a.ID, now.UnixMilli(), 0)
			} else {
				err = s.FinishCommandReply(ctx, a.ID, "confirmed", "remote")
			}
			if err == nil {
				t.Fatal("SQL fault accepted")
			}
			var state string
			if err := s.DB.QueryRow("SELECT state FROM command_reply_attempts WHERE attempt_id=?", a.ID).Scan(&state); err != nil || state != "dispatching" {
				t.Fatal("half-committed attempt", err)
			}
			var n int
			if err := s.DB.QueryRow("SELECT count(*) FROM command_reply_throttles").Scan(&n); err != nil || n != 0 {
				t.Fatal("half-committed retry", err)
			}
			if _, err := s.DB.Exec("DROP TRIGGER fail_reply_outcome"); err != nil {
				t.Fatal(err)
			}
			if err := s.RecoverCommandDispatches(ctx, "host"); err != nil {
				t.Fatal(err)
			}
			if _, claimed, err := s.ClaimCommandReply(ctx, "host", now.Add(time.Hour).UnixMilli()); err != nil || claimed {
				t.Fatal("ambiguous outcome retried", err)
			}
			if err := s.DeferCommandReplyAttempt(ctx, a.ID, now.UnixMilli()+9000, 0); !errors.Is(err, ErrConflict) {
				t.Fatal("unknown reopened", err)
			}
		})
	}
}

func TestCommandRetryClaimAtomicityConcurrencyAndBackoff(t *testing.T) {
	ctx := context.Background()
	s, req, now := commandStoreRequest(t)
	req.Kind = "help"
	if _, _, err := s.ApplyLocalCommand(ctx, req, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_reply_claim BEFORE INSERT ON command_reply_attempts BEGIN SELECT RAISE(FAIL,'fixture'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimCommandReply(ctx, "host", now.UnixMilli()); err == nil || claimed {
		t.Fatal("failed claim was dispatched")
	}
	r, err := s.CommandReply(ctx, "host", "app", req.MessageID)
	if err != nil || r.State != "ready" {
		t.Fatal("failed journal changed ready state", err)
	}
	if _, err := s.DB.Exec("DROP TRIGGER fail_reply_claim"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER ignore_reply_claim BEFORE UPDATE ON command_replies BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimCommandReply(ctx, "host", now.UnixMilli()); !errors.Is(err, ErrConflict) || claimed {
		t.Fatal("ignored claim was dispatched", err)
	}
	var noAttempts int
	if err := s.DB.QueryRow("SELECT count(*) FROM command_reply_attempts").Scan(&noAttempts); err != nil || noAttempts != 0 {
		t.Fatal("ignored claim published an attempt", err)
	}
	if _, err := s.DB.Exec("DROP TRIGGER ignore_reply_claim"); err != nil {
		t.Fatal(err)
	}
	var claims atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, claimed, err := s.ClaimCommandReply(ctx, "host", now.UnixMilli())
			if err != nil {
				t.Error(err)
			}
			if claimed {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatal("concurrent reply duplicate", claims.Load())
	}
	at := now.UnixMilli()
	for n, delay := range []int64{1000, 2000, 4000, 8000, 16000, 32000, 60000, 60000, 90000} {
		a, err := s.CommandReply(ctx, "host", "app", req.MessageID)
		if err != nil {
			t.Fatal(err)
		}
		floor := int64(0)
		if n == 8 {
			floor = 90000
		}
		if err := s.DeferCommandReplyAttempt(ctx, a.ID, at, floor); err != nil {
			t.Fatal(err)
		}
		var due, count int64
		if err := s.DB.QueryRow("SELECT next_retry_at_ms,rejection_count FROM command_reply_throttles").Scan(&due, &count); err != nil || due != at+delay || count != int64(n+1) {
			t.Fatal("wrong retry delay", n, due, count, err)
		}
		at = due
		retryCommandClaim(t, s, at)
	}
	a, err := s.CommandReply(ctx, "host", "app", req.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeferCommandReplyAttempt(ctx, a.ID, math.MaxInt64, 1000); !errors.Is(err, ErrConflict) {
		t.Fatal("overflow accepted", err)
	}
}

func TestCommandRetryMigrationPreservesUnknownWithoutInventingAttempt(t *testing.T) {
	ctx := context.Background()
	s, req, now := commandStoreRequest(t)
	req.Kind = "help"
	original, _, err := s.ApplyLocalCommand(ctx, req, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE command_replies SET state='dispatching'; DROP TABLE command_reply_throttles; DROP TABLE command_reply_attempts; PRAGMA user_version=13"); err != nil {
		t.Fatal(err)
	}
	var sequence int
	var name, path string
	if err := s.DB.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverCommandDispatches(ctx, "host"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM command_reply_attempts").Scan(&n); err != nil || n != 0 {
		t.Fatal("legacy attempt guessed", err)
	}
	if _, claimed, err := s.ClaimCommandReply(ctx, "host", now.Add(time.Hour).UnixMilli()); err != nil || claimed {
		t.Fatal("legacy unknown resent", err)
	}
	if err := s.FinishCommandReply(ctx, original.ID, "confirmed", "readback-result"); err != nil {
		t.Fatal(err)
	}
}

func TestCommandRetryNoOpProofWritesNeverPublishSuccess(t *testing.T) {
	for _, phase := range []string{"apply", "claim", "defer", "finish"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			s, req, now := commandStoreRequest(t)
			if phase == "apply" {
				if _, err := s.DB.Exec(`CREATE TRIGGER ignore_reply BEFORE INSERT ON command_replies BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.ApplyLocalCommand(ctx, req, now); !errors.Is(err, ErrConflict) {
					t.Fatal("no dedup record accepted command effect", err)
				}
				remaining, err := s.DirectoryRules(ctx, "host", "app")
				if err != nil || len(remaining) != 0 {
					t.Fatal("unrecorded command effect persisted", err)
				}
				return
			}
			req.Kind = "help"
			if _, _, err := s.ApplyLocalCommand(ctx, req, now); err != nil {
				t.Fatal(err)
			}
			if phase == "claim" {
				if _, err := s.DB.Exec(`CREATE TRIGGER ignore_attempt BEFORE INSERT ON command_reply_attempts BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
					t.Fatal(err)
				}
				if _, claimed, err := s.ClaimCommandReply(ctx, "host", now.UnixMilli()); !errors.Is(err, ErrConflict) || claimed {
					t.Fatal("claim without snapshot accepted", err)
				}
				current, err := s.CommandReply(ctx, "host", "app", req.MessageID)
				if err != nil || current.State != "ready" {
					t.Fatal("claim did not roll back", err)
				}
				return
			}
			a := retryCommandClaim(t, s, now.UnixMilli())
			var err error
			if phase == "defer" {
				if _, err := s.DB.Exec(`CREATE TRIGGER ignore_deadline BEFORE INSERT ON command_reply_throttles BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
					t.Fatal(err)
				}
				err = s.DeferCommandReplyAttempt(ctx, a.ID, now.UnixMilli(), 1000)
			} else {
				if _, err := s.DB.Exec(`CREATE TRIGGER ignore_confirmation BEFORE UPDATE ON command_reply_attempts BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
					t.Fatal(err)
				}
				err = s.FinishCommandReply(ctx, a.ID, "confirmed", "remote")
			}
			if !errors.Is(err, ErrConflict) {
				t.Fatal("missing proof accepted", err)
			}
			current, err := s.CommandReply(ctx, "host", "app", req.MessageID)
			if err != nil || current.ID != a.ID || current.State != "dispatching" {
				t.Fatal("outcome did not roll back", err)
			}
		})
	}
}
