package download

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/log"
)

// transfersByStatus groups put.io transfers by their status string
// (e.g. "COMPLETED", "SEEDING", "ERROR").
type transfersByStatus map[string][]*putio.Transfer

// TransferProcessor handles the processing of Put.io transfers
type TransferProcessor struct {
	manager   *Manager
	folderID  int64
	targetDir string

	// mu guards latest, which is published by the monitor goroutine on every
	// tick and read by RPC handler goroutines via GetTransfers.
	mu     sync.RWMutex
	latest []*putio.Transfer

	retryAttempts     sync.Map // map[int64]int - retry attempts for errored put.io transfers
	reprocessAttempts sync.Map // map[int64]int - local reprocess attempts for failed transfers
}

// GetTransfers returns the transfers seen on the most recent poll of the
// managed folders. Safe to call from any goroutine.
//
// The returned slice is a fresh copy, but the *putio.Transfer values are
// shared. That is safe because each poll builds new values and never mutates
// them afterwards.
func (p *TransferProcessor) GetTransfers() []*putio.Transfer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.latest) == 0 {
		return nil
	}
	out := make([]*putio.Transfer, len(p.latest))
	copy(out, p.latest)
	return out
}

// setTransfers publishes the transfers from the latest poll.
func (p *TransferProcessor) setTransfers(transfers []*putio.Transfer) {
	p.mu.Lock()
	p.latest = transfers
	p.mu.Unlock()
}

// newTransferProcessor creates a new transfer processor
func newTransferProcessor(m *Manager) *TransferProcessor {
	return &TransferProcessor{
		manager:   m,
		folderID:  m.cfg.FolderID,
		targetDir: m.cfg.TargetDir,
	}
}

// monitorTransfers periodically checks for completed transfers
func (m *Manager) monitorTransfers() {
	log.Debug("transfers").Msg("Starting transfer monitor")

	log.Debug("transfers").
		Int64("folder_id", m.processor.folderID).
		Str("target_dir", m.processor.targetDir).
		Msg("Transfer processor initialized")

	// Initial check
	m.processor.checkTransfers()

	ticker := time.NewTicker(m.dlConfig.TransferCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			log.Debug("transfers").Msg("Transfer monitor stopping")
			return
		case <-ticker.C:
			m.processor.checkTransfers()
		}
	}
}

// checkTransfers looks for completed or seeding transfers and processes them
func (p *TransferProcessor) checkTransfers() {
	log.Debug("transfers").Msg("Checking transfers")

	transfers, err := p.manager.client.GetTransfers(p.manager.Context())
	if err != nil {
		log.Error("transfers").Err(err).Msg("Failed to get transfers")
		return
	}

	log.Debug("transfers").
		Int("api_transfers_count", len(transfers)).
		Msg("Retrieved transfers from API")

	// Determine which put.io folders we consider "ours". This is the configured
	// folder plus, when put.io categorization is enabled, its category
	// subfolders.
	managed := p.managedFolders()

	// Keep only transfers in a managed folder, then group by status.
	inFolder := make([]*putio.Transfer, 0, len(transfers))
	byStatus := make(transfersByStatus)
	for _, t := range transfers {
		if _, ok := managed[t.SaveParentID]; !ok {
			log.Debug("transfers").
				Int64("transfer_id", t.ID).
				Int64("parent_id", t.SaveParentID).
				Int64("target_folder", p.folderID).
				Msg("Skipping transfer from different folder")
			continue
		}
		inFolder = append(inFolder, t)
		byStatus[t.Status] = append(byStatus[t.Status], t)
	}

	// Publish for RPC handlers before doing any slow work.
	p.setTransfers(inFolder)

	p.logTransferSummary(byStatus, inFolder)

	// Process transfers by status
	p.processReadyTransfers(byStatus)
	p.processErroredTransfers(byStatus)

	// Check for transfers that are in "Completed" state but haven't been fully cleaned up
	p.finalizeCompletedTransfers()
}

