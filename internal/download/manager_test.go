package download

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/log"
)

// TestMain silences logging so test output shows test results, not the
// download manager's very chatty INFO stream.
func TestMain(m *testing.M) {
	log.SetLevel(log.LevelNone)
	os.Exit(m.Run())
}

// fakeClient is a PutioClient stub. Each field lets a test override one call;
// the zero value returns empty results without erroring.
type fakeClient struct {
	transfers      func() ([]*putio.Transfer, error)
	files          func(fileID int64) ([]*putio.File, error)
	folderContents func(folderID int64) ([]*putio.File, error)
	downloadURL    func(fileID int64) (string, error)
	deletedFiles   chan int64
}

func (f *fakeClient) GetTransfers(context.Context) ([]*putio.Transfer, error) {
	if f.transfers != nil {
		return f.transfers()
	}
	return nil, nil
}

func (f *fakeClient) GetAllTransferFiles(_ context.Context, fileID int64) ([]*putio.File, error) {
	if f.files != nil {
		return f.files(fileID)
	}
	return nil, nil
}

// GetFiles backs managedFolders' discovery of put.io category subfolders.
func (f *fakeClient) GetFiles(_ context.Context, folderID int64) ([]*putio.File, error) {
	if f.folderContents != nil {
		return f.folderContents(folderID)
	}
	return nil, nil
}

func (f *fakeClient) RetryTransfer(context.Context, int64) (*putio.Transfer, error) {
	return &putio.Transfer{}, nil
}

func (f *fakeClient) DeleteTransfer(context.Context, int64) error { return nil }

func (f *fakeClient) DeleteFile(_ context.Context, fileID int64) error {
	if f.deletedFiles != nil {
		f.deletedFiles <- fileID
	}
	return nil
}

func (f *fakeClient) GetDownloadURL(_ context.Context, fileID int64) (string, error) {
	if f.downloadURL != nil {
		return f.downloadURL(fileID)
	}
	return "", nil
}

// newManagerForTest builds a Manager backed by client, rooted at a temp dir.
func newManagerForTest(t *testing.T, client PutioClient) *Manager {
	t.Helper()
	return New(&config.Config{
		TargetDir:   t.TempDir(),
		FolderID:    testFolderID,
		WorkerCount: 1,
	}, client)
}

const testFolderID = 42

// Regression test: checkTransfers republishes the transfer list from the
// monitor goroutine while torrent-get reads it from an HTTP handler
// goroutine. This previously raced on a plain map field. Meaningful under
// -race, which CI runs.
func TestGetTransfersIsRaceFreeWithCheckTransfers(t *testing.T) {
	client := &fakeClient{
		transfers: func() ([]*putio.Transfer, error) {
			return []*putio.Transfer{
				{ID: 1, Name: "a", Status: "DOWNLOADING", SaveParentID: testFolderID},
				{ID: 2, Name: "b", Status: "COMPLETED", SaveParentID: testFolderID},
				{ID: 3, Name: "c", Status: "ERROR", SaveParentID: 999}, // different folder
			}, nil
		},
	}
	m := newManagerForTest(t, client)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			m.processor.checkTransfers()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			for _, tr := range m.GetTransfers() {
				_ = tr.Name
			}
		}
	}()
	wg.Wait()

	// Only transfers in the configured folder are published.
	got := m.GetTransfers()
	if len(got) != 2 {
		t.Fatalf("expected 2 transfers from the configured folder, got %d", len(got))
	}
	for _, tr := range got {
		if tr.SaveParentID != testFolderID {
			t.Errorf("transfer %d from foreign folder %d leaked into results", tr.ID, tr.SaveParentID)
		}
	}
}

// GetTransfers must hand back a copy so callers cannot mutate the published
// slice out from under the monitor.
func TestGetTransfersReturnsCopy(t *testing.T) {
	client := &fakeClient{
		transfers: func() ([]*putio.Transfer, error) {
			return []*putio.Transfer{
				{ID: 1, Name: "a", Status: "SEEDING", SaveParentID: testFolderID},
			}, nil
		},
	}
	m := newManagerForTest(t, client)
	m.processor.checkTransfers()

	first := m.GetTransfers()
	if len(first) != 1 {
		t.Fatalf("expected 1 transfer, got %d", len(first))
	}
	first[0] = nil // caller scribbles on its copy

	second := m.GetTransfers()
	if len(second) != 1 || second[0] == nil {
		t.Fatal("mutating the returned slice affected the processor's copy")
	}
}

// No transfers yet (or none in our folder) must not report stale or bogus data.
func TestGetTransfersEmptyBeforeFirstPoll(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	if got := m.GetTransfers(); len(got) != 0 {
		t.Fatalf("expected no transfers before the first poll, got %d", len(got))
	}
}
