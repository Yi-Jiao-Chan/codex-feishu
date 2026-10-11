package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/feishu"
	"codex-feishu/internal/rules"
	"codex-feishu/internal/store"
)

type retryRequestGateway struct {
	requestGateway
	memberCalls int
	onMembers   func()
}

func (g *retryRequestGateway) MemberIDs(context.Context, string) ([]string, error) {
	g.memberCalls++
	if g.onMembers != nil {
		g.onMembers()
	}
	return []string{"ou_user"}, nil
}

func liveRetryTracker(t *testing.T) (*RequestTracker, *time.Time, *retryRequestGateway) {
	t.Helper()
	ctx := context.Background()
	tr, now, _ := durableTrackerFixture(t)
	if err := tr.Hydrated(ctx, "thread", nil); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(3 * time.Second)
	at := now.Unix()
	tr.started = map[string]map[string]int64{"thread": {"turn": at}}
	if err := tr.ObserveContext(ctx, routedEvent("item/tool/requestUserInput", "19", 1)); err != nil {
		t.Fatal(err)
	}
	stageTrackerProgress(t, tr, *now)
	if err := tr.Hydrated(ctx, "thread", []codexclient.Turn{{ID: "turn", Status: "inProgress", StartedAt: &at}}); err != nil {
		t.Fatal(err)
	}
	bindTracker(t, tr)
	g := &retryRequestGateway{}
	tr.Gateway, tr.Membership = g, g
	return tr, now, g
}

func TestRequestReminderRetryUsesResponseClockAndSkipsMembershipDuringWait(t *testing.T) {
	ctx := context.Background()
	tr, now, g := liveRetryTracker(t)
	initial := *now
	g.sendErr = &feishu.RateLimited{RetryAfter: 7 * time.Second}
	g.afterRPC = func() { *now = now.Add(3 * time.Second) }
	if sent, err := tr.DispatchOne(ctx, "thread"); !sent || err != nil {
		t.Fatal(sent, err)
	}
	oldID, oldCard := g.uuid, g.card
	var due int64
	if err := tr.Store.DB.QueryRow(`SELECT next_retry_at_ms FROM request_reminder_throttles`).Scan(&due); err != nil || due != initial.Add(10*time.Second).UnixMilli() {
		t.Fatal(due, err)
	}
	*now = time.UnixMilli(due - 1)
	for n := 0; n < 4; n++ {
		if sent, err := tr.DispatchOne(ctx, "thread"); sent || err != nil {
			t.Fatal(sent, err)
		}
	}
	if g.memberCalls != 1 || g.calls != 1 {
		t.Fatal("network work before deadline", g.memberCalls, g.calls)
	}
	*now = time.UnixMilli(due)
	g.sendErr, g.afterRPC = nil, nil
	if sent, err := tr.DispatchOne(ctx, "thread"); !sent || err != nil {
		t.Fatal(sent, err)
	}
	if g.memberCalls != 2 || g.calls != 2 || g.uuid == oldID || !sameVisibleTestCard(t, tr.Store, g.uuid, g.card, oldID, oldCard) || g.parent != "original-input" {
		t.Fatal("invalid retry evidence", g)
	}
	if sent, err := tr.DispatchOne(ctx, "thread"); sent || err != nil || g.memberCalls != 2 {
		t.Fatal("confirmed reminder repeated", sent, err)
	}
}

func TestRequestReminderRateWaitPrunesSuppressionWithoutNetworkReads(t *testing.T) {
	for _, variant := range []string{"off_on", "whole_quiet", "resolved", "configuration_removed"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			tr, now, g := liveRetryTracker(t)
			g.sendErr = &feishu.RateLimited{RetryAfter: 3 * time.Minute}
			if sent, err := tr.DispatchOne(ctx, "thread"); !sent || err != nil {
				t.Fatal(sent, err)
			}
			*now = now.Add(time.Second)
			p, _ := rules.DefaultPolicy("UTC")
			switch variant {
			case "off_on":
				p.Enabled, p.Revision = false, 1
				if err := tr.Store.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, *now); err != nil {
					t.Fatal(err)
				}
				*now = now.Add(time.Second)
				p.Enabled, p.Revision = true, 2
				if err := tr.Store.SetNotificationPolicyAt(ctx, "app", "ou_user", 1, p, *now); err != nil {
					t.Fatal(err)
				}
			case "whole_quiet":
				p.QuietStart, p.QuietEnd, p.Revision = "10:01", "10:02", 1
				if err := tr.Store.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, *now); err != nil {
					t.Fatal(err)
				}
				*now = now.Add(2 * time.Minute)
			case "resolved":
				if err := tr.ObserveContext(ctx, routedEvent("serverRequest/resolved", "19", 1)); err != nil {
					t.Fatal(err)
				}
			case "configuration_removed":
				tr.Options.Targets = nil
			}
			// Still inside server wait, yet local suppression is durable now.
			if sent, err := tr.DispatchOne(ctx, "thread"); sent || err != nil {
				t.Fatal(sent, err)
			}
			if d := trackerDecision(t, tr, "19"); d.State != "suppressed" {
				t.Fatal(d)
			}
			*now = now.Add(24 * time.Hour)
			g.sendErr = nil
			if sent, err := tr.DispatchOne(ctx, "thread"); sent || err != nil || g.calls != 1 || g.memberCalls != 1 {
				t.Fatal("old reminder revived", sent, err, g.calls, g.memberCalls)
			}
		})
	}
}

