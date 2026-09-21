package download

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
)

const (
	legacyDriftManifest = `[{"name":"old-root/file.epub","length":4}]`
	nestedDriftManifest = `{"version":1,"transferId":101,"localRoot":"old-root/book","remoteName":"old-root/book","files":[{"name":"old-root/book/file.epub","length":4}]}`
)

func driftWrite(t *testing.T, root, path, contents string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

type driftEntry struct {
	info os.FileInfo
	data string
}

func driftSnapshot(t *testing.T, root string) map[string]driftEntry {
	t.Helper()
	entries := make(map[string]driftEntry)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		entry := driftEntry{info: info}
		if info.Mode()&os.ModeSymlink != 0 {
			entry.data, err = os.Readlink(path)
		} else if info.Mode().IsRegular() {
			var data []byte
			data, err = os.ReadFile(path)
			entry.data = string(data)
		}
		entries[path] = entry
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func driftUnchanged(t *testing.T, root string, before map[string]driftEntry) {
	t.Helper()
	after := driftSnapshot(t, root)
	if len(after) != len(before) {
		t.Fatalf("filesystem entry count changed: %d -> %d", len(before), len(after))
	}
	for path, old := range before {
		current, ok := after[path]
		if !ok || current.data != old.data || current.info.Mode() != old.info.Mode() || !os.SameFile(old.info, current.info) || !current.info.ModTime().Equal(old.info.ModTime()) {
			t.Errorf("filesystem entry changed: %s", path)
		}
	}
}

func TestManifestNameDriftSafety(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
		setup    func(*testing.T, *Manager)
		wantErr  string
	}{
		{name: "valid legacy"},
		{name: "missing root", setup: func(t *testing.T, m *Manager) {
			if err := os.RemoveAll(filepath.Join(m.cfg.TargetDir, "old-root")); err != nil {
				t.Fatal(err)
			}
		}, wantErr: "stat manifest path"},
		{name: "missing child", setup: func(t *testing.T, m *Manager) {
			if err := os.Remove(filepath.Join(m.cfg.TargetDir, "old-root/file.epub")); err != nil {
				t.Fatal(err)
			}
		}, wantErr: "stat manifest path"},
		{name: "malformed", manifest: `{`, wantErr: "parse transfer file state"},
		{name: "empty", manifest: `[]`, wantErr: "empty"},
		{name: "null", manifest: `null`, wantErr: "invalid manifest"},
		{name: "duplicate", manifest: `[{"name":"old-root/file.epub","length":4},{"name":"old-root/file.epub","length":4}]`, wantErr: "duplicate"},
		{name: "multiple roots", manifest: `[{"name":"old-root/file.epub","length":4},{"name":"other-root/book.epub","length":4}]`, wantErr: "share one local root"},
		{name: "ambiguous legacy root", manifest: `[{"name":"old-root/book/file.epub","length":4}]`, wantErr: `ambiguous legacy manifest root below "old-root"`},
		{name: "ambiguous legacy sibling roots", manifest: `[{"name":"old-root/book/file.epub","length":4},{"name":"old-root/art/cover.jpg","length":5}]`, wantErr: `ambiguous legacy manifest root below "old-root"`},
		{name: "traversal", manifest: `[{"name":"../outside/file.epub","length":4}]`, wantErr: "unsafe"},
		{name: "cleaned traversal", manifest: `[{"name":"old-root/book/../file.epub","length":4}]`, wantErr: "unsafe"},
		{name: "absolute", manifest: `[{"name":"/outside/file.epub","length":4}]`, wantErr: "unsafe"},
		{name: "reserved", manifest: `[{"name":".PLUNDRIO-FILES/file.epub","length":4}]`, wantErr: "unsafe"},
		{name: "negative length", manifest: `[{"name":"old-root/file.epub","length":-1}]`, wantErr: "unsafe"},
		{name: "wrong size", manifest: `[{"name":"old-root/file.epub","length":5}]`, wantErr: "expected length"},
		{name: "collision", setup: func(t *testing.T, m *Manager) {
			driftWrite(t, m.cfg.TargetDir, ".plundrio-files/202.json", `[{"name":"old-root/other.epub","length":5}]`)
			driftWrite(t, m.cfg.TargetDir, "old-root/other.epub", "other")
		}, wantErr: "collides with transfer 202"},
		{name: "case collision", setup: func(t *testing.T, m *Manager) {
			driftWrite(t, m.cfg.TargetDir, ".plundrio-files/202.json", `[{"name":"OLD-ROOT/other.epub","length":5}]`)
		}, wantErr: "collides with transfer 202"},
		{name: "category ancestor collision", setup: func(t *testing.T, m *Manager) {
			m.cfg.UseCategoriesTarget = true
			m.SetCategory(202, "old-root")
			driftWrite(t, m.cfg.TargetDir, ".plundrio-files/202.json", `[{"name":"nested/other.epub","length":5}]`)
		}, wantErr: "collides with transfer 202"},
		{name: "wrong embedded ID", manifest: `{"version":1,"transferId":202,"localRoot":"old-root","files":[{"name":"old-root/book/file.epub","length":4}]}`, wantErr: "invalid manifest"},
		{name: "wrong version", manifest: `{"version":2,"transferId":101,"localRoot":"old-root","files":[{"name":"old-root/book/file.epub","length":4}]}`, wantErr: "invalid manifest"},
		{name: "wrong explicit root", manifest: `{"version":1,"transferId":101,"localRoot":"other-root","files":[{"name":"old-root/book/file.epub","length":4}]}`, wantErr: "outside local root"},
		{name: "explicit nested root", manifest: nestedDriftManifest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManagerForTest(t, nil)
			data := tc.manifest
			if data == "" {
				data = legacyDriftManifest
			}
			driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", data)
			driftWrite(t, m.cfg.TargetDir, "old-root/file.epub", "book")
			driftWrite(t, m.cfg.TargetDir, "old-root/book/file.epub", "book")
			if tc.setup != nil {
				tc.setup(t, m)
			}
			before := driftSnapshot(t, m.cfg.TargetDir)
			manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: "new-root"}, ManifestCheckComplete)
			if tc.wantErr == "" {
				if err != nil || manifest.TransferID != 101 || manifest.RemoteName != "new-root" || manifest.LocalRoot == "new-root" || len(manifest.Files) != 1 {
					t.Fatalf("manifest = %+v, err = %v", manifest, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			driftUnchanged(t, m.cfg.TargetDir, before)
		})
	}
}

func TestManifestNameDriftRejectsSymlinks(t *testing.T) {
	for _, tc := range []struct{ manifest, component string }{
		{legacyDriftManifest, "old-root"},
		{legacyDriftManifest, "old-root/file.epub"},
		{legacyDriftManifest, ".plundrio-files/101.json"},
		{legacyDriftManifest, ".plundrio-files"},
		{nestedDriftManifest, "old-root"},
		{nestedDriftManifest, "old-root/book"},
		{nestedDriftManifest, "old-root/book/file.epub"},
	} {
		component := tc.component
		t.Run(fmt.Sprintf("%s/%d", component, len(tc.manifest)), func(t *testing.T) {
			m := newManagerForTest(t, nil)
			external := t.TempDir()
			for _, dir := range []string{m.cfg.TargetDir, external} {
				driftWrite(t, dir, ".plundrio-files/101.json", tc.manifest)
				driftWrite(t, dir, "old-root/file.epub", "book")
				driftWrite(t, dir, "old-root/book/file.epub", "book")
			}
			path := filepath.Join(m.cfg.TargetDir, component)
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(external, component), path); err != nil {
				t.Fatal(err)
			}
			before, outside := driftSnapshot(t, m.cfg.TargetDir), driftSnapshot(t, external)
			for _, check := range []ManifestCheck{ManifestCheckPending, ManifestCheckProcessed, ManifestCheckComplete} {
				if _, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: "new-root"}, check); err == nil || !strings.Contains(err.Error(), "symlink") {
					t.Fatalf("check %d: error = %v", check, err)
				}
			}
			driftUnchanged(t, m.cfg.TargetDir, before)
			driftUnchanged(t, external, outside)
		})
	}
}

