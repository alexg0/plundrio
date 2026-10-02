package server

import (
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
}

func (s *manifestRPCService) GetTransfers() []*putio.Transfer { return s.transfers }

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
	const manifest = `[{"name":"old-root/file.epub","length":4}]`
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(manifest))
	writeManifestFixture(t, root, "old-root/file.epub", []byte("book"))
	before, err := os.Stat(filepath.Join(root, "old-root/file.epub"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{TargetDir: root}
	for _, name := range []string{"old-root", "new-root"} {
		t.Run(name, func(t *testing.T) {
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
					want := []transmissionFile{{Name: "old-root/file.epub", Length: 4}}
					if len(got) != 1 || got[0].ID != 101 || got[0].Name != "old-root" || got[0].HashString != "ABC123" || got[0].Error != 0 || !reflect.DeepEqual(got[0].Files, want) {
						t.Fatalf("files-inclusive response = %+v, want unchanged old-root files %+v", got, want)
					}
				})
			}
		})
	}
	after, err := os.Stat(filepath.Join(root, "old-root/file.epub"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("payload moved or replaced: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".plundrio-files/101.json"))
	if err != nil || string(data) != manifest {
		t.Fatalf("manifest changed: %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "new-root")); !os.IsNotExist(err) {
		t.Fatalf("remote name created a local root: %v", err)
	}
}
