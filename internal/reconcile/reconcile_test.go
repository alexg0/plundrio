package reconcile

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/elsbrock/go-putio"
)

type fakeClient struct {
	transfers []*putio.Transfer
	files     map[int64][]*putio.File
}

func (f *fakeClient) GetTransfers(context.Context) ([]*putio.Transfer, error) {
	return f.transfers, nil
}

func (f *fakeClient) GetFiles(_ context.Context, folderID int64) ([]*putio.File, error) {
	return f.files[folderID], nil
}

func TestReconcileReportsOnlyUnownedObjectsAsUnmanaged(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "tv", "Active.Show"))
	mustWrite(t, filepath.Join(root, "tv", "Active.Show", "episode.mkv"), "active")
	mustWrite(t, filepath.Join(root, "tv", "leftover.mkv"), "leftover")
	mustWrite(t, filepath.Join(root, stateFileName), `{"active-hash":"tv"}`)

	client := &fakeClient{
		transfers: []*putio.Transfer{{
			ID: 10, FileID: 100, Hash: "active-hash", Name: "Active.Show", SaveParentID: 1,
		}},
		files: map[int64][]*putio.File{
			1: {
				putioFile(100, "Active.Show", 1, true, 6),
				putioFile(200, "leftover.mkv", 1, false, 8),
			},
			100: {putioFile(101, "episode.mkv", 100, false, 6)},
		},
	}

	report, err := New(client, 1, root).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	wantActive := []string{"local:tv/Active.Show", "putio:Active.Show"}
	wantUnmanaged := []string{"local:tv/leftover.mkv", "putio:leftover.mkv"}
	if got := objectLabels(report.Active); !reflect.DeepEqual(got, wantActive) {
		t.Fatalf("active objects = %v, want %v", got, wantActive)
	}
	if got := objectLabels(report.Unmanaged); !reflect.DeepEqual(got, wantUnmanaged) {
		t.Fatalf("unmanaged objects = %v, want %v", got, wantUnmanaged)
	}
	if report.Summary.UnmanagedCount != 2 || report.Summary.UnmanagedBytes != 16 {
		t.Fatalf("unexpected unmanaged summary: %+v", report.Summary)
	}
}

func TestReconcileHandlesNestedAndEmptyRootsDeterministically(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "category", "active"))
	mustWrite(t, filepath.Join(root, "category", "active", "file"), "123")
	mustWrite(t, filepath.Join(root, "category", "z-unmanaged"), "12")
	mustWrite(t, filepath.Join(root, "category", "a-unmanaged"), "1")
	mustWrite(t, filepath.Join(root, stateFileName), `{"nested-hash":"category"}`)

	client := &fakeClient{
		transfers: []*putio.Transfer{{ID: 1, FileID: 11, Hash: "nested-hash", Name: "active", SaveParentID: 7}},
		files: map[int64][]*putio.File{
			7:  {putioFile(10, "container", 7, true, 0)},
			10: {putioFile(13, "z-unmanaged", 10, false, 2), putioFile(11, "active", 10, true, 3), putioFile(12, "a-unmanaged", 10, false, 1)},
			11: {putioFile(14, "file", 11, false, 3)},
		},
	}

	service := New(client, 7, root)
	first, err := service.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("unchanged reports differ:\n%s\n%s", firstJSON, secondJSON)
	}

	want := []string{
		"local:category/a-unmanaged",
		"local:category/z-unmanaged",
		"putio:container/a-unmanaged",
		"putio:container/z-unmanaged",
	}
	if got := objectLabels(first.Unmanaged); !reflect.DeepEqual(got, want) {
		t.Fatalf("unmanaged objects = %v, want %v", got, want)
	}

	empty, err := New(&fakeClient{files: map[int64][]*putio.File{}}, 9, t.TempDir()).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if empty.Active == nil || empty.Unmanaged == nil || len(empty.Active) != 0 || len(empty.Unmanaged) != 0 {
		t.Fatalf("empty report should contain empty arrays: %+v", empty)
	}
}

func TestReconcileRejectsUnsafeActiveLocalPath(t *testing.T) {
	root := t.TempDir()
	client := &fakeClient{
		transfers: []*putio.Transfer{{ID: 1, Name: "../../../outside", SaveParentID: 1}},
		files:     map[int64][]*putio.File{},
	}
	if _, err := New(client, 1, root).Reconcile(context.Background()); err == nil {
		t.Fatal("expected unsafe transfer path to fail reconciliation")
	}
}

func putioFile(id int64, name string, parentID int64, directory bool, size int64) *putio.File {
	contentType := "application/octet-stream"
	if directory {
		contentType = "application/x-directory"
	}
	modified := putio.Time{Time: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)}
	return &putio.File{
		ID: id, Name: name, ParentID: parentID, ContentType: contentType, Size: size, UpdatedAt: &modified,
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func objectLabels(objects []Object) []string {
	labels := make([]string, len(objects))
	for i, object := range objects {
		labels[i] = object.Source + ":" + object.Path
	}
	return labels
}
