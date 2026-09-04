package download

import (
	"fmt"
	"time"

	"github.com/elsbrock/go-putio"
)

type transferProgress struct {
	downloaded   int64
	lastProgress time.Time
	stalled      bool
}

// stallTracker detects Put.io transfers whose downloaded byte count remains
// unchanged while their status is DOWNLOADING. Its state is intentionally
// in-memory so a restart always establishes a fresh observation baseline. It
// is owned by the monitor goroutine; RPC only sees annotated snapshots.
type stallTracker struct {
	timeout  time.Duration
	now      func() time.Time
	progress map[int64]transferProgress
}

func newStallTracker(timeout time.Duration, now func() time.Time) *stallTracker {
	return &stallTracker{
		timeout:  timeout,
		now:      now,
		progress: make(map[int64]transferProgress),
	}
}

func (s *stallTracker) Observe(transfers []*putio.Transfer) {
	if s.timeout <= 0 {
		clear(s.progress)
		return
	}

	now := s.now()
	active := make(map[int64]struct{}, len(transfers))
	for _, transfer := range transfers {
		if transfer.Status != "DOWNLOADING" {
			continue
		}

		active[transfer.ID] = struct{}{}
		progress, exists := s.progress[transfer.ID]
		if !exists || progress.downloaded != transfer.Downloaded {
			s.progress[transfer.ID] = transferProgress{
				downloaded:   transfer.Downloaded,
				lastProgress: now,
			}
			continue
		}

		if now.Sub(progress.lastProgress) >= s.timeout {
			progress.stalled = true
			s.progress[transfer.ID] = progress
		}
	}

	for transferID := range s.progress {
		if _, exists := active[transferID]; !exists {
			delete(s.progress, transferID)
		}
	}
}

func (s *stallTracker) Error(transferID int64) string {
	if progress, exists := s.progress[transferID]; exists && progress.stalled {
		return fmt.Sprintf(
			"Put.io transfer stalled: no byte progress for %s; inspect the transfer in Put.io",
			s.timeout,
		)
	}
	return ""
}
