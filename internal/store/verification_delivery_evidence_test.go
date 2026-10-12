package store

import (
	"context"
	"testing"
)

type deliveryEvidenceFixture struct {
	s  *Store
	b  VerificationBinding
	r  Receipt
	at int64
}

func newDeliveryEvidenceFixture(t *testing.T) *deliveryEvidenceFixture {
	t.Helper()
	s, b, _ := inputEvidenceFixture(t)
	r, _, err := s.Claim(context.Background(), Receipt{AppID: b.AppID, ThreadID: b.ThreadID, ChatID: b.ChatID, MessageID: "original-input", Body: "fixture input"})
	if err != nil {
		t.Fatal(err)
	}
	return &deliveryEvidenceFixture{s: s, b: b, r: r, at: 10000}
}

func (f *deliveryEvidenceFixture) doc(key, kind string) Mirror {
	return Mirror{Key: key, TurnID: "native-turn", Kind: kind, Card: `{"fixture":1}`, Terminal: kind == "final", ParentMessageID: f.r.MessageID}
}

func (f *deliveryEvidenceFixture) stage(t *testing.T, links []InputLink, docs ...Mirror) {
	t.Helper()
	f.at += 3000
	if err := f.s.StageSnapshot(context.Background(), f.b.HostID, f.b.ThreadID, docs, links, Watermark{ObservedAtMS: f.at}); err != nil {
		t.Fatal(err)
	}
}

func (f *deliveryEvidenceFixture) claim(t *testing.T, key string) MirrorAttempt {
	t.Helper()
	f.at += 3000
	m, err := f.s.Mirror(context.Background(), f.b.HostID, f.b.ThreadID, key)
	if err != nil {
		t.Fatal(err)
	}
	a, claimed, err := f.s.ClaimMirror(context.Background(), f.b.HostID, f.b.ThreadID, key, m.Revision, f.at)
	if err != nil || !claimed {
		t.Fatal("claim mirror fixture", key, claimed, err)
	}
	return a
}

func (f *deliveryEvidenceFixture) finish(t *testing.T, key, remote string) MirrorAttempt {
	t.Helper()
	a := f.claim(t, key)
	if err := f.s.FinishMirrorAttempt(context.Background(), a.ID, "confirmed", remote); err != nil {
		t.Fatal(err)
	}
	return a
}

func assertDeliveryEvidence(t *testing.T, s *Store, b VerificationBinding, want VerificationDeliveryEvidence) {
	t.Helper()
	var before, after int
	if err := s.DB.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	got, err := s.VerificationDeliveryEvidence(context.Background(), b)
	if err != nil || got != want {
		t.Fatalf("delivery evidence: got %+v want %+v err %v", got, want, err)
	}
	if err := s.DB.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil || before != after {
		t.Fatal("evidence query wrote", before, after, err)
	}
}