func TestManifestNameDriftDoesNotAdoptUnmanagedFiles(t *testing.T) {
	m := newManagerForTest(t, nil)
	driftWrite(t, m.cfg.TargetDir, "new-root/book/file.epub", "book")
	before := driftSnapshot(t, m.cfg.TargetDir)
	manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: "new-root"}, ManifestCheckComplete)
	if err != nil || len(manifest.Files) != 0 || manifest.LocalRoot != "" {
		t.Fatalf("unmanaged files adopted: %+v %v", manifest, err)
	}
	driftUnchanged(t, m.cfg.TargetDir, before)
}

type driftClient struct {
	*fakeClient
	t *testing.T
}

func (c *driftClient) DeleteFile(context.Context, int64) error {
	c.t.Error("unexpected remote file deletion")
	return nil
}
func (c *driftClient) DeleteTransfer(context.Context, int64) error {
	c.t.Error("unexpected remote transfer deletion")
	return nil
}

func TestManifestNameDriftPollAndRestart(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprintf("complete=%t", complete), func(t *testing.T) {
			root := t.TempDir()
			cfg := &config.Config{TargetDir: root, FolderID: testFolderID, WorkerCount: 1}
			currentName := "old-root"
			client := &driftClient{t: t, fakeClient: &fakeClient{
				transfers: func() ([]*putio.Transfer, error) {
					fileID := int64(501)
					if complete {
						fileID = 0
					}
					return []*putio.Transfer{{ID: 101, Hash: "ABC123", Name: currentName, FileID: fileID, SaveParentID: testFolderID, Status: "COMPLETED", PercentDone: 100, Size: 4}}, nil
				},
				files: func(int64) ([]*putio.File, error) {
					return []*putio.File{{ID: 11, Name: "file.epub", Size: 4}}, nil
				},
			}}
			driftWrite(t, root, ".plundrio-files/101.json", legacyDriftManifest)
			contents := "bo"
			if complete {
				contents = "book"
			}
			driftWrite(t, root, "old-root/file.epub", contents)
			before := driftSnapshot(t, root)
			m := New(cfg, client)
			for _, phase := range []string{"initial poll", "renamed poll", "restart"} {
				if phase != "initial poll" {
					currentName = "new-root"
				}
				if phase == "restart" {
					m = New(cfg, client)
				}
				m.processor.checkTransfers()
				m.processorWg.Wait()
				ctx, ok := m.GetTransferContext(101)
				wantState := TransferLifecycleDownloading
				if complete {
					wantState = TransferLifecycleProcessed
				}
				if !ok || ctx.GetState() != wantState || ctx.Name != "old-root" {
					t.Fatalf("%s: context=%+v", phase, ctx)
				}
				transfer := m.GetTransfers()[0]
				check := ManifestCheckPending
				if complete {
					check = ManifestCheckComplete
				}
				manifest, err := m.GetTransferManifest(transfer, check)
				if err != nil || manifest.LocalRoot != "old-root" || manifest.RemoteName != currentName {
					t.Fatalf("%s: manifest=%+v err=%v", phase, manifest, err)
				}
				if !complete && phase != "renamed poll" {
					select {
					case job := <-m.jobs:
						if job.Name != "old-root/file.epub" || job.TransferID != 101 {
							t.Fatalf("wrong download destination: %+v", job)
						}
					default:
						t.Fatal("expected original-root download job")
					}
				}
				driftUnchanged(t, root, before)
			}
		})
	}
}

