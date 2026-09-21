package download

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/elsbrock/go-putio"
)

// LocalManifest separates immutable, ID-keyed ownership from the latest remote
// display name. Legacy arrays use their single top-level directory; new
// manifests record the processing-time root explicitly. Neither is authority
// to adopt other files found in that directory.
type LocalManifest struct {
	TransferID int64          `json:"transferId"`
	Hash       string         `json:"hash,omitempty"`
	LocalRoot  string         `json:"localRoot"`
	RemoteName string         `json:"remoteName"`
	Files      []TransferFile `json:"files"`
}

func (manifest LocalManifest) resolve() (LocalManifest, error) {
	if err := manifest.validateFiles(); err != nil {
		return manifest, err
	}
	if manifest.LocalRoot == "" && len(manifest.Files) > 0 {
		root, err := legacyLocalRoot(manifest.Files)
		if err != nil {
			return manifest, err
		}
		manifest.LocalRoot = root
	}
	return manifest, manifest.validateRoot()
}

// validate accepts a stored record whose legacy root cannot be resolved: its
// widest claim still guards other transfers even while its own boundary is
// unknown, and only the transfer itself is refused.
func (manifest LocalManifest) validate() (LocalManifest, error) {
	if err := manifest.validateFiles(); err != nil {
		return manifest, err
	}
	return manifest, manifest.validateRoot()
}