func TestVerificationDeliveryEvidenceTracksCurrentRevisionReferencesAndRetirement(t *testing.T) {
	f := newDeliveryEvidenceFixture(t)
	ctx := context.Background()
	p, a, b, never := f.doc("progress", "progress"), f.doc("final:0", "final"), f.doc("final:1", "final"), f.doc("final:2", "final")
	f.stage(t, []InputLink{{f.r.ID, "native-turn", "native-user-item"}}, p, a, b, never)
	want := VerificationDeliveryEvidence{ProgressCards: 1, FinalCards: 3, FinalTurns: 1}
	assertDeliveryEvidence(t, f.s, f.b, want)
	progress := f.claim(t, p.Key)
	want.UnresolvedAttempts = 1
	assertDeliveryEvidence(t, f.s, f.b, want)
	if err := f.s.FinishMirrorAttempt(ctx, progress.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	assertDeliveryEvidence(t, f.s, f.b, want)
	if err := f.s.FinishMirrorAttempt(ctx, progress.ID, "confirmed", "progress-message"); err != nil {
		t.Fatal(err)
	}
	want.ProgressConfirmed, want.ReferencedCards, want.UnresolvedAttempts = 1, 1, 0
	assertDeliveryEvidence(t, f.s, f.b, want)
	f.finish(t, a.Key, "final-message-0")
	f.finish(t, b.Key, "final-message-1")
	want.FinalConfirmed, want.ReferencedCards = 2, 3
	assertDeliveryEvidence(t, f.s, f.b, want)
	never.Retired, never.Card = true, `{"retired":true}`
	f.stage(t, nil, never)
	want.FinalCards, want.FinalTurnsDelivered = 2, 1
	assertDeliveryEvidence(t, f.s, f.b, want)
	p.Card = `{"fixture":2}`
	f.stage(t, nil, p)
	want.ProgressConfirmed = 0
	assertDeliveryEvidence(t, f.s, f.b, want)
	update := f.finish(t, p.Key, "progress-message")
	if update.Operation != "update" || update.TargetMessageID != "progress-message" {
		t.Fatal("fixture did not update original card")
	}
	want.ProgressConfirmed = 1
	b.Retired, b.Card = true, `{"retired":true}`
	f.stage(t, nil, b)
	want.FinalCards, want.FinalConfirmed, want.FinalTurnsDelivered, want.ReferencedCards = 1, 1, 0, 2
	assertDeliveryEvidence(t, f.s, f.b, want)
	f.finish(t, b.Key, "final-message-1")
	want.FinalTurnsDelivered = 1
	ui, uiTurn := f.doc("ui:queue:fixture", "progress"), f.doc("progress-with-ui-turn", "progress")
	uiTurn.TurnID = "ui:queue:fixture"
	f.stage(t, nil, ui, uiTurn)
	queued := f.claim(t, ui.Key)
	if err := f.s.FinishMirrorAttempt(ctx, queued.ID, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	want.UnresolvedAttempts = 1
	assertDeliveryEvidence(t, f.s, f.b, want)
}

func TestVerificationDeliveryEvidenceRequiresReliableAnchorBirthAndCurrentAttempt(t *testing.T) {
	for _, damage := range []string{"valid-receipt", "receipt-chat", "receipt-app", "receipt-empty-client", "link-turn", "missing-link", "birth-chat", "birth-parent", "birth-send", "missing-birth", "current-revision", "update-target", "valid-human", "human-turn", "human-birth-chat", "human-missing-birth"} {
		t.Run(damage, func(t *testing.T) {
			f := newDeliveryEvidenceFixture(t)
			p := f.doc("progress", "progress")
			link := []InputLink{{f.r.ID, p.TurnID, "user-item"}}
			if damage == "missing-link" {
				link = nil
			}
			human := Mirror{Key: "human", TurnID: p.TurnID, Kind: "human", Card: `{"fixture":1}`}
			var humanBirth MirrorAttempt
			if damage == "valid-human" || damage == "human-turn" || damage == "human-birth-chat" || damage == "human-missing-birth" {
				p.ParentKey, p.ParentMessageID = human.Key, ""
				f.stage(t, nil, human, p)
				humanBirth = f.finish(t, human.Key, "human-message")
			} else {
				f.stage(t, link, p)
			}
			birth := f.finish(t, p.Key, "progress-message")
			var update MirrorAttempt
			if damage == "missing-birth" || damage == "update-target" {
				p.Card = `{"fixture":2}`
				f.stage(t, nil, p)
				update = f.finish(t, p.Key, "progress-message")
			}
			query, args := "", []any{}
			switch damage {
			case "receipt-chat":
				query = `UPDATE m0_receipts SET chat_id='other-chat'`
			case "receipt-app":
				query = `UPDATE m0_receipts SET app_id='other-app'`
			case "receipt-empty-client":
				query = `UPDATE m0_receipts SET client_id=''`
			case "link-turn":
				query = `UPDATE input_history_links SET turn_id='other-turn'`
			case "birth-chat":
				query = `UPDATE outbound_attempts SET chat_id='other-chat' WHERE attempt_id=?`
				args = []any{birth.ID}
			case "birth-parent":
				query = `UPDATE outbound_attempts SET parent_message_id='other-parent' WHERE attempt_id=?`
				args = []any{birth.ID}
			case "birth-send":
				query = `UPDATE outbound_attempts SET operation='send' WHERE attempt_id=?`
				args = []any{birth.ID}
			case "missing-birth":
				query = `UPDATE outbound_attempts SET state='rejected' WHERE attempt_id=?`
				args = []any{birth.ID}
			case "current-revision":
				query = `UPDATE mirrors SET revision=revision+1,delivered_revision=revision+1 WHERE mirror_key='progress'`
			case "update-target":
				query = `UPDATE outbound_attempts SET target_message_id='other-message' WHERE attempt_id=?`
				args = []any{update.ID}
			case "human-turn":
				query = `UPDATE mirrors SET turn_id='other-turn' WHERE mirror_key='human'`
			case "human-birth-chat":
				query = `UPDATE outbound_attempts SET chat_id='other-chat' WHERE attempt_id=?`
				args = []any{humanBirth.ID}
			case "human-missing-birth":
				query = `UPDATE outbound_attempts SET state='rejected' WHERE attempt_id=?`
				args = []any{humanBirth.ID}
			}
			if query != "" {
				if _, err := f.s.DB.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			want := VerificationDeliveryEvidence{ProgressCards: 1}
			if damage == "valid-receipt" || damage == "valid-human" {
				want.ProgressConfirmed, want.ReferencedCards = 1, 1
			}
			if damage == "current-revision" || damage == "update-target" {
				want.ReferencedCards = 1
			}
			assertDeliveryEvidence(t, f.s, f.b, want)
		})
	}
}

func TestVerificationDeliveryEvidenceCannotSkipPreviouslySentOrUnresolvedRetiredTail(t *testing.T) {
	for _, damage := range []string{"sent-other-chat", "unknown-other-chat", "missing-outbox"} {
		t.Run(damage, func(t *testing.T) {
			f := newDeliveryEvidenceFixture(t)
			main, tail := f.doc("final:0", "final"), f.doc("final:1", "final")
			f.stage(t, []InputLink{{f.r.ID, main.TurnID, "user-item"}}, main, tail)
			f.finish(t, main.Key, "main-message")
			var birth MirrorAttempt
			if damage == "sent-other-chat" {
				birth = f.finish(t, tail.Key, "tail-message")
			} else if damage == "unknown-other-chat" {
				birth = f.claim(t, tail.Key)
				if err := f.s.FinishMirrorAttempt(context.Background(), birth.ID, "unknown", ""); err != nil {
					t.Fatal(err)
				}
			}
			tail.Retired, tail.Card = true, `{"retired":true}`
			f.stage(t, nil, tail)
			if damage == "missing-outbox" {
				if _, err := f.s.DB.Exec(`DELETE FROM outbound WHERE mirror_key=?`, tail.Key); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.s.DB.Exec(`UPDATE outbound_attempts SET chat_id='other-chat' WHERE attempt_id=?`, birth.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.DB.Exec(`UPDATE mirrors SET remote_id='' WHERE mirror_key=?`, tail.Key); err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.DB.Exec(`UPDATE outbound SET state='confirmed' WHERE mirror_key=?`, tail.Key); err != nil {
					t.Fatal(err)
				}
			}
			assertDeliveryEvidence(t, f.s, f.b, VerificationDeliveryEvidence{FinalCards: 1, FinalConfirmed: 1, FinalTurns: 1, ReferencedCards: 1})
		})
	}
}

func TestVerificationDeliveryEvidenceReadOnlyGateAndFailures(t *testing.T) {
	f := newDeliveryEvidenceFixture(t)
	p := f.doc("progress", "progress")
	f.stage(t, []InputLink{{f.r.ID, p.TurnID, "user-item"}}, p)
	f.finish(t, p.Key, "progress-message")
	var sequence int
	var name, path string
	if err := f.s.DB.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	reader, _, err := OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	assertDeliveryEvidence(t, reader, f.b, VerificationDeliveryEvidence{ProgressCards: 1, ProgressConfirmed: 1, ReferencedCards: 1})
	for _, damage := range []string{"binding-caller", "stored-binding", "route-alias", "intent-owner", "read-failure", "canceled"} {
		t.Run(damage, func(t *testing.T) {
			f := newDeliveryEvidenceFixture(t)
			queries := map[string]string{
				"stored-binding": `UPDATE verification_bindings SET cwd='other'`,
				"route-alias":    `UPDATE bindings SET chat_id='other-chat'`,
				"intent-owner":   `UPDATE create_intents SET owner_id='other-owner'`,
				"read-failure":   `ALTER TABLE outbound_attempts RENAME TO hidden_outbound_attempts`,
			}
			if query := queries[damage]; query != "" {
				if _, err := f.s.DB.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			if damage == "binding-caller" {
				f.b.BotID = "other-bot"
			}
			if damage == "canceled" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			if _, err := f.s.VerificationDeliveryEvidence(ctx, f.b); err == nil {
				t.Fatal("invalid read reported successful empty evidence")
			}
		})
	}
}
