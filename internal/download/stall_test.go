package download

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
)

type stallTestClient struct {
	transfer *putio.Transfer
}

func (c *stallTestClient) GetTransfers(context.Context) ([]*putio.Transfer, error) {
	transferCopy := *c.transfer
	return []*putio.Transfer{&transferCopy}, nil
}

func (*stallTestClient) GetAllTransferFiles(context.Context, int64) ([]*putio.File, error) {
	return nil, nil
}

func (*stallTestClient) GetFiles(context.Context, int64) ([]*putio.File, error) {
	return nil, nil
}

func (*stallTestClient) RetryTransfer(context.Context, int64) (*putio.Transfer, error) {
	return nil, nil
}

func (*stallTestClient) DeleteTransfer(context.Context, int64) error { return nil }
func (*stallTestClient) DeleteFile(context.Context, int64) error     { return nil }
func (*stallTestClient) GetDownloadURL(context.Context, int64) (string, error) {
	return "", nil
}

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestStallTrackerProgressTimeoutAndRecovery(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)}
	tracker := newStallTracker(2*time.Hour, clock.Now)
	transfer := &putio.Transfer{ID: 42, Status: "DOWNLOADING", Downloaded: 100}

	tracker.Observe([]*putio.Transfer{transfer})
	clock.Advance(90 * time.Minute)
	tracker.Observe([]*putio.Transfer{transfer})
	if got := tracker.Error(transfer.ID); got != "" {
		t.Fatalf("active transfer reported stalled before timeout: %q", got)
	}

	transfer.Downloaded = 200
	tracker.Observe([]*putio.Transfer{transfer})
	clock.Advance(2 * time.Hour)
	tracker.Observe([]*putio.Transfer{transfer})
	if got := tracker.Error(transfer.ID); !strings.Contains(got, "no byte progress for 2h0m0s") {
		t.Fatalf("stalled transfer error = %q, want timeout detail", got)
	}

	transfer.Downloaded = 201
	tracker.Observe([]*putio.Transfer{transfer})
	if got := tracker.Error(transfer.ID); got != "" {
		t.Fatalf("progress did not immediately clear stalled error: %q", got)
	}
}

func TestStallTrackerRestartEstablishesFreshBaseline(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)}
	transfer := &putio.Transfer{ID: 42, Status: "DOWNLOADING", Downloaded: 100}

	beforeRestart := newStallTracker(time.Hour, clock.Now)
	beforeRestart.Observe([]*putio.Transfer{transfer})
	clock.Advance(time.Hour)
	beforeRestart.Observe([]*putio.Transfer{transfer})
	if got := beforeRestart.Error(transfer.ID); got == "" {
		t.Fatal("transfer was not stalled before restart")
	}

	afterRestart := newStallTracker(time.Hour, clock.Now)
	afterRestart.Observe([]*putio.Transfer{transfer})
	if got := afterRestart.Error(transfer.ID); got != "" {
		t.Fatalf("fresh tracker immediately reported transfer stalled: %q", got)
	}
}

func TestStallTrackerDisabledAndInactiveTransfers(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)}
	disabled := newStallTracker(0, clock.Now)
	transfer := &putio.Transfer{ID: 42, Status: "DOWNLOADING"}

	disabled.Observe([]*putio.Transfer{transfer})
	clock.Advance(24 * time.Hour)
	disabled.Observe([]*putio.Transfer{transfer})
	if got := disabled.Error(transfer.ID); got != "" {
		t.Fatalf("disabled tracker reported stalled transfer: %q", got)
	}

	enabled := newStallTracker(time.Hour, clock.Now)
	enabled.Observe([]*putio.Transfer{transfer})
	clock.Advance(time.Hour)
	enabled.Observe([]*putio.Transfer{transfer})
	if got := enabled.Error(transfer.ID); got == "" {
		t.Fatal("transfer was not stalled before leaving downloading state")
	}

	transfer.Status = "COMPLETED"
	enabled.Observe([]*putio.Transfer{transfer})
	if got := enabled.Error(transfer.ID); got != "" {
		t.Fatalf("inactive transfer retained stalled error: %q", got)
	}
}

func TestTransferProcessorPublishesStallStateConcurrently(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)}
	client := &stallTestClient{transfer: &putio.Transfer{
		ID:           42,
		Status:       "DOWNLOADING",
		Downloaded:   100,
		SaveParentID: 7,
	}}
	manager := New(&config.Config{
		FolderID:               7,
		TargetDir:              t.TempDir(),
		WorkerCount:            1,
		StalledTransferTimeout: time.Hour,
	}, client)
	manager.processor.stallTracker = newStallTracker(time.Hour, clock.Now)

	manager.processor.checkTransfers()
	clock.Advance(time.Hour)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			manager.processor.checkTransfers()
		}
	}()
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_ = manager.GetTransfers()
			}
		}()
	}
	wg.Wait()

	transfers := manager.GetTransfers()
	if got := len(transfers); got != 1 {
		t.Fatalf("published transfer count = %d, want 1", got)
	}
	if got := transfers[0].ErrorMessage; got == "" {
		t.Fatal("processor did not publish the detected stall through the manager")
	}
}
