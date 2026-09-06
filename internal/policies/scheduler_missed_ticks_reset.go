package policies

import "time"

// ClearMissedTicks drops in-memory maintenance-blocked scheduler ticks when the
// backing persistence epoch changes (for example after restoring a backup).
// Those ticks describe the pre-replacement policy/inventory state and must not
// be replayed against the restored database.
func (s *Service) ClearMissedTicks() {
	if s == nil {
		return
	}
	s.missedTickMu.Lock()
	s.missedTicks = map[string]time.Time{}
	s.missedTickMu.Unlock()
}
