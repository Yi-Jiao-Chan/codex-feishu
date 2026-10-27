package app

import (
	"context"
	"time"
)

// compactDeliveryPayloads drains legacy settled card and CoT bodies in small
// transactions. Current state transitions compact their own rows, so one
// successful startup pass is sufficient and avoids rescanning the journal.
func (s *Service) compactDeliveryPayloads(ctx context.Context) {
	delay := time.Duration(0)
	for {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		count, err := s.Store.CompactDeliveryPayloads(ctx, s.Config.Host.ID, 64)
		switch {
		case err != nil:
			s.notice("delivery.payload_compaction_pending")
			delay = 30 * time.Second
		case count == 0:
			return
		default:
			delay = 250 * time.Millisecond
		}
	}
}