func TestManifestNameDriftPreservesExplicitRoot(t *testing.T) {
	m := newManagerForTest(t, nil)
	transfer := &putio.Transfer{ID: 101, Name: "old-root/nested", Hash: "ABC123"}
	files := []*putio.File{{ID: 11, Name: "file.epub", Size: 4}}
	if _, err := m.prepareManifest(transfer, files); err != nil {
		t.Fatal(err)
	}
	driftWrite(t, m.cfg.TargetDir, "old-root/nested/file.epub", "book")
	data, err := os.ReadFile(m.transferFiles.path(101))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Version int
		LocalManifest
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Version != 1 || stored.LocalRoot != transfer.Name || stored.TransferID != 101 || stored.RemoteName != transfer.Name {
		t.Fatalf("identity not explicit: %+v", stored)
	}
	before := driftSnapshot(t, m.cfg.TargetDir)
	transfer.Name = "new-root"
	restarted := New(m.cfg, nil)
	local, err := restarted.prepareManifest(transfer, files)
	if err != nil || local.Name != "old-root/nested" {
		t.Fatalf("restart destination = %+v, err=%v", local, err)
	}
	manifest, err := restarted.GetTransferManifest(transfer, ManifestCheckComplete)
	if err != nil || manifest.RemoteName != "new-root" || !reflect.DeepEqual(manifest.Files, stored.Files) {
		t.Fatalf("restart manifest = %+v, err=%v", manifest, err)
	}
	driftUnchanged(t, m.cfg.TargetDir, before)
}