// managedFolders returns the set of put.io folder IDs whose transfers this
// processor should handle. It always includes the configured folder. When
// put.io categorization is enabled, it also includes the configured folder's
// direct subfolders (the category folders such as "tv" or "movies").
//
// Only one level of category subfolders is discovered; deeply-nested put.io
// categories are not supported by the monitor.
func (p *TransferProcessor) managedFolders() map[int64]struct{} {
	managed := map[int64]struct{}{p.folderID: {}}

	if !p.manager.cfg.UseCategoriesPutio {
		return managed
	}

	files, err := p.manager.client.GetFiles(p.manager.Context(), p.folderID)
	if err != nil {
		log.Warn("transfers").
			Int64("folder_id", p.folderID).
			Err(err).
			Msg("Failed to list category subfolders; monitoring configured folder only")
		return managed
	}

	for _, f := range files {
		if f.IsDir() {
			managed[f.ID] = struct{}{}
		}
	}
	return managed
}

// logTransferSummary logs counts of transfers in each status and detailed information for all transfers
func (p *TransferProcessor) logTransferSummary(byStatus transfersByStatus, all []*putio.Transfer) {
	log.Info("transfers").
		Int("queued", len(byStatus["IN_QUEUE"])).
		Int("waiting", len(byStatus["WAITING"])).
		Int("preparing", len(byStatus["PREPARING"])).
		Int("downloading", len(byStatus["DOWNLOADING"])).
		Int("completing", len(byStatus["COMPLETING"])).
		Int("seeding", len(byStatus["SEEDING"])).
		Int("completed", len(byStatus["COMPLETED"])).
		Int("error", len(byStatus["ERROR"])).
		Msg("Transfer status summary")

	// Log detailed information for all transfers
	p.logAllTransfersDetails(all)
}

// logAllTransfersDetails logs detailed information for all transfers
func (p *TransferProcessor) logAllTransfersDetails(allTransfers []*putio.Transfer) {
	if len(allTransfers) == 0 {
		log.Debug("transfers").Msg("No transfers found for detailed logging")
		return
	}

	for _, t := range allTransfers {
		// Create a logger with common fields for all transfers
		transferLogger := log.Debug("transfers").
			Int64("id", t.ID).
			Str("name", t.Name).
			Str("status", t.Status).
			Int64("save_parent_id", t.SaveParentID).
			Int64("file_id", t.FileID).
			Int("size", t.Size).
			Str("type", t.Type).
			Str("status_message", t.StatusMessage).
			Int("availability", t.Availability).
			Bool("is_private", t.IsPrivate)

		// Add peer information if available
		if t.PeersConnected > 0 || t.PeersSendingToUs > 0 || t.PeersGettingFromUs > 0 {
			transferLogger = transferLogger.
				Int("peers_connected", t.PeersConnected).
				Int("peers_sending_to_us", t.PeersSendingToUs).
				Int("peers_getting_from_us", t.PeersGettingFromUs)
		}

		// Add speed information if available
		if t.DownloadSpeed > 0 || t.UploadSpeed > 0 {
			transferLogger = transferLogger.
				Int("download_speed", t.DownloadSpeed).
				Int("upload_speed", t.UploadSpeed).
				Int64("downloaded", t.Downloaded).
				Int64("uploaded", t.Uploaded)
		}

		// Add progress information if available
		if t.PercentDone > 0 && t.PercentDone < 100 {
			transferLogger = transferLogger.
				Int("percent_done", t.PercentDone).
				Int64("estimated_time", t.EstimatedTime)
		}

		// Add seeding information if available
		if t.SecondsSeeding > 0 {
			transferLogger = transferLogger.Int("seconds_seeding", t.SecondsSeeding)
		}

		// Add tracker information if available
		if t.Trackers != "" {
			transferLogger = transferLogger.Str("trackers", t.Trackers)
		}
		if t.TrackerMessage != "" {
			transferLogger = transferLogger.Str("tracker_message", t.TrackerMessage)
		}

		// Add torrent information if available
		if t.MagnetURI != "" {
			transferLogger = transferLogger.Str("magnet_uri", t.MagnetURI)
		}
		if t.TorrentLink != "" {
			transferLogger = transferLogger.Str("torrent_link", t.TorrentLink)
		}
		if t.CreatedTorrent {
			transferLogger = transferLogger.Bool("created_torrent", t.CreatedTorrent)
		}

		// Add error information if available
		if t.ErrorMessage != "" {
			transferLogger = transferLogger.Str("error_message", t.ErrorMessage)
		}

		// Add timestamps if available
		if t.CreatedAt != nil {
			transferLogger = transferLogger.Interface("created_at", t.CreatedAt)
		}
		if t.FinishedAt != nil {
			transferLogger = transferLogger.Interface("finished_at", t.FinishedAt)
		}

		// Add other miscellaneous fields
		if t.ClientIP != "" {
			transferLogger = transferLogger.Str("client_ip", t.ClientIP)
		}
		if t.CallbackURL != "" {
			transferLogger = transferLogger.Str("callback_url", t.CallbackURL)
		}
		if t.Extract {
			transferLogger = transferLogger.Bool("extract", t.Extract)
		}
		if t.DownloadID != 0 {
			transferLogger = transferLogger.Int64("download_id", t.DownloadID)
		}
		if t.SubscriptionID != 0 {
			transferLogger = transferLogger.Int("subscription_id", t.SubscriptionID)
		}

		// Whether we already downloaded and cleaned up this transfer locally.
		// The coordinator is the single source of truth for that.
		processed := false
		if ctx, ok := p.manager.coordinator.GetTransferContext(t.ID); ok {
			processed = ctx.GetState() == TransferLifecycleProcessed
		}
		transferLogger = transferLogger.Bool("processed_locally", processed)

		// Log the transfer details with a message that includes the status
		transferLogger.Msgf("Transfer details (%s)", t.Status)
	}
}

