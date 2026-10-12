package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func verificationGroupFixture(t *testing.T) (*Store, VerificationGroup) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "verification-groups.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := VerificationGroup{
		RequestID:    ID(),
		HostID:       "host",
		AppID:        "app",
		OwnerID:      "owner",
		BridgeCommit: "build",
		BinarySHA256: strings.Repeat("b", 64),
		UUID:         ID(),
		Name:         "Codex verification",
		MembersJSON:  `["owner"]`,
		CreatedAtMS:  100,
		ExpiresAtMS:  200,
	}
	return s, c
}

func TestVerificationGroupMigrationFromEighteenPreservesM0AndCard(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "verification-migration.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	card := VerificationCard{
		RequestID:    ID(),
		HostID:       "host",
		AppID:        "app",
		OwnerID:      "owner",
		BridgeCommit: "build",
		BinarySHA256: strings.Repeat("a", 64),
		Token:        ID(),
		UUID:         ID(),
		Card:         "frozen card",
		CreatedAtMS:  100,
		ExpiresAtMS:  200,
	}
	if _, err := s.PrepareVerificationCard(ctx, card); err != nil {
		t.Fatal(err)
	}
	if fresh, err := s.Begin(ctx, "m0-retained", "send"); err != nil || !fresh {
		t.Fatal("m0 journal", fresh, err)
	}
	if err := s.Finish(ctx, "m0-retained", "confirmed", "om_m0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DROP TABLE verification_groups; PRAGMA user_version=18`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, remote, err := s.Operation(ctx, "m0-retained")
	if err != nil || state != "confirmed" || remote != "om_m0" {
		t.Fatal("migration lost M0 evidence", state, remote, err)
	}
	gotCard, err := s.VerificationCard(ctx, card.RequestID, card.HostID, card.AppID, card.OwnerID)
	if err != nil || gotCard.Card != card.Card || gotCard.Token != card.Token {
		t.Fatal("migration lost card evidence", gotCard, err)
	}
	var version int
	if err := s.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal("unexpected schema version", version, err)
	}
	var groups int
	if err := s.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='verification_groups'`).Scan(&groups); err != nil || groups != 1 {
		t.Fatal("verification group table missing", groups, err)
	}
}

func TestVerificationGroupPrepareIsImmutableAndScoped(t *testing.T) {
	ctx := context.Background()
	s, c := verificationGroupFixture(t)
	got, err := s.PrepareVerificationGroup(ctx, c)
	if err != nil || got.State != "ready" || got.ChatID != "" || got.GroupMessageID != "" || got.MembershipVerifiedAtMS != 0 {
		t.Fatal("initial group journal", got, err)
	}

	changed := c
	changed.UUID = ID()
	changed.Name = "replacement"
	changed.MembersJSON = `["other"]`
	changed.ExpiresAtMS = 300
	got, err = s.PrepareVerificationGroup(ctx, changed)
	if err != nil || got.UUID != c.UUID || got.Name != c.Name || got.MembersJSON != c.MembersJSON || got.ExpiresAtMS != c.ExpiresAtMS {
		t.Fatal("group journal was replaced", got, err)
	}

	duplicateUUID := c
	duplicateUUID.RequestID = ID()
	if _, err := s.PrepareVerificationGroup(ctx, duplicateUUID); err == nil {
		t.Fatal("duplicate group UUID accepted")
	}

	foreign := changed
	foreign.HostID = "other-host"
	if _, err := s.PrepareVerificationGroup(ctx, foreign); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("scope collision leaked group", err)
	}
	if _, err := s.VerificationGroup(ctx, c.RequestID, "other-host", c.AppID, c.OwnerID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("unscoped group read", err)
	}
}

func TestVerificationGroupClaimIsSingleAndFinishCannotOverwrite(t *testing.T) {
	ctx := context.Background()
	s, c := verificationGroupFixture(t)
	if _, err := s.PrepareVerificationGroup(ctx, c); err != nil {
		t.Fatal(err)
	}
	const attempts = 10
	results := make(chan bool, attempts)
	errCh := make(chan error, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.ClaimVerificationGroup(ctx, c, 150)
			results <- ok
			errCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errCh)
	claimed := 0
	for ok := range results {
		if ok {
			claimed++
		}
	}
	for err := range errCh {
		if err != nil {
			t.Fatal("concurrent claim", err)
		}
	}
	if claimed != 1 {
		t.Fatal("claim count", claimed)
	}
	if err := s.FinishVerificationGroup(ctx, c, "confirmed", "chat-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishVerificationGroup(ctx, c, "unknown", ""); err == nil {
		t.Fatal("finished group was overwritten")
	}
	got, err := s.VerificationGroup(ctx, c.RequestID, c.HostID, c.AppID, c.OwnerID)
	if err != nil || got.State != "confirmed" || got.ChatID != "chat-1" {
		t.Fatal("finish state changed", got, err)
	}
}

func TestVerificationGroupMessageIsScopedExpiringAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s, c := verificationGroupFixture(t)
	if _, err := s.PrepareVerificationGroup(ctx, c); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimVerificationGroup(ctx, c, 150); err != nil || !ok {
		t.Fatal("claim", ok, err)
	}
	if err := s.FinishVerificationGroup(ctx, c, "confirmed", "chat-1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ObserveVerificationGroupMessage(ctx, "other-host", c.AppID, c.OwnerID, c.RequestID, "om-wrong-scope", "chat-1", 150); err != nil || ok {
		t.Fatal("wrong scope message recorded", ok, err)
	}
	if ok, err := s.ObserveVerificationGroupMessage(ctx, c.HostID, c.AppID, c.OwnerID, c.RequestID, "om-wrong-chat", "chat-2", 150); err != nil || ok {
		t.Fatal("wrong chat message recorded", ok, err)
	}
	if ok, err := s.ObserveVerificationGroupMessage(ctx, c.HostID, c.AppID, c.OwnerID, c.RequestID, "om-expired", "chat-1", 200); err != nil || ok {
		t.Fatal("expired message recorded", ok, err)
	}
	if ok, err := s.ObserveVerificationGroupMessage(ctx, c.HostID, c.AppID, c.OwnerID, c.RequestID, "om-owner", "chat-1", 150); err != nil || !ok {
		t.Fatal("valid message not recorded", ok, err)
	}
	if ok, err := s.ObserveVerificationGroupMessage(ctx, c.HostID, c.AppID, c.OwnerID, c.RequestID, "om-owner", "chat-1", 151); err != nil || !ok {
		t.Fatal("duplicate message not idempotent", ok, err)
	}
	if ok, err := s.ObserveVerificationGroupMessage(ctx, c.HostID, c.AppID, c.OwnerID, c.RequestID, "om-other", "chat-1", 151); err != nil || ok {
		t.Fatal("second message replaced first", ok, err)
	}
	got, err := s.VerificationGroup(ctx, c.RequestID, c.HostID, c.AppID, c.OwnerID)
	if err != nil || got.GroupMessageID != "om-owner" {
		t.Fatal("message identity changed", got, err)
	}
}

func TestVerificationGroupMembershipRequiresConfirmedExactScopeAndChat(t *testing.T) {
	ctx := context.Background()
	s, c := verificationGroupFixture(t)
	if _, err := s.PrepareVerificationGroup(ctx, c); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ObserveVerificationGroupMembership(ctx, c, 150); err != nil || ok {
		t.Fatal("unconfirmed membership accepted", ok, err)
	}
	if ok, err := s.ClaimVerificationGroup(ctx, c, 150); err != nil || !ok {
		t.Fatal("claim", ok, err)
	}
	if err := s.FinishVerificationGroup(ctx, c, "confirmed", "chat-1"); err != nil {
		t.Fatal(err)
	}
	wrongChat := c
	wrongChat.ChatID = "chat-2"
	if ok, err := s.ObserveVerificationGroupMembership(ctx, wrongChat, 150); err != nil || ok {
		t.Fatal("wrong chat membership accepted", ok, err)
	}
	wrongHash := c
	wrongHash.ChatID = "chat-1"
	wrongHash.BinarySHA256 = strings.Repeat("c", 64)
	if ok, err := s.ObserveVerificationGroupMembership(ctx, wrongHash, 150); err != nil || ok {
		t.Fatal("wrong binary membership accepted", ok, err)
	}
	wrongScope := c
	wrongScope.ChatID = "chat-1"
	wrongScope.OwnerID = "other-owner"
	if ok, err := s.ObserveVerificationGroupMembership(ctx, wrongScope, 150); err != nil || ok {
		t.Fatal("wrong scope membership accepted", ok, err)
	}
	if ok, err := s.ObserveVerificationGroupMembership(ctx, c, 200); err != nil || ok {
		t.Fatal("expired membership accepted", ok, err)
	}
	c.ChatID = "chat-1"
	if ok, err := s.ObserveVerificationGroupMembership(ctx, c, 150); err != nil || !ok {
		t.Fatal("valid membership not recorded", ok, err)
	}
	if ok, err := s.ObserveVerificationGroupMembership(ctx, c, 151); err != nil || !ok {
		t.Fatal("duplicate membership not idempotent", ok, err)
	}
	got, err := s.VerificationGroup(ctx, c.RequestID, c.HostID, c.AppID, c.OwnerID)
	if err != nil || got.MembershipVerifiedAtMS != 150 {
		t.Fatal("membership timestamp changed", got, err)
	}
}