func TestManifestNameDriftRefusesChangedRemoteFiles(t *testing.T) {
	m := newManagerForTest(t, nil)
	driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", legacyDriftManifest)
	driftWrite(t, m.cfg.TargetDir, "old-root/file.epub", "book")
	before := driftSnapshot(t, m.cfg.TargetDir)
	_, err := m.prepareManifest(&putio.Transfer{ID: 101, Name: "new-root"}, []*putio.File{{ID: 11, Name: "other.epub", Size: 4}})
	if err == nil || !strings.Contains(err.Error(), "differs from persisted manifest") {
		t.Fatalf("changed remote listing: %v", err)
	}
	driftUnchanged(t, m.cfg.TargetDir, before)
}

func TestManifestNameDriftLegacyNestedRoot(t *testing.T) {
	for _, name := range []string{"old-root/nested", "new-root"} {
		t.Run(name, func(t *testing.T) {
			m := newManagerForTest(t, nil)
			driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", `[{"name":"old-root/nested/file.epub","length":4},{"name":"old-root/nested/cover.jpg","length":5}]`)
			driftWrite(t, m.cfg.TargetDir, "old-root/nested/file.epub", "book")
			driftWrite(t, m.cfg.TargetDir, "old-root/nested/cover.jpg", "cover")
			before := driftSnapshot(t, m.cfg.TargetDir)
			// Changed listing order must not change the persisted file order.
			local, err := m.prepareManifest(&putio.Transfer{ID: 101, Name: name}, []*putio.File{{ID: 12, Name: "cover.jpg", Size: 5}, {ID: 11, Name: "file.epub", Size: 4}})
			if err != nil || local.Name != "old-root/nested" {
				t.Fatalf("nested legacy root: %+v %v", local, err)
			}
			driftUnchanged(t, m.cfg.TargetDir, before)
		})
	}
}

func TestManifestNameDriftCategoryRestart(t *testing.T) {
	root := t.TempDir()
	driftWrite(t, root, ".plundrio-state.json", `{"101":"books"}`)
	driftWrite(t, root, ".plundrio-files/101.json", legacyDriftManifest)
	driftWrite(t, root, "books/old-root/file.epub", "book")
	before := driftSnapshot(t, root)
	m := New(&config.Config{TargetDir: root, UseCategoriesTarget: true}, &driftClient{t: t, fakeClient: &fakeClient{}})
	m.categories.Load() // The same persistence step performed by Manager.Start.
	m.processor.processTransfer(&putio.Transfer{ID: 101, Name: "new-root", FileID: 0})
	ctx, ok := m.GetTransferContext(101)
	if !ok || ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("category restart failed: %+v", ctx)
	}
	driftUnchanged(t, root, before)
}

func TestManifestNameDriftStoredHash(t *testing.T) {
	m := newManagerForTest(t, nil)
	driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", `{"version":1,"transferId":101,"hash":"ABC123","localRoot":"old-root","remoteName":"old-root","files":[{"name":"old-root/book/file.epub","length":4}]}`)
	driftWrite(t, m.cfg.TargetDir, "old-root/book/file.epub", "book")
	before := driftSnapshot(t, m.cfg.TargetDir)
	for _, hash := range []string{"abc123", "other-hash"} {
		_, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: "new-root", Hash: hash}, ManifestCheckComplete)
		if hash == "abc123" && err != nil {
			t.Fatal(err)
		}
		if hash == "other-hash" && (err == nil || !strings.Contains(err.Error(), "hash does not match")) {
			t.Fatalf("changed hash accepted: %v", err)
		}
	}
	driftUnchanged(t, m.cfg.TargetDir, before)
}

