package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

// Supply the latest mock Put.io poll while using the real persisted file store.
type manifestRPCService struct {
	*download.Manager
	transfers []*putio.Transfer
	contexts  map[int64]*download.TransferContext
}

func (s *manifestRPCService) GetTransfers() []*putio.Transfer { return s.transfers }

func (s *manifestRPCService) GetTransferContext(id int64) (*download.TransferContext, bool) {
	ctx, ok := s.contexts[id]
	return ctx, ok
}

type manifestRPCTorrent struct {
	ID           int64
	Name         string
	HashString   string
	Error        int
	ErrorString  string
	Status       int
	SeedIdleMode int
	Files        []transmissionFile
}

func manifestRPC(t *testing.T, srv *Server, arguments string) []manifestRPCTorrent {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/transmission/rpc", strings.NewReader(
		`{"method":"torrent-get","tag":7,"arguments":`+arguments+`}`))
	req.Header.Set("X-Transmission-Session-Id", "123")
	response := httptest.NewRecorder()
	srv.handleRPC(response, req)
	var decoded struct {
		Result    string
		Tag       int
		Arguments struct{ Torrents []manifestRPCTorrent }
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || decoded.Result != "success" || decoded.Tag != 7 {
		t.Fatalf("files-inclusive RPC failed: HTTP %d %s", response.Code, response.Body.String())
	}
	return decoded.Arguments.Torrents
}

