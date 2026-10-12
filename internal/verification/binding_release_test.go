package verification

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/store"
)

func startHeldBindingModel(t *testing.T, m *BindingManager, id string) <-chan error {
	t.Helper()
	entry := m.Sessions.active[id]
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, entry.model.URL()+"/responses", strings.NewReader(`{"model":"fixture-model","stream":true}`))
		if err != nil {
			done <- err
			return
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- err
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err == nil && !strings.Contains(string(body), `"type":"response.completed"`) {
			err = errors.New("fixture response did not complete")
		}
		done <- err
	}()
	select {
	case <-entry.model.Started():
	case err := <-done:
		t.Fatal("model request did not reach held gate", err)
	case <-time.After(2 * time.Second):
		t.Fatal("model request did not start")
	}
	return done
}

func TestBindingReleaseOpensOnlyOriginalHeldModelAndQueriesDoNotRelease(t *testing.T) {
	m, n, g, id := bindingFixture(t)
	ctx := context.Background()
	initial, err := m.Begin(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	done := startHeldBindingModel(t, m, id)
	reads, joins := g.reads.Load(), n.joins.Load()
	state, err := m.State(ctx, id)
	if err != nil || state.ModelReleased || g.reads.Load() != reads || n.joins.Load() != joins {
		t.Fatal("query opened gate or invoked verification APIs", state, err)
	}
	select {
	case <-done:
		t.Fatal("query completed held response")
	default:
	}
	for range 2 {
		r, err := m.Release(ctx, id)
		if err != nil || !r.ModelReleased || r.ActivationRequested || r.BoundAtMS != initial.BoundAtMS || r.ExpiresAtMS != initial.ExpiresAtMS {
			t.Fatal("release replaced or extended original binding", r, err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("authorized release did not complete local fixture response")
	}
	reads, joins = g.reads.Load(), n.joins.Load()
	state, err = m.State(ctx, id)
	if err != nil || !state.ModelReleased || g.reads.Load() != reads || n.joins.Load() != joins || n.calls.Load() != 1 || g.calls.Load() != 1 || m.Sessions.active[id].model.Calls() != 1 {
		t.Fatal("release/query recreated components or requests", state, err)
	}
}

func TestBindingReleaseRequiresRegisteredPairAndStartedOriginalModel(t *testing.T) {
	m, _, _, id := bindingFixture(t)
	ctx := context.Background()
	if _, err := m.Release(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("release implicitly registered", err)
	}
	if _, err := m.Release(ctx, store.ID()); !errors.Is(err, ErrNotFound) {
		t.Fatal("release created unknown request", err)
	}
	if _, err := m.Begin(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, id); !errors.Is(err, ErrNotReady) {
		t.Fatal("release before first model request skipped busy window", err)
	}
	if model := m.Sessions.active[id].model; model.Calls() != 0 || model.Released() {
		t.Fatal("release started/opened model itself")
	}
}

func TestBindingReleaseRechecksCurrentAuthorityBeforeOpeningGate(t *testing.T) {
	for _, reason := range []string{"owner-missing", "extra-member", "member-get-error", "bot-missing", "new-connection", "old-build", "expired", "join-rejected", "canceled", "route-removed"} {
		t.Run(reason, func(t *testing.T) {
			m, n, g, id := bindingFixture(t)
			ctx := context.Background()
			if _, err := m.Begin(ctx, id); err != nil {
				t.Fatal(err)
			}
			startHeldBindingModel(t, m, id)
			switch reason {
			case "owner-missing":
				g.members = []string{"bot"}
			case "extra-member":
				g.members = append(g.members, "other")
			case "member-get-error":
				g.memberErr = errors.New("PRIVATE API payload")
			case "bot-missing":
				g.bot = false
			case "new-connection":
				n.connection = store.ID()
			case "old-build":
				m.Sessions.native.Commit = "different-build"
			case "expired":
				m.Sessions.native.Now = func() time.Time { return time.UnixMilli(2_000_000) }
			case "join-rejected":
				n.join = func(context.Context, string, codexclient.LocalVerificationSpec) (codexclient.Thread, error) {
					return codexclient.Thread{}, errors.New("PRIVATE native reply")
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "route-removed":
				if _, err := m.Sessions.native.Store.DB.Exec(`DELETE FROM bindings WHERE host_id=? AND thread_id=?`, m.Groups.HostID, "owned-thread"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.Release(ctx, id); err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("invalid release accepted or private error leaked", err)
			}
			if m.Sessions.active[id].model.Released() || n.calls.Load() != 1 || g.calls.Load() != 1 {
				t.Fatal("failed release opened gate or created components")
			}
		})
	}
}