func TestManifestNameDriftInitialDownloadWithoutRoot(t *testing.T) {
	for _, absentTarget := range []bool{false, true} {
		t.Run(fmt.Sprintf("absent-target=%t", absentTarget), func(t *testing.T) {
			root := t.TempDir()
			if absentTarget {
				root = filepath.Join(root, "downloads")
			}
			m := New(&config.Config{TargetDir: root}, nil)
			transfer := &putio.Transfer{ID: 101, Name: "old-root"}
			if _, err := m.prepareManifest(transfer, []*putio.File{{ID: 11, Name: "file.epub", Size: 4}}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.GetTransferManifest(transfer, ManifestCheckPending); err != nil {
				t.Fatalf("pending unchanged-name transfer: %v", err)
			}
			transfer.Name = "new-root"
			if _, err := m.GetTransferManifest(transfer, ManifestCheckPending); err == nil {
				t.Fatal("name drift silently accepted missing local root")
			}
		})
	}
}

// The issue's case: an unchanged transfer ID keeps its original local root
// after the remote name changes, with no local mutation of any kind.
func TestManifestNameDriftPreservesLegacyRoot(t *testing.T) {
	m := newManagerForTest(t, nil)
	driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", `[{"name":"old-root/file.epub","length":4}]`)
	driftWrite(t, m.cfg.TargetDir, "old-root/file.epub", "book")
	before := driftSnapshot(t, m.cfg.TargetDir)
	manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: "new-root"}, ManifestCheckComplete)
	if err != nil || manifest.LocalRoot != "old-root" || manifest.RemoteName != "new-root" || len(manifest.Files) != 1 {
		t.Fatalf("legacy root lost across name drift: %+v %v", manifest, err)
	}
	driftUnchanged(t, m.cfg.TargetDir, before)
}

// A legacy array whose files all sit below one subdirectory records no boundary
// between the transfer root and the directories inside it, so no read may claim
// either the subdirectory or the parent that also holds another season this
// transfer never downloaded. The current remote name, even when it equals one
// of those directories, is not evidence of the boundary.
func TestManifestNameDriftNeverClaimsSiblingAncestor(t *testing.T) {
	for _, name := range []string{"Show/S01", "renamed", "Show"} {
		t.Run(name, func(t *testing.T) {
			m := newManagerForTest(t, nil)
			driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", `[{"name":"Show/S01/ep1.mkv","length":3},{"name":"Show/S01/ep2.mkv","length":3}]`)
			driftWrite(t, m.cfg.TargetDir, "Show/S01/ep1.mkv", "one")
			driftWrite(t, m.cfg.TargetDir, "Show/S01/ep2.mkv", "two")
			driftWrite(t, m.cfg.TargetDir, "Show/S02/ep3.mkv", "thr")
			driftWrite(t, m.cfg.TargetDir, "Show/poster.jpg", "img")
			before := driftSnapshot(t, m.cfg.TargetDir)
			for _, check := range []ManifestCheck{ManifestCheckPending, ManifestCheckProcessed, ManifestCheckComplete} {
				manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: name}, check)
				if err == nil || !strings.Contains(err.Error(), `ambiguous legacy manifest root below "Show"`) {
					t.Fatalf("check %d: manifest = %+v, err = %v", check, manifest, err)
				}
			}
			driftUnchanged(t, m.cfg.TargetDir, before)
		})
	}
}

// An ambiguous record still guards the roots other transfers may claim instead
// of failing every unrelated transfer closed.
func TestManifestNameDriftAmbiguousRecordStillGuardsOthers(t *testing.T) {
	m := newManagerForTest(t, nil)
	driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", `[{"name":"Show/S01/ep1.mkv","length":3}]`)
	driftWrite(t, m.cfg.TargetDir, ".plundrio-files/202.json", `[{"name":"Show/other.mkv","length":3}]`)
	driftWrite(t, m.cfg.TargetDir, ".plundrio-files/303.json", `[{"name":"other-root/file.epub","length":4}]`)
	driftWrite(t, m.cfg.TargetDir, "Show/S01/ep1.mkv", "one")
	driftWrite(t, m.cfg.TargetDir, "Show/other.mkv", "two")
	driftWrite(t, m.cfg.TargetDir, "other-root/file.epub", "book")
	before := driftSnapshot(t, m.cfg.TargetDir)
	if _, err := m.GetTransferManifest(&putio.Transfer{ID: 202, Name: "renamed"}, ManifestCheckComplete); err == nil || !strings.Contains(err.Error(), "collides with transfer 101") {
		t.Fatalf("ambiguous record stopped guarding its claim: %v", err)
	}
	manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 303, Name: "renamed"}, ManifestCheckComplete)
	if err != nil || manifest.LocalRoot != "other-root" {
		t.Fatalf("unrelated transfer failed closed: %+v %v", manifest, err)
	}
	driftUnchanged(t, m.cfg.TargetDir, before)
}

