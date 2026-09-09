package download

import "testing"

func TestCleanupNeverDeletesPutioRoot(t *testing.T) {
	deletedFiles := make(chan int64, 1)
	m := newManagerForTest(t, &fakeClient{deletedFiles: deletedFiles})
	m.coordinator.InitiateTransfer(101, "completed", 0, 0)
	if err := m.coordinator.StartDownload(101); err != nil {
		t.Fatal(err)
	}

	m.cleanupTransfer(101)

	select {
	case fileID := <-deletedFiles:
		t.Fatalf("DeleteFile called with %d, want no call for Put.io root", fileID)
	default:
	}
	ctx, ok := m.coordinator.GetTransferContext(101)
	if !ok || ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("transfer was not marked processed: context=%v exists=%v", ctx, ok)
	}
}
