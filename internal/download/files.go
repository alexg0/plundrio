package download

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/elsbrock/plundrio/internal/log"
)

const transferFilesStateDirName = ".plundrio-files"

// IsReservedTransferName reports whether name would occupy Plundrio's
// manifest directory in the download root.
func IsReservedTransferName(name string) bool {
	return strings.EqualFold(filepath.Clean(name), transferFilesStateDirName)
}

// TransferFile describes one local file belonging to a Put.io transfer. Name
// is relative to the transfer's reported Transmission downloadDir.
type TransferFile struct {
	Name   string `json:"name"`
	Length int64  `json:"length"`
}

// TransferFileStore preserves one authoritative manifest per transfer after
// Plundrio deletes the source file from Put.io.
type TransferFileStore struct {
	stateDir string
}

func newTransferFileStore(targetDir string) *TransferFileStore {
	return &TransferFileStore{stateDir: filepath.Join(targetDir, transferFilesStateDirName)}
}

func (fs *TransferFileStore) path(transferID int64) string {
	return filepath.Join(fs.stateDir, strconv.FormatInt(transferID, 10)+".json")
}

// Set stores a transfer's complete expected file list. If interrupted, Put.io
// still owns the source and the next poll rewrites the manifest before cleanup.
func (fs *TransferFileStore) Set(transferID int64, files []TransferFile) error {
	if transferID <= 0 || len(files) == 0 {
		return fmt.Errorf("transfer file manifest requires a transfer ID and at least one file")
	}
	if err := os.MkdirAll(fs.stateDir, 0700); err != nil {
		return fmt.Errorf("create transfer file state: %w", err)
	}
	data, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("marshal transfer file state: %w", err)
	}
	if err := os.WriteFile(fs.path(transferID), data, 0600); err != nil {
		return fmt.Errorf("write transfer file state: %w", err)
	}
	return nil
}

func (fs *TransferFileStore) Get(transferID int64) ([]TransferFile, bool) {
	data, err := os.ReadFile(fs.path(transferID))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Error("files").Err(err).Msg("Failed to load transfer file state")
		}
		return nil, false
	}
	var files []TransferFile
	if err := json.Unmarshal(data, &files); err != nil {
		log.Error("files").Err(err).Msg("Failed to parse transfer file state")
		return nil, false
	}
	return files, len(files) > 0
}

// Remove deletes a transfer's manifest after the Transmission client removes it.
func (fs *TransferFileStore) Remove(transferID int64) error {
	err := os.Remove(fs.path(transferID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove transfer file state: %w", err)
	}
	return nil
}