// A record listing several local roots is structurally invalid rather than
// merely ambiguous: it cannot express its own claim, so every transfer that
// would otherwise claim one of those roots must fail closed instead of
// silently taking the root the broken record left unguarded.
func TestManifestMalformedMultipleRootsGuardsEveryRoot(t *testing.T) {
	for _, malformed := range []string{
		`[{"name":"a/first.epub","length":3},{"name":"b/owned.epub","length":3}]`,
		`[{"name":"b/owned.epub","length":3},{"name":"a/first.epub","length":3}]`,
	} {
		t.Run(malformed, func(t *testing.T) {
			m := newManagerForTest(t, nil)
			driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", malformed)
			driftWrite(t, m.cfg.TargetDir, ".plundrio-files/202.json", `[{"name":"b/second.epub","length":3}]`)
			driftWrite(t, m.cfg.TargetDir, "a/first.epub", "one")
			driftWrite(t, m.cfg.TargetDir, "b/owned.epub", "two")
			driftWrite(t, m.cfg.TargetDir, "b/second.epub", "thr")
			before := driftSnapshot(t, m.cfg.TargetDir)
			for _, check := range []ManifestCheck{ManifestCheckPending, ManifestCheckProcessed, ManifestCheckComplete} {
				if _, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: "a"}, check); err == nil || !strings.Contains(err.Error(), "share one local root") {
					t.Fatalf("check %d: malformed record accepted: %v", check, err)
				}
				manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 202, Name: "b"}, check)
				if err == nil || !strings.Contains(err.Error(), "manifest 101") || !strings.Contains(err.Error(), "share one local root") {
					t.Fatalf("check %d: competing claim on %q was lost: %+v %v", check, "b", manifest, err)
				}
			}
			driftUnchanged(t, m.cfg.TargetDir, before)
		})
	}
}

// A transfer without a manifest owns nothing. Its remote name is not ownership
// proof, so it may never resolve onto a root another record already claims.
func TestManifestNameDriftManifestlessTransferClaimsNothing(t *testing.T) {
	m := newManagerForTest(t, nil)
	driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", `[{"name":"Show/ep1.mkv","length":3}]`)
	driftWrite(t, m.cfg.TargetDir, "Show/ep1.mkv", "one")
	driftWrite(t, m.cfg.TargetDir, "Other/loose.mkv", "two")
	before := driftSnapshot(t, m.cfg.TargetDir)
	for _, check := range []ManifestCheck{ManifestCheckPending, ManifestCheckProcessed, ManifestCheckComplete} {
		manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 202, Name: "Show"}, check)
		if err == nil || !strings.Contains(err.Error(), "collides with transfer 101") {
			t.Fatalf("check %d: manifest-less transfer claimed an owned root: %+v %v", check, manifest, err)
		}
		if manifest.LocalRoot != "" {
			t.Fatalf("check %d: refused transfer still reported ownership %q", check, manifest.LocalRoot)
		}
		// An unclaimed name resolves, but still records no ownership.
		manifest, err = m.GetTransferManifest(&putio.Transfer{ID: 202, Name: "Other"}, check)
		if err != nil || manifest.LocalRoot != "" || len(manifest.Files) != 0 {
			t.Fatalf("check %d: unclaimed name failed closed: %+v %v", check, manifest, err)
		}
	}
	driftUnchanged(t, m.cfg.TargetDir, before)
}