func (manifest LocalManifest) validateFiles() error {
	if manifest.TransferID <= 0 {
		return fmt.Errorf("invalid manifest transfer ID %d", manifest.TransferID)
	}
	seen := make(map[string]bool, len(manifest.Files))
	for _, file := range manifest.Files {
		name := filepath.FromSlash(file.Name)
		if !safeManifestPath(name) || file.Length < 0 {
			return fmt.Errorf("unsafe manifest entry %q (length %d)", file.Name, file.Length)
		}
		root, _, hasFile := strings.Cut(name, string(filepath.Separator))
		if !hasFile || IsReservedTransferName(root) {
			return fmt.Errorf("unsafe manifest root in %q", file.Name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate manifest file %q", file.Name)
		}
		seen[name] = true
	}
	return nil
}

func (manifest LocalManifest) validateRoot() error {
	if manifest.LocalRoot == "" {
		return nil
	}
	if !safeManifestPath(manifest.LocalRoot) || IsReservedTransferName(strings.Split(manifest.LocalRoot, string(filepath.Separator))[0]) {
		return fmt.Errorf("unsafe manifest root %q", manifest.LocalRoot)
	}
	for _, file := range manifest.Files {
		name := filepath.FromSlash(file.Name)
		rel, err := filepath.Rel(manifest.LocalRoot, name)
		if err != nil || !safeManifestPath(rel) {
			return fmt.Errorf("manifest file %q is outside local root %q", file.Name, manifest.LocalRoot)
		}
	}
	return nil
}

func safeManifestPath(path string) bool {
	return path != "." && filepath.IsLocal(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\\\x00")
}

// A legacy array records only file paths, never how many of their components
// formed the transfer root. An entry sitting directly inside the root proves
// where it ends; below that the boundary is unknowable, and the mutable remote
// display name is not evidence, so ownership is refused instead of inferred.
func legacyLocalRoot(files []TransferFile) (string, error) {
	root, _, _ := strings.Cut(filepath.FromSlash(files[0].Name), string(filepath.Separator))
	proven := false
	for _, file := range files {
		dirs := manifestDirs(file.Name)
		if len(dirs) == 0 || dirs[0] != root {
			return "", fmt.Errorf("legacy manifest files do not share one local root")
		}
		proven = proven || len(dirs) == 1
	}
	if !proven {
		return "", fmt.Errorf("ambiguous legacy manifest root below %q", root)
	}
	return root, nil
}

func manifestDirs(name string) []string {
	parts := strings.Split(filepath.FromSlash(name), string(filepath.Separator))
	return parts[:len(parts)-1]
}

// claimedRoot is the widest directory a manifest could own: its recorded root,
// or the shared first component of a legacy array whose root was never written
// down. Collision detection uses it so a narrower reading of one record can
// never hide an overlap with another.
func (manifest LocalManifest) claimedRoot() string {
	if manifest.LocalRoot != "" || len(manifest.Files) == 0 {
		return manifest.LocalRoot
	}
	root, _, _ := strings.Cut(filepath.FromSlash(manifest.Files[0].Name), string(filepath.Separator))
	return root
}

// ManifestCheck selects how much local evidence a manifest read requires.
// Ownership, confinement and symlink rejection are enforced in every mode.
type ManifestCheck int

const (
	// ManifestCheckPending tolerates a transfer whose local copy is still
	// being written, including a root that does not exist yet.
	ManifestCheckPending ManifestCheck = iota
	// ManifestCheckProcessed reports historical metadata for a transfer this
	// instance already downloaded. The payload may have been imported, renamed
	// or resized, and an unchanged-name root may be gone altogether.
	ManifestCheckProcessed
	// ManifestCheckComplete requires every manifest file at its exact length.
	ManifestCheckComplete
)

// GetTransferManifest is read-only: an absent manifest never authorizes a scan
// or adoption, and a corrupt manifest is an error rather than absence.
func (m *Manager) GetTransferManifest(transfer *putio.Transfer, check ManifestCheck) (LocalManifest, error) {
	return m.TransferFileReader().GetTransferManifest(transfer, check)
}

// TransferFileReader keeps one ownership snapshot for a files-inclusive RPC,
// avoiding a full disk scan for every torrent in the response.
type TransferFileReader interface {
	GetTransferManifest(*putio.Transfer, ManifestCheck) (LocalManifest, error)
}

type manifestSnapshot struct {
	targetDir  string
	manifests  map[int64]LocalManifest
	categories map[int64]string
	errors     map[int64]error
	scanErr    error
}

func (m *Manager) TransferFileReader() TransferFileReader {
	m.transferFiles.mu.RLock()
	defer m.transferFiles.mu.RUnlock()
	return m.readManifests()
}

// Caller holds transferFiles.mu, including across publication of a new claim.
func (m *Manager) readManifests() *manifestSnapshot {
	snapshot := &manifestSnapshot{targetDir: m.cfg.TargetDir, manifests: make(map[int64]LocalManifest), categories: make(map[int64]string), errors: make(map[int64]error)}
	root, err := os.OpenRoot(m.cfg.TargetDir)
	if os.IsNotExist(err) {
		return snapshot
	}
	if err != nil {
		snapshot.scanErr = err
		return snapshot
	}
	defer root.Close()
	info, err := root.Lstat(transferFilesStateDirName)
	if os.IsNotExist(err) {
		return snapshot
	}
	if err != nil {
		snapshot.scanErr = err
		return snapshot
	}
	if info.Mode()&os.ModeSymlink != 0 {
		snapshot.scanErr = fmt.Errorf("symlink in transfer file state directory")
		return snapshot
	}
	dir, err := root.Open(transferFilesStateDirName)
	if err != nil {
		snapshot.scanErr = err
		return snapshot
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		snapshot.scanErr = fmt.Errorf("read manifest ownership: %w", err)
	}
	for _, entry := range entries {
		id, err := strconv.ParseInt(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
		if err != nil || id <= 0 || entry.Name() != strconv.FormatInt(id, 10)+".json" {
			continue
		}
		manifest, err := m.transferFiles.loadManifest(id)
		if err == nil {
			_, err = manifest.validate()
		}
		if err != nil {
			snapshot.errors[id] = err
			continue
		}
		snapshot.manifests[id] = manifest
		snapshot.categories[id] = m.localCategory(id)
	}
	return snapshot
}

func (s *manifestSnapshot) GetTransferManifest(transfer *putio.Transfer, check ManifestCheck) (LocalManifest, error) {
	if err := s.errors[transfer.ID]; err != nil {
		return LocalManifest{}, err
	}
	manifest, exists := s.manifests[transfer.ID]
	if !exists {
		manifest.TransferID = transfer.ID
	}
	return s.validateManifest(transfer, manifest, check)
}

func (s *manifestSnapshot) validateManifest(transfer *putio.Transfer, manifest LocalManifest, check ManifestCheck) (LocalManifest, error) {
	if s.scanErr != nil {
		return manifest, s.scanErr
	}
	if manifest.Hash != "" && transfer.Hash != "" && !strings.EqualFold(manifest.Hash, transfer.Hash) {
		return manifest, fmt.Errorf("manifest hash does not match transfer %d", transfer.ID)
	}
	// Latest poll metadata is explicit in the result, independent of the stored
	// processing-time name. Reading never rewrites the ownership record.
	manifest.RemoteName = transfer.Name
	claim := manifest.claimedRoot()
	manifest, err := manifest.resolve()
	if err != nil || len(manifest.Files) == 0 {
		return manifest, err
	}
	category := s.categories[transfer.ID]
	if category != "" && !safeManifestPath(category) {
		return manifest, fmt.Errorf("unsafe manifest category %q", category)
	}
	localRoot := filepath.Join(category, manifest.LocalRoot)
	if err := s.checkManifestCollision(transfer.ID, filepath.Join(category, claim)); err != nil {
		return manifest, err
	}
	root, err := os.OpenRoot(s.targetDir)
	if os.IsNotExist(err) && check == ManifestCheckPending && manifest.LocalRoot == transfer.Name {
		return manifest, nil // The ordinary first download creates the target.
	}
	if err != nil {
		return manifest, fmt.Errorf("open download root: %w", err)
	}
	defer root.Close()
	// An unchanged-name transfer may not have created its root yet, and an
	// importer may have moved the finished payload out and removed it again.
	// Name drift always requires the root; never create a replacement for it.
	rootRequired := check == ManifestCheckComplete || manifest.LocalRoot != transfer.Name
	if err := checkManifestPath(root, localRoot, true, rootRequired, 0, check); err != nil {
		return manifest, err
	}
	for _, file := range manifest.Files {
		required := check == ManifestCheckComplete
		if err := checkManifestPath(root, filepath.Join(category, filepath.FromSlash(file.Name)), false, required, file.Length, check); err != nil {
			return manifest, err
		}
	}
	return manifest, nil
}

// Reject symlinks in every component, including category and transfer roots.
// os.Root also confines each lookup if a component changes during validation.
func checkManifestPath(root *os.Root, path string, directory, required bool, length int64, check ManifestCheck) error {
	parts := strings.Split(path, string(filepath.Separator))
	for i := range parts {
		component := filepath.Join(parts[:i+1]...)
		info, err := root.Lstat(component)
		if os.IsNotExist(err) && !required {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stat manifest path %q: %w", component, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in manifest path %q", component)
		}
		if i < len(parts)-1 || directory {
			if !info.IsDir() {
				return fmt.Errorf("manifest root %q is not a directory", component)
			}
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("manifest file %q is not a regular file", path)
		} else if check != ManifestCheckProcessed && (info.Size() > length || (check == ManifestCheckComplete && info.Size() != length)) {
			return fmt.Errorf("manifest file %q does not match expected length %d", path, length)
		}
	}
	return nil
}

func (s *manifestSnapshot) checkManifestCollision(id int64, root string) error {
	if len(s.errors) > 0 {
		lowest := sortedManifestIDs(s.errors)[0]
		return fmt.Errorf("cannot establish ownership: manifest %d: %w", lowest, s.errors[lowest])
	}
	for _, otherID := range sortedManifestIDs(s.manifests) {
		other := s.manifests[otherID]
		if otherID == id || len(other.Files) == 0 {
			continue
		}
		category := s.categories[otherID]
		if category != "" && !safeManifestPath(category) {
			return fmt.Errorf("unsafe category for manifest %d", otherID)
		}
		otherRoot := filepath.Join(category, other.claimedRoot())
		// Conservatively reject case-only aliases on both case-sensitive and
		// case-insensitive volumes. Ancestor claims also collide across categories.
		a, b := strings.ToLower(root), strings.ToLower(otherRoot)
		if a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator)) {
			return fmt.Errorf("local root %q collides with transfer %d root %q", root, otherID, otherRoot)
		}
	}
	return nil
}

