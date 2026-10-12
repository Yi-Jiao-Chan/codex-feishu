package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/store"
	"codex-feishu/internal/verification"
)

type healthNative struct {
	*serviceNative
	done chan struct{}
}

func TestHealthBuildIdentityComesFromThisScopedRunningArtifact(t *testing.T) {
	db, cfg, _ := serviceFixture(t)
	m := &verification.Manager{HostID: cfg.Host.ID, AppID: cfg.Feishu.AppID, OwnerID: cfg.Access.OwnerOpenID, Commit: strings.Repeat("a", 40), BinarySHA256: strings.Repeat("b", 64)}
	s := &Service{Store: db, Config: cfg, Verification: m}
	live, err := s.Health(context.Background())
	if err != nil || live.BridgeCommit != m.Commit || live.BinarySHA256 != m.BinarySHA256 {
		t.Fatal("scoped build metadata missing", err)
	}
	m.OwnerID = "ou_other_owner"
	live, err = s.Health(context.Background())
	if err != nil || live.BridgeCommit != "" || live.BinarySHA256 != "" {
		t.Fatal("unrelated manager supplied artifact identity", err)
	}
	s.Verification = nil
	live, err = s.Health(context.Background())
	if err != nil || live.BridgeCommit != "" || live.BinarySHA256 != "" {
		t.Fatal("missing build metadata inferred from readiness", err)
	}
}

func (n *healthNative) Done() <-chan struct{} { return n.done }

func TestHealthReadsExistingConnectionsAndHydrationWithoutBodies(t *testing.T) {
	db, cfg, _ := serviceFixture(t)
	gateway := &serviceGateway{}
	gateway.ready.Store(true)
	s := &Service{Store: db, Config: cfg, Gateway: gateway, actionsReady: map[string]bool{"thread": true, "unrelated": true}}
	s.running.Store(true)
	s.startedAtMS.Store(time.Now().Add(-time.Second).UnixMilli())
	s.nativeConnected.Store(true)
	live, err := s.Health(context.Background())
	if err != nil || !live.Running || !live.NativeConnected || !live.FeishuConnected || live.HydratedBindings != 1 || live.Persistent.Bindings != 1 {
		t.Fatal("actual generation facts unavailable", live, err)
	}
	b, _ := json.Marshal(live)
	// Match the actual quoted routing ID, not a capability field name such as
	// verification_thread_creation_supported.
	if strings.Contains(string(b), `"thread"`) || strings.Contains(string(b), cfg.Feishu.AppID) || strings.Contains(string(b), cfg.Access.OwnerOpenID) {
		t.Fatal("health exposed routing identities", string(b))
	}
	s.nativeConnected.Store(false)
	live, err = s.Health(context.Background())
	if err != nil || live.NativeConnected || live.HydratedBindings != 0 || !live.FeishuConnected {
		t.Fatal("offline generation reported hydrated", live, err)
	}
	s.running.Store(false)
	live, err = s.Health(context.Background())
	if err != nil || live.FeishuConnected || live.NativeConnected {
		t.Fatal("stopped service claimed connected", live, err)
	}
}

func TestHealthAndButtonsFailClosedBeforeDisconnectedActorsJoin(t *testing.T) {
	db, cfg, root := serviceFixture(t)
	native := &healthNative{serviceNative: serviceTestNative(root), done: make(chan struct{})}
	s := &Service{Store: db, Config: cfg, current: &Runtime{Native: native}, actionsReady: map[string]bool{"thread": true}}
	s.running.Store(true)
	s.nativeConnected.Store(true)
	if !s.NativeConnected() || !s.actionAvailable("thread") {
		t.Fatal("healthy native connection unavailable")
	}
	// The socket reader has stopped, but Runtime may still be joining an
	// actor with a bounded Feishu HTTP request. No event is consumed here.
	close(native.done)
	if s.NativeConnected() || s.actionAvailable("thread") {
		t.Fatal("closed socket still enabled native operations")
	}
	live, err := s.Health(context.Background())
	if err != nil || live.NativeConnected || live.HydratedBindings != 0 {
		t.Fatal("closed native reader reported healthy", live, err)
	}
}

func TestHealthPublishesActualInitialDiscoveryBaseline(t *testing.T) {
	_, cfg, _ := serviceFixture(t)
	cfg.Storage.DatabaseFile = filepath.Join(t.TempDir(), "fresh.db")
	db, err := store.Open(cfg.Storage.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.BeginBaseline(context.Background(), cfg.Host.ID, cfg.Feishu.AppID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	cfg.Discovery.Enabled = true
	s := &Service{Store: db, Config: cfg}
	live, err := s.Health(context.Background())
	if err != nil || !live.DiscoveryEnabled || live.Persistent.BaselineComplete {
		t.Fatal("initial baseline invented from a live configuration", live, err)
	}
	if err := db.CompleteBaseline(context.Background(), cfg.Host.ID, nil); err != nil {
		t.Fatal(err)
	}
	live, err = s.Health(context.Background())
	if err != nil || !live.Persistent.BaselineComplete {
		t.Fatal("actual initial history snapshot not reflected", live, err)
	}
}
