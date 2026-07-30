package download

import (
	"context"
	"time"

	grab "github.com/cavaliergopher/grab/v3"
	"github.com/elsbrock/plundrio/internal/log"
)

// trackDownloadProgress reports grab's progress until done is closed, the
// context is cancelled, or the download stalls past DownloadStallTimeout.
//
// It runs on the caller's goroutine — callers start it with `go` and wait for
// it to return before touching the byte accounting, so no progress update can
// land after the download has been settled.
//
// cancelDownload aborts the request when a stall is detected.
func (m *Manager) trackDownloadProgress(
	ctx context.Context,
	state *DownloadState,
	resp *grab.Response,
	done <-chan struct{},
	progressTicker *time.Ticker,
	cancelDownload context.CancelFunc,
) {
	log.Info("download").
		Str("file_name", state.Name).
		Float64("size_mb", float64(resp.Size())/1024/1024).
		Msg("Starting download")

	// Get transfer context to update downloaded bytes
	transferCtx, exists := m.coordinator.GetTransferContext(state.TransferID)
	if !exists {
		log.Error("download").
			Str("file_name", state.Name).
			Int64("transfer_id", state.TransferID).
			Msg("Transfer context not found during download")
	}

	// Tracks when the byte count last advanced, for stall detection.
	lastAdvance := time.Now()

	for {
		select {
		case <-progressTicker.C:
			totalSize := resp.Size()
			if totalSize <= 0 {
				continue
			}

			state.mu.Lock()
			bytesComplete := resp.BytesComplete()
			bytesDelta := bytesComplete - state.downloaded
			state.downloaded = bytesComplete
			state.Progress = resp.Progress()

			// Calculate ETA based on current download rate
			elapsed := time.Since(state.StartTime).Seconds()
			if elapsed > 0 {
				speed := float64(state.downloaded) / elapsed
				remaining := float64(totalSize - state.downloaded)
				if speed > 0 {
					etaSeconds := remaining / speed
					state.ETA = time.Now().Add(time.Duration(etaSeconds) * time.Second)
				}
			}

			downloadedMB := float64(state.downloaded) / 1024 / 1024
			totalMB := float64(totalSize) / 1024 / 1024
			progress := state.Progress * 100
			speedMBps := downloadedMB / elapsed
			eta := time.Until(state.ETA).Round(time.Second)
			state.LastProgress = time.Now()
			state.mu.Unlock()

			// Update transfer context with downloaded bytes if it exists
			if exists && bytesDelta > 0 {
				transferCtx.AddDownloadedBytes(bytesDelta)
				transferCtx.SetLocalProgress(speedMBps*1024*1024, state.ETA)

				downloadedSize, transferTotal, _, _ := transferCtx.GetProgress()

				log.Debug("download").
					Str("file_name", state.Name).
					Int64("transfer_id", state.TransferID).
					Int64("bytes_delta", bytesDelta).
					Int64("transfer_downloaded", downloadedSize).
					Int64("transfer_total", transferTotal).
					Msg("Updated transfer downloaded bytes")
			}

			log.Info("download").
				Str("file_name", state.Name).
				Float64("progress_percent", progress).
				Float64("downloaded_mb", downloadedMB).
				Float64("total_mb", totalMB).
				Float64("speed_mbps", speedMBps).
				Str("eta", eta.String()).
				Msg("Download progress")

			// Stall detection: grab has no timeout of its own, so without this
			// a wedged connection would pin this worker until process exit.
			if bytesDelta > 0 {
				lastAdvance = time.Now()
				continue
			}
			if stalled := time.Since(lastAdvance); stalled >= m.dlConfig.DownloadStallTimeout {
				log.Warn("download").
					Str("file_name", state.Name).
					Dur("stalled_for", stalled).
					Msg("Download stalled, cancelling")
				cancelDownload()
				return
			}
		case <-ctx.Done():
			log.Info("download").
				Str("file_name", state.Name).
				Msg("Download cancelled")
			return
		case <-done:
			return
		}
	}
}