// Ownership diagnostics must name the same competing record on every read,
// so an operator can resolve a blocking manifest instead of chasing whichever
// one map iteration happened to surface.
func sortedManifestIDs[V any](m map[int64]V) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// ManifestLocalRoot reports the local root one transfer's persisted manifest
// owns. Reconciliation needs it to keep a transfer whose remote name has
// drifted from being classified as unmanaged local data. Unreadable ownership
// evidence fails closed rather than silently shrinking the protected set.
func ManifestLocalRoot(targetDir string, transfer *putio.Transfer) (string, error) {
	manifest, err := newTransferFileStore(targetDir).loadManifest(transfer.ID)
	if err == nil {
		manifest, err = manifest.resolve()
	}
	if err != nil {
		return "", fmt.Errorf("manifest ownership for transfer %d: %w", transfer.ID, err)
	}
	return manifest.LocalRoot, nil
}

// prepareManifest retains an existing claim across retries/restarts. A changed
// remote file list is not permission to overwrite local ownership evidence.
func (m *Manager) prepareManifest(transfer *putio.Transfer, files []*putio.File) (*putio.Transfer, error) {
	m.transferFiles.mu.Lock()
	defer m.transferFiles.mu.Unlock()
	stored, err := m.transferFiles.loadManifest(transfer.ID)
	if err != nil {
		return nil, err
	}
	local := *transfer
	snapshot := m.readManifests()
	snapshot.categories[transfer.ID] = m.localCategory(transfer.ID)
	if len(stored.Files) > 0 {
		if stored.LocalRoot == "" {
			stored.LocalRoot, err = legacyManifestRoot(stored.Files, files)
			if err != nil {
				return nil, err
			}
		}
		manifest, err := snapshot.validateManifest(transfer, stored, ManifestCheckPending)
		if err != nil {
			return nil, err
		}
		local.Name = manifest.LocalRoot
	}
	expected, err := buildTransferFileManifest(&local, files)
	if err != nil {
		return nil, err
	}
	if len(stored.Files) > 0 {
		lengths := make(map[string]int64, len(stored.Files))
		for _, file := range stored.Files {
			lengths[file.Name] = file.Length
		}
		if len(stored.Files) != len(expected) {
			return nil, fmt.Errorf("remote file list differs from persisted manifest for transfer %d", transfer.ID)
		}
		for _, file := range expected {
			if length, ok := lengths[file.Name]; !ok || length != file.Length {
				return nil, fmt.Errorf("remote file %q differs from persisted manifest for transfer %d", file.Name, transfer.ID)
			}
		}
	} else {
		manifest := LocalManifest{TransferID: transfer.ID, Hash: transfer.Hash, LocalRoot: filepath.Clean(transfer.Name), RemoteName: transfer.Name, Files: expected}
		if _, err := snapshot.validateManifest(&local, manifest, ManifestCheckPending); err != nil {
			return nil, err
		}
		if err := m.transferFiles.setManifest(manifest); err != nil {
			return nil, err
		}
	}
	return &local, nil
}