func writeManifestFixture(t *testing.T, root, relative string, data []byte) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// Run with: go test ./internal/server -run '^TestManifestNameDriftRPC$' -count=1 -v
func TestManifestNameDriftRPC(t *testing.T) {
	root := t.TempDir()
	const manifest = `[{"name":"old-root/book/file.epub","length":4}]`
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(manifest))
	writeManifestFixture(t, root, "old-root/book/file.epub", []byte("book"))
	before, err := os.Stat(filepath.Join(root, "old-root/book/file.epub"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{TargetDir: root}
	for _, name := range []string{"old-root", "new-root"} {
		t.Run(name, func(t *testing.T) {
			// The legacy array records no root, so a drifted name leaves only
			// the deepest directory its entries share as proven ownership.
			wantName := "old-root"
			if name != "old-root" {
				wantName = "old-root/book"
			}
			// A fresh manager exercises the on-disk legacy format, not cached state.
			service := &manifestRPCService{Manager: download.New(cfg, nil), transfers: []*putio.Transfer{
				{ID: 101, Hash: "ABC123", Name: name, Status: "COMPLETED", PercentDone: 100, Size: 4},
			}}
			srv := &Server{cfg: cfg, dlService: service}
			for _, selector := range []string{`101`, `"abc123"`} {
				t.Run(selector, func(t *testing.T) {
					// This masks the bug: fields without files do not validate ownership.
					ids := manifestRPC(t, srv, fmt.Sprintf(`{"ids":[%s],"fields":["id","hashString"]}`, selector))
					if len(ids) != 1 || ids[0].ID != 101 {
						t.Fatalf("ID-only control = %+v", ids)
					}
					got := manifestRPC(t, srv, fmt.Sprintf(`{"ids":[%s],"fields":["id","name","hashString","files","error","errorString"]}`, selector))
					want := []transmissionFile{{Name: "old-root/book/file.epub", Length: 4}}
					// The name stays the locally owned root so the importer's
					// downloadDir/name output path keeps resolving.
					if len(got) != 1 || got[0].ID != 101 || got[0].Name != wantName || got[0].HashString != "ABC123" || got[0].Error != 0 || !reflect.DeepEqual(got[0].Files, want) {
						t.Fatalf("files-inclusive response = %+v, want unchanged old-root files %+v", got, want)
					}
					// downloadDir/name must still contain every reported file.
					if !strings.HasPrefix(want[0].Name, got[0].Name+"/") {
						t.Fatalf("reported name %q does not contain %q", got[0].Name, want[0].Name)
					}
				})
			}
		})
	}
	after, err := os.Stat(filepath.Join(root, "old-root/book/file.epub"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("payload moved or replaced: %v", err)
	}
	payload, err := os.ReadFile(filepath.Join(root, "old-root/book/file.epub"))
	if err != nil || string(payload) != "book" || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("payload contents or metadata changed: %q, %v", payload, err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".plundrio-files/101.json"))
	if err != nil || string(data) != manifest {
		t.Fatalf("manifest changed: %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "new-root")); !os.IsNotExist(err) {
		t.Fatalf("remote name created a local root: %v", err)
	}
}

func TestManifestNameDriftMixedRPC(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"old-root/file.epub","length":4}]`))
	writeManifestFixture(t, root, ".plundrio-files/202.json", []byte(`[{"name":"new-root/other.epub","length":5}]`))
	writeManifestFixture(t, root, ".plundrio-files/303.json", []byte(`[{"name":"missing-root/file.epub","length":4}]`))
	writeManifestFixture(t, root, "old-root/file.epub", []byte("book"))
	writeManifestFixture(t, root, "new-root/other.epub", []byte("other"))
	cfg := &config.Config{TargetDir: root}
	service := &manifestRPCService{Manager: download.New(cfg, nil), transfers: []*putio.Transfer{
		{ID: 101, Hash: "ABC123", Name: "new-root", Status: "COMPLETED", PercentDone: 100},
		// Equal remote names do not imply equal ownership: these roots are disjoint.
		{ID: 202, Hash: "DEF456", Name: "new-root", Status: "COMPLETED", PercentDone: 100},
		{ID: 303, Hash: "BAD789", Name: "bad-new", Status: "COMPLETED", PercentDone: 100},
	}}
	srv := &Server{cfg: cfg, dlService: service}
	for _, ids := range []string{``, `"ids":[101,"def456",303],`} {
		torrents := manifestRPC(t, srv, `{`+ids+`"fields":["id","name","files","error","errorString","status","seedIdleMode"]}`)
		if len(torrents) != 3 {
			t.Fatalf("lost unrelated transfers: %+v", torrents)
		}
		for i, want := range []string{"old-root/file.epub", "new-root/other.epub"} {
			if torrents[i].Error != 0 || len(torrents[i].Files) != 1 || torrents[i].Files[0].Name != want {
				t.Errorf("unaffected transfer %d: %+v", i, torrents[i])
			}
		}
		bad := torrents[2]
		if bad.ID != 303 || bad.Error != trErrorLocal || !strings.Contains(bad.ErrorString, "missing-root") || bad.Files == nil || len(bad.Files) != 0 || bad.Status != trStatusStopped || bad.SeedIdleMode != transmissionLimitModeUnlimited {
			t.Errorf("unsafe transfer did not retain actionable per-transfer failure: %+v", bad)
		}
	}
	for _, path := range []string{"old-root/file.epub", "new-root/other.epub", ".plundrio-files/101.json", ".plundrio-files/202.json", ".plundrio-files/303.json"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("fixture disappeared: %s: %v", path, err)
		}
	}
}

func TestManifestNameDriftMalformedRPC(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`{`))
	cfg := &config.Config{TargetDir: root}
	service := &manifestRPCService{Manager: download.New(cfg, nil), transfers: []*putio.Transfer{{ID: 101, Name: "new-root"}}}
	torrents := manifestRPC(t, &Server{cfg: cfg, dlService: service}, `{"fields":["id","files","error","errorString"]}`)
	if len(torrents) != 1 || torrents[0].Error != trErrorLocal || !strings.Contains(torrents[0].ErrorString, "parse transfer file state") || torrents[0].Files == nil || len(torrents[0].Files) != 0 {
		t.Fatalf("corrupt state was hidden as missing: %+v", torrents)
	}
}

// torrent-remove must delete the local root the manifest owns, not whatever
// the remote transfer is currently called.
func TestManifestNameDriftRemoveDeletesOwnedRoot(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"old-root/file.epub","length":4}]`))
	writeManifestFixture(t, root, ".plundrio-files/202.json", []byte(`[{"name":"new-root/other.epub","length":5}]`))
	writeManifestFixture(t, root, "old-root/file.epub", []byte("book"))
	writeManifestFixture(t, root, "new-root/other.epub", []byte("other"))

	cfg := &config.Config{TargetDir: root}
	transfers := []*putio.Transfer{
		{ID: 101, Hash: "ABC123", Name: "new-root", PercentDone: 100},
		{ID: 202, Hash: "DEF456", Name: "new-root", PercentDone: 100},
	}
	client := &torrentAddClient{transfers: transfers}
	service := &manifestRPCService{Manager: download.New(cfg, nil), transfers: transfers}
	srv := &Server{cfg: cfg, client: client, dlService: service}

	if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "old-root")); !os.IsNotExist(err) {
		t.Fatalf("owned local root was not deleted: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "new-root/other.epub")); err != nil || string(data) != "other" {
		t.Fatalf("removal destroyed another transfer's data: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".plundrio-files/202.json")); err != nil {
		t.Fatalf("removal destroyed another transfer's manifest: %v", err)
	}
}

// A legacy manifest whose files all sit in one subdirectory must not delete the
// parent directory, which holds unmanaged data this transfer never downloaded.
func TestManifestNameDriftRemoveKeepsUnmanagedSiblings(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"Show/S01/ep1.mkv","length":3},{"name":"Show/S01/ep2.mkv","length":3}]`))
	writeManifestFixture(t, root, "Show/S01/ep1.mkv", []byte("one"))
	writeManifestFixture(t, root, "Show/S01/ep2.mkv", []byte("two"))
	writeManifestFixture(t, root, "Show/S02/ep3.mkv", []byte("thr"))
	writeManifestFixture(t, root, "Show/poster.jpg", []byte("art"))

	cfg := &config.Config{TargetDir: root}
	transfers := []*putio.Transfer{{ID: 101, Hash: "ABC123", Name: "Show renamed", PercentDone: 100}}
	client := &torrentAddClient{transfers: transfers}
	service := &manifestRPCService{Manager: download.New(cfg, nil), transfers: transfers}
	srv := &Server{cfg: cfg, client: client, dlService: service}

	if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Show/S01")); !os.IsNotExist(err) {
		t.Fatalf("owned season was not deleted: %v", err)
	}
	for path, want := range map[string]string{"Show/S02/ep3.mkv": "thr", "Show/poster.jpg": "art"} {
		if data, err := os.ReadFile(filepath.Join(root, path)); err != nil || string(data) != want {
			t.Fatalf("removal destroyed unmanaged sibling %q: %q %v", path, data, err)
		}
	}
}

// Ownership is resolved before anything is mutated: a transfer whose local root
// cannot be established keeps its remote record, its local state and its files.
func TestTorrentRemoveRefusesBeforeAnyMutation(t *testing.T) {
	root := t.TempDir()
	const corrupt = `[{"name":"../escape/file.epub","length":4}]`
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(corrupt))
	writeManifestFixture(t, root, "books/old-root/file.epub", []byte("book"))

	cfg := &config.Config{TargetDir: root, UseCategoriesTarget: true}
	manager := download.New(cfg, nil)
	manager.SetCategory(101, "books")
	transfers := []*putio.Transfer{{ID: 101, Hash: "ABC123", Name: "new-root", FileID: 501, Status: "DOWNLOADING", PercentDone: 100}}
	client := &torrentAddClient{transfers: transfers}
	srv := &Server{cfg: cfg, client: client, dlService: &manifestRPCService{Manager: manager, transfers: transfers}}

	_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`))
	if err == nil || !strings.Contains(err.Error(), "establish local ownership for transfer 101; nothing was removed") {
		t.Fatalf("unresolved ownership did not refuse removal: %v", err)
	}
	if len(client.deleted) != 0 || len(client.deletedFiles) != 0 {
		t.Fatalf("refused removal still mutated Put.io: transfers=%v files=%v", client.deleted, client.deletedFiles)
	}
	if got := manager.GetCategory(101); got != "books" {
		t.Fatalf("category = %q, want it retained", got)
	}
	if manager.RemovalPending(101) {
		t.Fatal("refused removal published a removal marker")
	}
	entries, err := os.ReadDir(filepath.Join(root, ".plundrio-files"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "101.json" {
		t.Fatalf("local ownership state changed: %+v %v", entries, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, ".plundrio-files/101.json")); err != nil || string(data) != corrupt {
		t.Fatalf("manifest rewritten: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "books/old-root/file.epub")); err != nil || string(data) != "book" {
		t.Fatalf("local payload changed: %q %v", data, err)
	}
	// The transfer stays visible, so the operator can retry the same request.
	if torrents := manifestRPC(t, srv, `{"ids":[101],"fields":["id","name"]}`); len(torrents) != 1 || torrents[0].ID != 101 {
		t.Fatalf("refused removal dropped the transfer record: %+v", torrents)
	}
}
