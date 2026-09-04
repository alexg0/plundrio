package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

type torrentGetDownloadService struct {
	transfers []*putio.Transfer
}

func (s *torrentGetDownloadService) GetTransfers() []*putio.Transfer {
	return s.transfers
}

func (s *torrentGetDownloadService) GetTransferContext(int64) (*download.TransferContext, bool) {
	return nil, false
}

func (s *torrentGetDownloadService) SetCategory(int64, string) {}
func (s *torrentGetDownloadService) GetCategory(int64) string  { return "" }
func (s *torrentGetDownloadService) RemoveCategory(int64)      {}
func (s *torrentGetDownloadService) RemoveTransfer(int64)      {}

func TestHandleTorrentGetUsesTransmissionErrorFields(t *testing.T) {
	tests := []struct {
		name          string
		errorString   string
		wantError     int
		wantErrorText string
	}{
		{name: "healthy", wantError: 0},
		{
			name:          "sustained zero progress",
			errorString:   "Put.io transfer stalled: no byte progress for 6h0m0s; inspect the transfer in Put.io",
			wantError:     3,
			wantErrorText: "Put.io transfer stalled: no byte progress for 6h0m0s; inspect the transfer in Put.io",
		},
		{
			name:          "Put.io error",
			errorString:   "source unavailable",
			wantError:     3,
			wantErrorText: "source unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transfer := &putio.Transfer{
				ID:           42,
				Hash:         "hash",
				Name:         "example",
				Status:       "DOWNLOADING",
				ErrorMessage: tt.errorString,
			}
			service := &torrentGetDownloadService{
				transfers: []*putio.Transfer{transfer},
			}
			srv := &Server{
				cfg:       &config.Config{TargetDir: t.TempDir()},
				dlService: service,
			}

			result, err := srv.handleTorrentGet(context.Background(), json.RawMessage(`{"fields":["error","errorString"]}`))
			if err != nil {
				t.Fatalf("handleTorrentGet() error = %v", err)
			}
			torrents := result.(map[string]interface{})["torrents"].([]map[string]interface{})
			if len(torrents) != 1 {
				t.Fatalf("torrent count = %d, want 1", len(torrents))
			}
			if got := torrents[0]["error"]; got != tt.wantError {
				t.Errorf("error = %#v, want %d", got, tt.wantError)
			}
			if got := torrents[0]["errorString"]; got != tt.wantErrorText {
				t.Errorf("errorString = %#v, want %q", got, tt.wantErrorText)
			}
		})
	}
}