// A legacy array does not record how many path components formed its root.
// When the source is present, only a root reproducing the entire stored file
// set (names AND sizes) is usable. The remote display name is not evidence.
func legacyManifestRoot(stored []TransferFile, files []*putio.File) (string, error) {
	if len(files) == 0 || files[0] == nil {
		return "", fmt.Errorf("remote file list differs from persisted manifest")
	}
	lengths := make(map[string]int64, len(stored))
	for _, file := range stored {
		lengths[file.Name] = file.Length
	}
	var found string
	for _, file := range stored {
		root, ok := strings.CutSuffix(file.Name, "/"+filepath.ToSlash(files[0].Name))
		if !ok || !safeManifestPath(root) || len(files) != len(stored) {
			continue
		}
		expected, err := buildTransferFileManifest(&putio.Transfer{Name: root}, files)
		if err != nil {
			return "", err
		}
		matches := true
		for _, candidate := range expected {
			if length, ok := lengths[candidate.Name]; !ok || length != candidate.Length {
				matches = false
				break
			}
		}
		if matches {
			if found != "" && found != root {
				return "", fmt.Errorf("ambiguous legacy manifest root")
			}
			found = root
		}
	}
	if found == "" {
		return "", fmt.Errorf("remote file list differs from persisted manifest")
	}
	return found, nil
}
