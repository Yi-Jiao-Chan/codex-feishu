package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerificationMigrationFromSeventeenAndImmutableClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verification.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`INSERT INTO m0_operations(key,kind,state,remote_id,created_at) VALUES('retained','send','confirmed','om_old',1);DROP TABLE verification_cards;ALTER TABLE mirrors DROP COLUMN progress_observed_at_json; ALTER TABLE create_intents DROP COLUMN description; PRAGMA user_version=17;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	state, id, err := s.Operation(ctx, "retained")
	if err != nil || state != "confirmed" || id != "om_old" {
		t.Fatal("migration lost journal", state, id, err)
	}
	c := VerificationCard{RequestID: ID(), HostID: "host", AppID: "app", OwnerID: "owner", BridgeCommit: "build", BinarySHA256: strings.Repeat("b", 64), Token: ID(), UUID: ID(), Card: "original", CreatedAtMS: 100, ExpiresAtMS: 200}
	got, err := s.PrepareVerificationCard(ctx, c)
	if err != nil || got.Card != "original" {
		t.Fatal(got, err)
	}
	changed := c
	changed.Card = "replacement"
	changed.Token = ID()
	changed.UUID = ID()
	changed.ExpiresAtMS = 300
	got, err = s.PrepareVerificationCard(ctx, changed)
	if err != nil || got.Card != c.Card || got.Token != c.Token || got.UUID != c.UUID || got.ExpiresAtMS != 200 {
		t.Fatal("immutable probe replaced", got, err)
	}
	changed.OwnerID = "other"
	if _, err = s.PrepareVerificationCard(ctx, changed); err == nil {
		t.Fatal("request collision leaked other owner")
	}
	if ok, err := s.ClaimVerificationCard(ctx, c, 199); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.ClaimVerificationCard(ctx, c, 199); err != nil || ok {
		t.Fatal("duplicate dispatch claimed", ok, err)
	}
	if err := s.FinishVerificationCard(ctx, c, "confirmed", "om_card"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishVerificationCard(ctx, c, "unknown", ""); err == nil {
		t.Fatal("confirmed proof overwritten")
	}
	if ok, err := s.ObserveVerificationCallback(ctx, c.HostID, c.AppID, c.OwnerID, c.Token, "om_wrong", "chat", 199); err != nil || ok {
		t.Fatal("mismatched message callback recorded")
	}
	if ok, err := s.ObserveVerificationCallback(ctx, c.HostID, c.AppID, c.OwnerID, c.Token, "om_card", "chat", 200); err != nil || ok {
		t.Fatal("expired callback recorded")
	}
	var schema int
	if err := s.DB.QueryRow("PRAGMA user_version").Scan(&schema); err != nil || schema != SchemaVersion {
		t.Fatal(schema, err)
	}
}