// processReadyTransfers handles completed and seeding transfers
func (p *TransferProcessor) processReadyTransfers(byStatus transfersByStatus) {
	completed := byStatus["COMPLETED"]
	readyTransfers := make([]*putio.Transfer, 0, len(completed)+len(byStatus["SEEDING"]))
	readyTransfers = append(readyTransfers, completed...)
	readyTransfers = append(readyTransfers, byStatus["SEEDING"]...)
	now := time.Now()

	for _, transfer := range readyTransfers {
		select {
		case <-p.manager.stopChan:
			log.Debug("transfers").Msg("Stopping transfer processing")
			return
		default:
			if p.isTransferBeingProcessed(transfer.ID) {
				continue
			}
			if !CanStartDownloadNow(p.manager.cfg.DownloadStartWindow, now) {
				log.Debug("transfers").
					Int64("transfer_id", transfer.ID).
					Str("transfer_name", transfer.Name).
					Str("window_start", p.manager.cfg.DownloadStartWindow.Start).
					Str("window_end", p.manager.cfg.DownloadStartWindow.End).
					Msg("Skipping local download start because current time is outside the configured window")
				continue
			}
			p.startTransferProcessing(transfer)
		}
	}
}

// isTransferBeingProcessed checks if a transfer is already being handled
func (p *TransferProcessor) isTransferBeingProcessed(transferID int64) bool {
	if _, exists := p.manager.coordinator.GetTransferContext(transferID); exists {
		log.Debug("transfers").
			Int64("transfer_id", transferID).
			Msg("Transfer already being processed")
		return true
	}
	return false
}

// startTransferProcessing begins processing a transfer
func (p *TransferProcessor) startTransferProcessing(transfer *putio.Transfer) {
	log.Info("transfers").
		Str("name", transfer.Name).
		Str("status", transfer.Status).
		Int64("id", transfer.ID).
		Msg("Found ready transfer")

	p.manager.workerWg.Add(1)
	transferCopy := *transfer
	go func() {
		p.processTransfer(&transferCopy)
	}()
}

