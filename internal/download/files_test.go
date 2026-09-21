package download

import (
	"fmt"
	"os"
	"reflect"
	"testing"
)

func TestTransferFileStorePersists(t *testing.T) {
	dir := t.TempDir()
	store := newTransferFileStore(dir)
	want := []TransferFile{{Name: "Book/book.m4b", Length: 42}}
	if err := store.Set(101, want); err != nil {
		t.Fatal(err)
	}

	got, ok := newTransferFileStore(dir).Get(101)
	if !ok {
		t.Fatal("persisted transfer file manifest was not loaded")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest = %+v, want %+v", got, want)
	}
}

func TestTransferFileStoreRemovePersists(t *testing.T) {
	dir := t.TempDir()
	store := newTransferFileStore(dir)
	if err := store.Set(101, []TransferFile{{Name: "Book/book.m4b", Length: 42}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(101); err != nil {
		t.Fatal(err)
	}

	if _, ok := newTransferFileStore(dir).Get(101); ok {
		t.Fatal("removed transfer file manifest was restored")
	}
}

func TestTransferFileStoreRejectsEmptyManifest(t *testing.T) {
	store := newTransferFileStore(t.TempDir())
	if err := store.Set(101, nil); err == nil {
		t.Fatal("expected empty manifest to fail")
	}
	if _, err := os.Stat(store.path(101)); !os.IsNotExist(err) {
		t.Fatalf("empty manifest created state file: %v", err)
	}
}

// A concurrent reader (another process, or this one after a crash) must never
// observe a half-written ownership record: a truncated manifest parses as
// corrupt and fails every transfer closed.
func TestTransferFileStoreWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	files := make([]TransferFile, 0, 2000)
	for i := range cap(files) {
		files = append(files, TransferFile{Name: fmt.Sprintf("Show/S01/episode-%04d.mkv", i), Length: int64(i + 1)})
	}
	writer, reader := newTransferFileStore(dir), newTransferFileStore(dir)
	if err := writer.Set(101, files); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	reads := make(chan error, 1)
	go func() {
		var failure error
		observed := 0
		for {
			got, err := reader.load(101)
			if err != nil {
				failure = err
			} else if len(got) != len(files) {
				failure = fmt.Errorf("partial manifest: %d of %d files", len(got), len(files))
			} else {
				observed++
			}
			select {
			case <-done:
				if observed == 0 && failure == nil {
					failure = fmt.Errorf("reader never observed the manifest")
				}
				reads <- failure
				return
			default:
			}
		}
	}()
	for i := 0; i < 50; i++ {
		files[0].Length = int64(i + 1)
		if err := writer.Set(101, files); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	if err := <-reads; err != nil {
		t.Fatalf("concurrent read of a rewritten manifest failed: %v", err)
	}
	if entries, err := os.ReadDir(newTransferFileStore(dir).stateDir); err != nil || len(entries) != 1 {
		t.Fatalf("publication left temporary state behind: %+v %v", entries, err)
	}
}
