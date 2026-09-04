package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
)

func TestHandleTorrentGetUsesTransmissionErrorFields(t *testing.T) {
	tests := []struct {
		name        string
		errorString string
		wantError   int
	}{
		{name: "healthy", wantError: 0},
		{
			name:        "sustained zero progress",
			errorString: "Put.io transfer stalled: no byte progress for 6h0m0s; inspect the transfer in Put.io",
			wantError:   3,
		},
		{
			name:        "Put.io error",
			errorString: "source unavailable",
			wantError:   3,
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
			service := &torrentAddDownloadService{
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
			if got := torrents[0]["errorString"]; got != tt.errorString {
				t.Errorf("errorString = %#v, want %q", got, tt.errorString)
			}
		})
	}
}