// The deletion candidate of a manifest-less transfer lives under the same
// category its removal would delete from, so it must be compared against other
// records in that layout instead of as a bare name at the download root.
func TestManifestNameDriftManifestlessClaimUsesCategory(t *testing.T) {
	m := newManagerForTest(t, nil)
	m.cfg.UseCategoriesTarget = true
	m.SetCategory(101, "tv")
	m.SetCategory(202, "tv")
	m.SetCategory(303, "movies")
	driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", `[{"name":"Show/ep1.mkv","length":3}]`)
	driftWrite(t, m.cfg.TargetDir, "tv/Show/ep1.mkv", "one")
	driftWrite(t, m.cfg.TargetDir, "movies/Show/feature.mkv", "two")
	before := driftSnapshot(t, m.cfg.TargetDir)
	for _, check := range []ManifestCheck{ManifestCheckPending, ManifestCheckProcessed, ManifestCheckComplete} {
		manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 202, Name: "Show"}, check)
		if err == nil || !strings.Contains(err.Error(), "collides with transfer 101") {
			t.Fatalf("check %d: manifest-less claim ignored the category: %+v %v", check, manifest, err)
		}
		if manifest.LocalRoot != "" {
			t.Fatalf("check %d: refused transfer reported ownership %q", check, manifest.LocalRoot)
		}
		// The same name in another category is a different directory.
		if manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 303, Name: "Show"}, check); err != nil || manifest.LocalRoot != "" {
			t.Fatalf("check %d: unrelated category failed closed: %+v %v", check, manifest, err)
		}
	}
	driftUnchanged(t, m.cfg.TargetDir, before)
}

// The category component is part of the validated path, so a category that is
// a symlink is refused before it can be walked into.
func TestManifestNameDriftManifestlessCategorySymlink(t *testing.T) {
	m := newManagerForTest(t, nil)
	m.cfg.UseCategoriesTarget = true
	m.SetCategory(202, "tv")
	driftWrite(t, m.cfg.TargetDir, "outside/Show/ep1.mkv", "one")
	if err := os.Symlink(filepath.Join(m.cfg.TargetDir, "outside"), filepath.Join(m.cfg.TargetDir, "tv")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	before := driftSnapshot(t, m.cfg.TargetDir)
	manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 202, Name: "Show"}, ManifestCheckProcessed)
	if err == nil || !strings.Contains(err.Error(), "symlink in manifest path") {
		t.Fatalf("symlinked category accepted: %+v %v", manifest, err)
	}
	driftUnchanged(t, m.cfg.TargetDir, before)
}

// A remote name that cannot resolve to a directory inside the download root
// establishes no boundary at all, so it is refused instead of reading clean and
// skipping the ownership inventory.
func TestManifestNameDriftManifestlessUnsafeName(t *testing.T) {
	for _, name := range []string{"", ".", "../escape", ".plundrio-files", "/"} {
		t.Run(name, func(t *testing.T) {
			for _, corrupt := range []bool{false, true} {
				m := newManagerForTest(t, nil)
				if corrupt {
					driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", `{`)
				}
				driftWrite(t, m.cfg.TargetDir, "keep/file.epub", "keep")
				before := driftSnapshot(t, m.cfg.TargetDir)
				manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 202, Name: name}, ManifestCheckProcessed)
				if err == nil {
					t.Fatalf("corrupt=%t: unsafe name read clean: %+v", corrupt, manifest)
				}
				if manifest.LocalRoot != "" {
					t.Fatalf("corrupt=%t: refused transfer reported ownership %q", corrupt, manifest.LocalRoot)
				}
				driftUnchanged(t, m.cfg.TargetDir, before)
			}
		})
	}
}

// A removal marker is the only durable record of a released transfer's
// category. When it cannot be read, that transfer's local root is unknown, so
// every ownership answer that depends on it must fail instead of falling back
// to the download root and leaving its payload unguarded.
func TestManifestNameDriftUnreadableRemovalMarkerCategory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		markerID   int64
		marker     string
		wantErr    string
		controlErr string
	}{
		{name: "corrupt owner marker", markerID: 101, marker: `{`, wantErr: "category for manifest 101", controlErr: "category for manifest 101"},
		{name: "non-string owner marker", markerID: 101, marker: `42`, wantErr: "category for manifest 101", controlErr: "category for manifest 101"},
		{name: "readable owner marker", markerID: 101, marker: `"tv"`, wantErr: "collides with transfer 101"},
		{name: "corrupt subject marker", markerID: 202, marker: `{`, wantErr: "category for transfer 202"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManagerForTest(t, nil)
			m.cfg.UseCategoriesTarget = true
			m.SetCategory(101, "tv")
			m.SetCategory(202, "tv")
			m.SetCategory(303, "movies")
			driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", `[{"name":"Show/ep1.mkv","length":3}]`)
			driftWrite(t, m.cfg.TargetDir, fmt.Sprintf(".plundrio-files/%d.removing.json", tc.markerID), tc.marker)
			driftWrite(t, m.cfg.TargetDir, "tv/Show/ep1.mkv", "one")
			driftWrite(t, m.cfg.TargetDir, "movies/Other/feature.mkv", "two")
			before := driftSnapshot(t, m.cfg.TargetDir)
			for _, check := range []ManifestCheck{ManifestCheckPending, ManifestCheckProcessed, ManifestCheckComplete} {
				manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 202, Name: "Show"}, check)
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("check %d: unreadable category did not fail closed: %+v %v", check, manifest, err)
				}
				if manifest.LocalRoot != "" {
					t.Fatalf("check %d: refused transfer reported ownership %q", check, manifest.LocalRoot)
				}
				manifest, err = m.GetTransferManifest(&putio.Transfer{ID: 303, Name: "Other"}, check)
				if tc.controlErr == "" {
					if err != nil || manifest.LocalRoot != "" {
						t.Fatalf("check %d: unrelated transfer failed closed: %+v %v", check, manifest, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tc.controlErr) {
					t.Fatalf("check %d: unrelated transfer ignored the broken record: %+v %v", check, manifest, err)
				}
			}
			driftUnchanged(t, m.cfg.TargetDir, before)
		})
	}
}

