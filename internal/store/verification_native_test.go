package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func verificationNativeFixture(t *testing.T) (*Store, VerificationNative) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "verification-native.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := VerificationNative{
		RequestID:    ID(),
		HostID:       "host",
		AppID:        "app",
		OwnerID:      "owner",
		BridgeCommit: "build",
		BinarySHA256: strings.Repeat("b", 64),
		CWD:          "/project",
		ModelURL:     "http://127.0.0.1:1234/v1",
		RequestJSON:  `{"cwd":"/project"}`,
		CreatedAtMS:  100,
		ExpiresAtMS:  200,
	}
	return s, c
}

func TestVerificationNativeMigrationFromNineteenPreservesJournals(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "verification-native-migration.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	card := VerificationCard{RequestID: ID(), HostID: "host", AppID: "app", OwnerID: "owner", BridgeCommit: "build", BinarySHA256: strings.Repeat("a", 64), Token: ID(), UUID: ID(), Card: "frozen card", CreatedAtMS: 100, ExpiresAtMS: 200}
	if _, err := s.PrepareVerificationCard(ctx, card); err != nil {
		t.Fatal(err)
	}
	group := VerificationGroup{RequestID: ID(), HostID: "host", AppID: "app", OwnerID: "owner", BridgeCommit: "build", BinarySHA256: strings.Repeat("a", 64), UUID: ID(), Name: "frozen group", MembersJSON: `[` + `"owner"` + `]`, CreatedAtMS: 100, ExpiresAtMS: 200}
	if _, err := s.PrepareVerificationGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if fresh, err := s.Begin(ctx, "m0-retained", "send"); err != nil || !fresh {
		t.Fatal("m0 journal", fresh, err)
	}
	if err := s.Finish(ctx, "m0-retained", "confirmed", "om_m0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`DROP TABLE verification_native; PRAGMA user_version=19`); err != nil {
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
	if state, remote, err := s.Operation(ctx, "m0-retained"); err != nil || state != "confirmed" || remote != "om_m0" {
		t.Fatal("migration lost M0 evidence", state, remote, err)
	}
	gotCard, err := s.VerificationCard(ctx, card.RequestID, card.HostID, card.AppID, card.OwnerID)
	if err != nil || gotCard.Card != card.Card || gotCard.Token != card.Token {
		t.Fatal("migration lost card evidence", gotCard, err)
	}
	gotGroup, err := s.VerificationGroup(ctx, group.RequestID, group.HostID, group.AppID, group.OwnerID)
	if err != nil || gotGroup.Name != group.Name || gotGroup.MembersJSON != group.MembersJSON {
		t.Fatal("migration lost group evidence", gotGroup, err)
	}
	var version int
	if err := s.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 20 {
		t.Fatal("unexpected schema version", version, err)
	}
	var nativeTables int
	if err := s.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='verification_native'`).Scan(&nativeTables); err != nil || nativeTables != 1 {
		t.Fatal("verification native table missing", nativeTables, err)
	}
}

func TestVerificationNativePrepareIsImmutableAndScoped(t *testing.T) {
	ctx := context.Background()
	s, c := verificationNativeFixture(t)
	got, err := s.PrepareVerificationNative(ctx, c)
	if err != nil || got.State != "ready" || got.Generation != 0 || got.ThreadID != "" {
		t.Fatal("initial native journal", got, err)
	}

	changed := c
	changed.CWD = "/replacement"
	changed.ModelURL = "http://127.0.0.1:9876/v1"
	changed.RequestJSON = `{"cwd":"/replacement"}`
	changed.BinarySHA256 = strings.Repeat("c", 64)
	changed.ExpiresAtMS = 300
	got, err = s.PrepareVerificationNative(ctx, changed)
	if err != nil || got.CWD != c.CWD || got.ModelURL != c.ModelURL || got.RequestJSON != c.RequestJSON || got.BinarySHA256 != c.BinarySHA256 || got.ExpiresAtMS != c.ExpiresAtMS {
		t.Fatal("native journal was replaced", got, err)
	}

	foreign := changed
	foreign.OwnerID = "other-owner"
	if _, err := s.PrepareVerificationNative(ctx, foreign); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("request collision leaked other owner", err)
	}
	if _, err := s.VerificationNative(ctx, c.RequestID, "other-host", c.AppID, c.OwnerID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("unscoped native read", err)
	}
	if gotJSON, err := json.Marshal(got); err != nil || string(gotJSON) != "{}" {
		t.Fatal("native journal escaped JSON", string(gotJSON), err)
	}
}

func TestVerificationNativePrepareValidatesScopeAndPath(t *testing.T) {
	ctx := context.Background()
	s, c := verificationNativeFixture(t)
	for name, mutate := range map[string]func(*VerificationNative){
		"request id":   func(v *VerificationNative) { v.RequestID = "short" },
		"host":         func(v *VerificationNative) { v.HostID = "" },
		"hash":         func(v *VerificationNative) { v.BinarySHA256 = "short" },
		"relative cwd": func(v *VerificationNative) { v.CWD = "relative" },
		"unclean cwd":  func(v *VerificationNative) { v.CWD = "/project/../other" },
		"model":        func(v *VerificationNative) { v.ModelURL = "" },
		"request":      func(v *VerificationNative) { v.RequestJSON = "" },
		"created":      func(v *VerificationNative) { v.CreatedAtMS = 0 },
		"expiry":       func(v *VerificationNative) { v.ExpiresAtMS = v.CreatedAtMS },
	} {
		bad := c
		mutate(&bad)
		if _, err := s.PrepareVerificationNative(ctx, bad); err == nil {
			t.Fatalf("accepted invalid %s", name)
		}
	}
}

func TestVerificationNativeConcurrentClaimFreezesGeneration(t *testing.T) {
	ctx := context.Background()
	s, c := verificationNativeFixture(t)
	if _, err := s.PrepareVerificationNative(ctx, c); err != nil {
		t.Fatal(err)
	}
	const attempts = 16
	results := make(chan bool, attempts)
	errCh := make(chan error, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.ClaimVerificationNative(ctx, c, 7, strings.Repeat("a", 32), 150)
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
	got, err := s.VerificationNative(ctx, c.RequestID, c.HostID, c.AppID, c.OwnerID)
	if err != nil || got.State != "dispatching" || got.Generation != 7 || got.ConnectionID != strings.Repeat("a", 32) {
		t.Fatal("claim did not freeze generation", got, err)
	}
	if ok, err := s.ClaimVerificationNative(ctx, c, 8, strings.Repeat("b", 32), 150); err != nil || ok {
		t.Fatal("dispatching native request was reused", ok, err)
	}
}

func TestVerificationNativeClaimRejectsNonpositiveGenerationAndWindow(t *testing.T) {
	ctx := context.Background()
	s, c := verificationNativeFixture(t)
	if _, err := s.PrepareVerificationNative(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, connectionID := range []string{"", "short", strings.Repeat("A", 32), strings.Repeat("g", 32)} {
		if ok, err := s.ClaimVerificationNative(ctx, c, 1, connectionID, 150); err != nil || ok {
			t.Fatal("invalid connection claimed", connectionID, ok, err)
		}
	}
	for _, generation := range []int64{0, -1} {
		if ok, err := s.ClaimVerificationNative(ctx, c, generation, strings.Repeat("a", 32), 150); err != nil || ok {
			t.Fatal("nonpositive generation claimed", generation, ok, err)
		}
	}
	for _, now := range []int64{99, 200} {
		if ok, err := s.ClaimVerificationNative(ctx, c, 1, strings.Repeat("a", 32), now); err != nil || ok {
			t.Fatal("out-of-window native request claimed", now, ok, err)
		}
	}
	got, err := s.VerificationNative(ctx, c.RequestID, c.HostID, c.AppID, c.OwnerID)
	if err != nil || got.State != "ready" || got.Generation != 0 {
		t.Fatal("invalid claims changed native journal", got, err)
	}
}

func TestVerificationNativeFinishIsScopedAndTerminal(t *testing.T) {
	ctx := context.Background()
	s, c := verificationNativeFixture(t)
	if _, err := s.PrepareVerificationNative(ctx, c); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimVerificationNative(ctx, c, 9, strings.Repeat("a", 32), 150); err != nil || !ok {
		t.Fatal("claim", ok, err)
	}
	c.Generation = 9
	c.ConnectionID = strings.Repeat("a", 32)

	wrongGeneration := c
	wrongGeneration.Generation = 8
	if err := s.FinishVerificationNative(ctx, wrongGeneration, "confirmed", "thread-wrong-generation"); err == nil {
		t.Fatal("wrong generation finished native request")
	}
	wrongScope := c
	wrongScope.Generation = 9
	wrongScope.OwnerID = "other-owner"
	if err := s.FinishVerificationNative(ctx, wrongScope, "confirmed", "thread-wrong-scope"); err == nil {
		t.Fatal("wrong scope finished native request")
	}
	wrongHash := c
	wrongHash.Generation = 9
	wrongHash.BinarySHA256 = strings.Repeat("c", 64)
	if err := s.FinishVerificationNative(ctx, wrongHash, "confirmed", "thread-wrong-hash"); err == nil {
		t.Fatal("wrong hash finished native request")
	}
	wrongConnection := c
	wrongConnection.ConnectionID = strings.Repeat("b", 32)
	if err := s.FinishVerificationNative(ctx, wrongConnection, "confirmed", "thread-wrong-connection"); err == nil {
		t.Fatal("wrong connection finished native request")
	}
	if err := s.FinishVerificationNative(ctx, c, "confirmed", "thread-confirmed"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishVerificationNative(ctx, c, "unknown", ""); err == nil {
		t.Fatal("confirmed native request was overwritten")
	}
	if err := s.FinishVerificationNative(ctx, c, "rejected", "thread"); err == nil {
		t.Fatal("invalid rejected thread was accepted")
	}
	got, err := s.VerificationNative(ctx, c.RequestID, c.HostID, c.AppID, c.OwnerID)
	if err != nil || got.State != "confirmed" || got.ThreadID != "thread-confirmed" || got.Generation != 9 || got.ConnectionID != strings.Repeat("a", 32) {
		t.Fatal("terminal native journal changed", got, err)
	}
}
