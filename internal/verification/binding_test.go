package verification

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

type bindingCreator struct {
	*nativeCreator
	joins atomic.Int32
	join  func(context.Context, string, codexclient.LocalVerificationSpec) (codexclient.Thread, error)
}

func (n *bindingCreator) AttachLocalVerification(ctx context.Context, id string, s codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
	n.joins.Add(1)
	return n.join(ctx, id, s)
}

func bindingFixture(t *testing.T) (*BindingManager, *bindingCreator, *groupGateway, string) {
	t.Helper()
	m, n, _, _ := nativeFixture(t)
	ctx := context.Background()
	if _, err := m.Store.BeginBaseline(ctx, m.HostID, m.AppID, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.CompleteBaseline(ctx, m.HostID, nil); err != nil {
		t.Fatal(err)
	}
	reader := &bindingCreator{nativeCreator: n}
	reader.join = func(_ context.Context, id string, s codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
		if id != "owned-thread" {
			t.Error("joined foreign thread")
		}
		th := nativeThread(s)
		th.Preview = "fixture input"
		th.Turns = []codexclient.Turn{{ID: "fixture-turn", Status: "inProgress"}}
		return th, nil
	}
	m.Current = func() NativeCreator { return reader }
	root := filepath.Join(t.TempDir(), "bindings")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := NewNativeSessions(ctx, m, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	gateway := &groupGateway{bot: true, members: []string{m.OwnerID}, create: func(context.Context, string, string, []string, string) (string, error) { return "oc_test", nil }}
	groups := &GroupManager{Store: m.Store, Gateway: gateway, HostID: m.HostID, AppID: m.AppID, OwnerID: m.OwnerID, BotID: "bot", Commit: m.Commit, BinarySHA256: m.BinarySHA256, Now: m.Now, Ready: func() bool { return true }}
	id := store.ID()
	if _, err := groups.Begin(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := groups.Message(ctx, m.OwnerID, id, "om_owner_group", "oc_test"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := s.Begin(ctx, id); err != nil {
		t.Fatal(err)
	}
	return &BindingManager{Sessions: s, Groups: groups}, reader, gateway, id
}

func TestVerificationBindingManagerRegistersOnlyOwnedConfirmedPair(t *testing.T) {
	m, n, g, id := bindingFixture(t)
	ctx := context.Background()
	b, err := m.Begin(ctx, id)
	if err != nil || b.BindingStatus != "pass" || b.ThreadID != "owned-thread" || b.ChatID != "oc_test" || !b.ResourcesReady || !b.CreatorConnected {
		t.Fatal(b, err)
	}
	if n.calls.Load() != 1 || n.joins.Load() != 1 || g.calls.Load() != 1 {
		t.Fatal("binding recreated remote component")
	}
	stored, err := m.Sessions.native.Store.VerificationBinding(ctx, id, m.Groups.HostID, m.Groups.AppID, m.Groups.OwnerID)
	if err != nil || stored.BoundAtMS != m.Sessions.native.now().UnixMilli() {
		t.Fatal(stored, err)
	}
	for range 3 {
		r, err := m.Begin(ctx, id)
		if err != nil || r.BoundAtMS != b.BoundAtMS || r.ExpiresAtMS != b.ExpiresAtMS {
			t.Fatal("replay changed registration", r, err)
		}
	}
	reads, joins := g.reads.Load(), n.joins.Load()
	if _, err := m.State(ctx, id); err != nil || g.reads.Load() != reads || n.joins.Load() != joins {
		t.Fatal("state performed GET/join", err)
	}
	if n.calls.Load() != 1 || g.calls.Load() != 1 {
		t.Fatal("replay created remote components")
	}
	if err := m.Sessions.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := m.State(ctx, id)
	if err != nil || r.BindingStatus != "pass" || r.ResourcesReady {
		t.Fatal("lost history or reopened resources", r, err)
	}
}

func TestVerificationBindingManagerInvalidatesChangedPrerequisites(t *testing.T) {
	for _, reason := range []string{"owner-missing", "extra-person", "bot-missing", "member-get-error", "group-input-missing", "unknown-birth", "old-build", "expired", "changed-connection", "join-rejected", "identity-mismatch", "nonce-changes-in-join", "resources-closed", "group-scope"} {
		t.Run(reason, func(t *testing.T) {
			m, n, g, id := bindingFixture(t)
			native := m.Sessions.native
			switch reason {
			case "owner-missing":
				g.members = []string{"bot"}
			case "extra-person":
				g.members = []string{native.OwnerID, "other"}
			case "bot-missing":
				g.bot = false
			case "member-get-error":
				g.memberErr = errors.New("PRIVATE group body")
			case "group-input-missing":
				_, err := native.Store.DB.Exec(`UPDATE verification_groups SET group_message_id='' WHERE request_id=?`, id)
				if err != nil {
					t.Fatal(err)
				}
			case "unknown-birth":
				_, err := native.Store.DB.Exec(`UPDATE verification_native SET state='unknown',thread_id='' WHERE request_id=?`, id)
				if err != nil {
					t.Fatal(err)
				}
			case "old-build":
				_, err := native.Store.DB.Exec(`UPDATE verification_native SET binary_sha256=? WHERE request_id=?`, strings.Repeat("c", 64), id)
				if err != nil {
					t.Fatal(err)
				}
			case "expired":
				native.Now = func() time.Time { return time.UnixMilli(2_000_000) }
			case "changed-connection":
				n.connection = store.ID()
			case "join-rejected":
				n.join = func(context.Context, string, codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
					return codexclient.Thread{}, errors.New("PRIVATE reply")
				}
			case "identity-mismatch":
				n.join = func(_ context.Context, _ string, s codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
					th := nativeThread(s)
					th.ID = "foreign"
					return th, nil
				}
			case "nonce-changes-in-join":
				n.join = func(_ context.Context, _ string, s codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
					n.connection = store.ID()
					return nativeThread(s), nil
				}
			case "resources-closed":
				m.Sessions.Close()
			case "group-scope":
				m.Groups.OwnerID = "foreign"
			}
			_, err := m.Begin(context.Background(), id)
			if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("invalid binding accepted/raw error leaked", err)
			}
			var count int
			if err := native.Store.DB.QueryRow(`SELECT count(*) FROM verification_bindings`).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed guard registered", count, err)
			}
			if n.calls.Load() != 1 || g.calls.Load() != 1 {
				t.Fatal("failed guard recreated component")
			}
		})
	}
}

func TestVerificationBindingManagerCloseCancelsJoinBeforeRegistration(t *testing.T) {
	m, n, _, id := bindingFixture(t)
	entered := make(chan struct{})
	n.join = func(ctx context.Context, _ string, _ codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
		close(entered)
		<-ctx.Done()
		return codexclient.Thread{}, ctx.Err()
	}
	result := make(chan error, 1)
	go func() { _, err := m.Begin(context.Background(), id); result <- err }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- m.Sessions.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resource close did not join canceled binding")
	}
	if err := <-result; err == nil {
		t.Fatal("canceled join registered")
	}
	if _, err := m.State(context.Background(), id); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestVerificationBindingStateDoesNotPretendDeletedRouteIsActive(t *testing.T) {
	m, _, _, id := bindingFixture(t)
	if _, err := m.Begin(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Sessions.native.Store.DB.Exec(`DELETE FROM bindings`); err != nil {
		t.Fatal(err)
	}
	r, err := m.State(context.Background(), id)
	if err != nil || r.BindingStatus != "pending" {
		t.Fatal("missing durable route claimed registered", r, err)
	}
	if _, err := m.Begin(context.Background(), id); !errors.Is(err, store.ErrConflict) {
		t.Fatal("replay repaired/adopted missing route", err)
	}
	if _, err := m.Sessions.native.Store.Binding(context.Background(), m.Groups.HostID, r.ThreadID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("deleted route recreated", err)
	}
}

func TestVerificationBindingModelStopsDuringJoin(t *testing.T) {
	for _, waitForCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "legal-response", true: "cancel-waiting-join"}[waitForCancel], func(t *testing.T) {
			m, n, _, id := bindingFixture(t)
			n.join = func(ctx context.Context, _ string, s codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
				if err := m.Sessions.active[id].model.Close(); err != nil {
					t.Error(err)
				}
				if waitForCancel {
					select {
					case <-ctx.Done():
					case <-time.After(time.Second):
						t.Error("stopped model did not cancel join")
					}
				}
				return nativeThread(s), nil
			}
			if _, err := m.Begin(context.Background(), id); err == nil {
				t.Fatal("stopped model registered a binding")
			}
			for _, table := range []string{"verification_bindings", "bindings", "create_intents", "thread_observations"} {
				var count int
				if err := m.Sessions.native.Store.DB.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("stopped model wrote derived binding", table, count, err)
				}
			}
		})
	}
}