// A backslash is an ordinary byte in a POSIX filename. Manifests record the
// name Put.io reported, so the same literal must initialize, reload and delete
// unchanged, exactly as it did before manifests existed.
func TestManifestNameDriftLiteralBackslashName(t *testing.T) {
	const name = `AC\DC - Album`
	const entry = `AC\DC - Album/file.mkv`

	m := newManagerForTest(t, nil)
	files, err := buildTransferFileManifest(&putio.Transfer{ID: 101, Name: name}, []*putio.File{{Name: "file.mkv", Size: 3}})
	if err != nil {
		t.Fatalf("literal name refused during manifest initialization: %v", err)
	}
	if len(files) != 1 || files[0].Name != entry {
		t.Fatalf("manifest = %+v, want single entry %q", files, entry)
	}
	if err := m.transferFiles.Set(101, files); err != nil {
		t.Fatalf("literal name refused during manifest persistence: %v", err)
	}
	driftWrite(t, m.cfg.TargetDir, filepath.Join(name, "file.mkv"), "one")
	driftWrite(t, m.cfg.TargetDir, `Solo\Act/other.mkv`, "two")
	before := driftSnapshot(t, m.cfg.TargetDir)

	for _, check := range []ManifestCheck{ManifestCheckPending, ManifestCheckProcessed, ManifestCheckComplete} {
		manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: "renamed upstream"}, check)
		if err != nil {
			t.Fatalf("check %d: persisted literal name failed to reload: %v", check, err)
		}
		if manifest.LocalRoot != name || len(manifest.Files) != 1 || manifest.Files[0].Name != entry {
			t.Fatalf("check %d: reload rewrote the literal name: %+v", check, manifest)
		}
		// An unclaimed literal-backslash name is still its own deletion candidate.
		if manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 202, Name: `Solo\Act`}, check); err != nil || manifest.LocalRoot != "" {
			t.Fatalf("check %d: manifest-less literal name refused: %+v %v", check, manifest, err)
		}
		// The owned root is still guarded against a same-named transfer.
		if manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 202, Name: name}, check); err == nil || !strings.Contains(err.Error(), "collides with transfer 101") {
			t.Fatalf("check %d: literal name bypassed ownership: %+v %v", check, manifest, err)
		}
	}
	driftUnchanged(t, m.cfg.TargetDir, before)
}

// A NUL byte cannot appear in a POSIX path, so it stays rejected.
func TestManifestNameDriftRejectsNulInManifestEntry(t *testing.T) {
	m := newManagerForTest(t, nil)
	driftWrite(t, m.cfg.TargetDir, ".plundrio-files/101.json", `[{"name":"AC\u0000DC/file.mkv","length":3}]`)
	before := driftSnapshot(t, m.cfg.TargetDir)
	manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: "AC\x00DC"}, ManifestCheckProcessed)
	if err == nil || !strings.Contains(err.Error(), "unsafe manifest entry") {
		t.Fatalf("NUL byte accepted in manifest entry: %+v %v", manifest, err)
	}
	driftUnchanged(t, m.cfg.TargetDir, before)
}