// processTransfer handles downloading of a completed or seeding transfer
func (p *TransferProcessor) processTransfer(transfer *putio.Transfer) {
	defer p.manager.workerWg.Done()

	log.Debug("transfers").
		Str("name", transfer.Name).
		Int64("id", transfer.ID).
		Int64("file_id", transfer.FileID).
		Msg("Processing transfer")

	files, err := p.manager.client.GetAllTransferFiles(p.manager.Context(), transfer.FileID)
	if err != nil {
		p.handleTransferError(transfer, err)
		return
	}

	if len(files) == 0 {
		err := NewNoFilesFoundError(transfer.ID)
		p.manager.coordinator.FailTransfer(transfer.ID, err)
		return
	}

	// Initialize transfer with total number of files
	if !p.initializeTransfer(transfer, len(files)) {
		return
	}

	// Queue files that need downloading
	filesToDownload := p.queueTransferFiles(transfer, files)

	// If no files need downloading (all exist), complete the transfer
	if filesToDownload == 0 {
		log.Info("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Msg("All files already exist, completing transfer")
		p.manager.coordinator.CompleteTransfer(transfer.ID)
		return
	}
}

// handleTransferError processes transfer errors appropriately
func (p *TransferProcessor) handleTransferError(transfer *putio.Transfer, err error) {
	if isPutioNotFound(err) {
		log.Debug("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Msg("Files no longer exist on Put.io, cleaning up")

		// Initialize transfer context before cleanup
		p.initializeTransfer(transfer, 0)
		p.manager.cleanupTransfer(transfer.ID)
		return
	}

	log.Error("transfers").
		Str("name", transfer.Name).
		Int64("id", transfer.ID).
		Err(err).
		Msg("Failed to get transfer files")
}

func isPutioNotFound(err error) bool {
	var putioErr *putio.ErrorResponse
	return errors.As(err, &putioErr) && putioErr.Type == "NotFound"
}

// queueTransferFiles processes files in a transfer and queues them for download
func (p *TransferProcessor) queueTransferFiles(transfer *putio.Transfer, files []*putio.File) int {
	filesToDownload := 0

	// Get the transfer context to update total size
	ctx, exists := p.manager.coordinator.GetTransferContext(transfer.ID)
	if !exists {
		log.Error("transfers").
			Int64("transfer_id", transfer.ID).
			Msg("Transfer context not found when queueing files")
		return 0
	}

	// Calculate total size of all files
	var totalSize int64
	for _, file := range files {
		totalSize += file.Size
	}

	// Update the transfer context with total size
	ctx.SetTotalSize(totalSize)

	log.Info("transfers").
		Int64("transfer_id", transfer.ID).
		Int64("total_size", totalSize).
		Int("file_count", len(files)).
		Msg("Updated transfer with total file size")

	for _, file := range files {
		if p.shouldDownloadFile(transfer, file) {
			filesToDownload++
			p.queueFileDownload(transfer, file)
		} else {
			// For files we don't need to download (already exist), mark as completed
			if err := p.manager.coordinator.FileCompleted(transfer.ID); err != nil {
				log.Error("transfers").
					Int64("transfer_id", transfer.ID).
					Str("file_name", file.Name).
					Err(err).
					Msg("Failed to mark existing file as completed")
			}

			// For existing files, add their size to the downloaded size
			ctx.AddDownloadedBytes(file.Size)

			log.Debug("transfers").
				Int64("transfer_id", transfer.ID).
				Str("file_name", file.Name).
				Int64("file_size", file.Size).
				Msg("Added existing file size to downloaded total")
		}
	}
	return filesToDownload
}

// shouldDownloadFile determines if a file needs to be downloaded
func (p *TransferProcessor) shouldDownloadFile(transfer *putio.Transfer, file *putio.File) bool {
	category := p.manager.localCategory(transfer.ID)
	targetPath := filepath.Join(p.targetDir, category, transfer.Name, file.Name)
	info, err := os.Stat(targetPath)

	// Skip if file exists with correct size
	if err == nil && info.Size() == file.Size {
		log.Info("transfers").
			Str("file_name", file.Name).
			Int64("file_id", file.ID).
			Msg("File already exists, skipping download")
		return false
	}

	// Skip if already being downloaded
	if _, exists := p.manager.activeFiles.Load(file.ID); exists {
		log.Debug("transfers").
			Str("file_name", file.Name).
			Int64("file_id", file.ID).
			Msg("File already being downloaded")
		return false
	}

	return true
}

// queueFileDownload adds a file to the download queue
func (p *TransferProcessor) queueFileDownload(transfer *putio.Transfer, file *putio.File) {
	category := p.manager.localCategory(transfer.ID)
	p.manager.QueueDownload(downloadJob{
		FileID:     file.ID,
		Name:       filepath.Join(category, transfer.Name, file.Name),
		TransferID: transfer.ID,
	})
	log.Debug("transfers").
		Str("file_name", file.Name).
		Int64("file_id", file.ID).
		Int64("size", file.Size).
		Msg("Queued file for download")
}

// initializeTransfer sets up transfer tracking
func (p *TransferProcessor) initializeTransfer(transfer *putio.Transfer, filesToDownload int) bool {
	p.manager.coordinator.InitiateTransfer(transfer.ID, transfer.Name, transfer.FileID, filesToDownload)
	if err := p.manager.coordinator.StartDownload(transfer.ID); err != nil {
		log.Error("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Err(err).
			Msg("Failed to start transfer download")
		p.manager.coordinator.FailTransfer(transfer.ID, err)
		return false
	}

	if filesToDownload > 0 {
		log.Info("transfers").
			Str("name", transfer.Name).
			Int("files", filesToDownload).
			Msg("Queued files for download")
	}
	return true
}

// processErroredTransfers handles failed transfers with retry logic
func (p *TransferProcessor) processErroredTransfers(byStatus transfersByStatus) {
	const maxRetryAttempts = 3

	for _, transfer := range byStatus["ERROR"] {
		// Get current retry count
		retryCountValue, exists := p.retryAttempts.Load(transfer.ID)
		retryCount := 0
		if exists {
			retryCount = retryCountValue.(int)
		}

		// Log the error with retry information
		logger := log.Error("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Str("error", transfer.ErrorMessage).
			Int("retry_count", retryCount)

		// Check if we should retry or delete
		if retryCount < maxRetryAttempts {
			// Increment retry count
			p.retryAttempts.Store(transfer.ID, retryCount+1)

			// Log retry attempt
			logger.Msgf("Transfer errored, retrying (attempt %d of %d)", retryCount+1, maxRetryAttempts)

			// Attempt to retry the transfer
			retried, err := p.manager.client.RetryTransfer(p.manager.Context(), transfer.ID)
			if err != nil {
				log.Error("transfers").
					Str("name", transfer.Name).
					Int64("id", transfer.ID).
					Err(err).
					Msgf("Failed to retry transfer (attempt %d of %d)", retryCount+1, maxRetryAttempts)
			} else {
				log.Info("transfers").
					Str("name", transfer.Name).
					Int64("id", transfer.ID).
					Str("new_status", retried.Status).
					Msgf("Successfully retried transfer (attempt %d of %d)", retryCount+1, maxRetryAttempts)
			}
		} else {
			// Log that we're giving up after max retries
			logger.Msgf("Transfer errored, giving up after %d retry attempts", maxRetryAttempts)

			// Delete the transfer after max retries
			if err := p.manager.client.DeleteTransfer(p.manager.Context(), transfer.ID); err != nil {
				log.Error("transfers").
					Str("name", transfer.Name).
					Int64("id", transfer.ID).
					Err(err).
					Msg("Failed to delete errored transfer after max retries")
			} else {
				// Clear retry counter after successful deletion
				p.retryAttempts.Delete(transfer.ID)
				log.Info("transfers").
					Str("name", transfer.Name).
					Int64("id", transfer.ID).
					Msg("Deleted errored transfer after max retries")
			}
		}
	}
}

// finalizeCompletedTransfers checks for transfers that are marked as completed in the
// internal tracking system but haven't been fully cleaned up yet.
func (p *TransferProcessor) finalizeCompletedTransfers() {
	// Get all active transfers from the coordinator
	var pendingCleanup []int64

	p.manager.coordinator.RangeTransfers(func(transferID int64, ctx *TransferContext) bool {

		state := ctx.GetState()

		if state == TransferLifecycleCompleted {
			log.Info("transfers").
				Int64("id", transferID).
				Str("name", ctx.Name).
				Msg("Found completed transfer pending cleanup")
			pendingCleanup = append(pendingCleanup, transferID)
		}
		return true
	})

	// Process any transfers that need cleanup
	for _, transferID := range pendingCleanup {
		log.Info("transfers").
			Int64("id", transferID).
			Msg("Finalizing completed transfer")

		if err := p.manager.coordinator.CompleteTransfer(transferID); err != nil {
			log.Error("transfers").
				Int64("id", transferID).
				Err(err).
				Msg("Failed to finalize completed transfer")
		}
	}
}
