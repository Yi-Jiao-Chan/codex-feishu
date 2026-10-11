package sync

import (
	"context"
	"errors"
	"testing"

	"codex-feishu/internal/store"
)

func TestOutboundIgnoredJournalNeverSendsRemoteWrite(t *testing.T) {
	for _, kind := range []string{"human", "progress", "final"} {
		for _, statement := range []string{
			`CREATE TRIGGER ignore_proof BEFORE INSERT ON outbound_attempts BEGIN SELECT RAISE(IGNORE); END`,
			`CREATE TRIGGER ignore_proof BEFORE UPDATE ON outbound WHEN NEW.state='dispatching' BEGIN SELECT RAISE(IGNORE); END`,
		} {
			t.Run(kind+statement, func(t *testing.T) {
				s, _, _ := outboundFixture(t, kind)
				g := &outboundGateway{s: s, t: t}
				if _, err := s.DB.Exec(statement); err != nil {
					t.Fatal(err)
				}
				o := Outbound{Store: s, Gateway: g}
				if _, err := o.DispatchOne(context.Background(), "host", "thread", 1000); !errors.Is(err, store.ErrConflict) || len(g.calls) != 0 {
					t.Fatal("unrecorded remote write", err, g.calls)
				}
			})
		}
	}
}