func TestRequestReminderClaimsAfterMembershipIOAndPersistsProofBeforeSending(t *testing.T) {
	for _, variant := range []string{"resolution_during_membership", "quiet_during_membership", "ignored_attempt", "ignored_recipient", "failed_defer_commit"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			tr, now, g := liveRetryTracker(t)
			switch variant {
			case "resolution_during_membership":
				g.onMembers = func() {
					if err := tr.ObserveContext(ctx, routedEvent("serverRequest/resolved", "19", 1)); err != nil {
						t.Fatal(err)
					}
				}
			case "quiet_during_membership":
				p := rules.Policy{Enabled: true, Timezone: "UTC", QuietStart: "10:01", QuietEnd: "10:02", Revision: 1}
				if err := tr.Store.SetNotificationPolicyAt(ctx, "app", "ou_user", 0, p, *now); err != nil {
					t.Fatal(err)
				}
				g.onMembers = func() { *now = now.Add(time.Minute) }
			case "ignored_attempt":
				if _, err := tr.Store.DB.Exec(`CREATE TRIGGER ignore_attempt BEFORE INSERT ON request_reminder_attempts BEGIN SELECT RAISE(IGNORE); END`); err != nil {
					t.Fatal(err)
				}
			case "ignored_recipient":
				if _, err := tr.Store.DB.Exec(`CREATE TRIGGER ignore_recipient BEFORE UPDATE ON request_mentions WHEN NEW.state='dispatching' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
					t.Fatal(err)
				}
			case "failed_defer_commit":
				g.sendErr = &feishu.RateLimited{RetryAfter: time.Second}
				g.afterRPC = func() {
					if _, err := tr.Store.DB.Exec(`CREATE TRIGGER ignore_defer BEFORE UPDATE ON app_requests BEGIN SELECT RAISE(IGNORE); END`); err != nil {
						t.Fatal(err)
					}
				}
			}
			sent, err := tr.DispatchOne(ctx, "thread")
			if variant == "failed_defer_commit" {
				if !sent || err == nil || g.calls != 1 {
					t.Fatal(sent, err, g.calls)
				}
				if _, err := tr.Store.DB.Exec(`DROP TRIGGER ignore_defer`); err != nil {
					t.Fatal(err)
				}
				if err := tr.Store.RecoverRequestReminders(ctx, "host"); err != nil {
					t.Fatal(err)
				}
				*now = now.Add(time.Minute)
				if sent, err := tr.DispatchOne(ctx, "thread"); sent || err != nil || g.calls != 1 || g.memberCalls != 1 {
					t.Fatal("failed journal retried", sent, err)
				}
				return
			}
			proofFailure := variant == "ignored_attempt" || variant == "ignored_recipient"
			if sent || (err != nil) != proofFailure || g.calls != 0 {
				t.Fatal("unproved/suppressed RPC", sent, err, g.calls)
			}
			if !proofFailure && trackerDecision(t, tr, "19").State != "suppressed" {
				t.Fatal("suppression lost")
			}
			if proofFailure && !errors.Is(err, store.ErrConflict) {
				t.Fatal("expected store conflict", err)
			}
		})
	}
}

func sameVisibleTestCard(t *testing.T, s *store.Store, aID, aCard, bID, bCard string) bool {
	t.Helper()
	strip := func(id, raw string) string {
		marker, err := s.CardRequestMarker(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		// The helper adds exactly one free top-level markdown ID; remove only it.
		var doc map[string]any
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatal(err)
		}
		for _, v := range doc["body"].(map[string]any)["elements"].([]any) {
			el := v.(map[string]any)
			if el["element_id"] == marker {
				delete(el, "element_id")
			}
		}
		encoded, _ := json.Marshal(doc)
		return string(encoded)
	}
	return strip(aID, aCard) == strip(bID, bCard)
}
