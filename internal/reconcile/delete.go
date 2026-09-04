package reconcile

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// DeleteClient adds the one Put.io mutation used by explicit cleanup.
type DeleteClient interface {
	Client
	DeleteFile(context.Context, int64) error
}

// DeleteOptions describes an explicit cleanup request. Apply defaults to false.
type DeleteOptions struct {
	IDs   []string
	Putio bool
	Local bool
	Apply bool
}

// DeleteResult is the outcome for one explicitly selected object.
type DeleteResult struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	ObjectID *int64 `json:"object_id,omitempty"`
	Path     string `json:"path,omitempty"`
	Size     int64  `json:"size"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Error    string `json:"error,omitempty"`
}

// DeleteSummary provides exact batch totals for dry runs and applied cleanup.
type DeleteSummary struct {
	SelectedCount int   `json:"selected_count"`
	SelectedBytes int64 `json:"selected_bytes"`
	WouldDelete   int   `json:"would_delete_count"`
	Deleted       int   `json:"deleted_count"`
	Skipped       int   `json:"skipped_count"`
	Failed        int   `json:"failed_count"`
}

// DeleteReport is emitted even when some selected objects fail or are refused.
type DeleteReport struct {
	SchemaVersion int            `json:"schema_version"`
	Applied       bool           `json:"applied"`
	Sources       []string       `json:"sources"`
	Results       []DeleteResult `json:"results"`
	Summary       DeleteSummary  `json:"summary"`
}

// HasFailures reports whether the command must return a non-zero status.
func (r DeleteReport) HasFailures() bool {
	return r.Summary.Skipped != 0 || r.Summary.Failed != 0
}

// Delete revalidates every report ID immediately before optionally deleting it.
func (s *Service) Delete(ctx context.Context, options DeleteOptions) (DeleteReport, error) {
	ids, err := validateDeleteOptions(options)
	if err != nil {
		return DeleteReport{}, err
	}

	report := DeleteReport{
		SchemaVersion: SchemaVersion,
		Applied:       options.Apply,
		Sources:       selectedSources(options),
		Results:       make([]DeleteResult, 0, len(ids)),
	}
	for _, id := range ids {
		result := s.deleteOne(ctx, id, options)
		report.Results = append(report.Results, result)
		report.Summary.SelectedCount++
		report.Summary.SelectedBytes += result.Size
		switch result.Status {
		case "would_delete":
			report.Summary.WouldDelete++
		case "deleted":
			report.Summary.Deleted++
		case "skipped":
			report.Summary.Skipped++
		case "failed":
			report.Summary.Failed++
		}
	}
	return report, nil
}

func validateDeleteOptions(options DeleteOptions) ([]string, error) {
	if !options.Putio && !options.Local {
		return nil, fmt.Errorf("select at least one deletion source with --putio or --local")
	}
	if len(options.IDs) == 0 {
		return nil, fmt.Errorf("select at least one report ID with --id")
	}

	unique := make(map[string]struct{}, len(options.IDs))
	for _, id := range options.IDs {
		if err := validateSelectedID(id, options); err != nil {
			return nil, err
		}
		unique[id] = struct{}{}
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func validateSelectedID(id string, options DeleteOptions) error {
	switch {
	case strings.HasPrefix(id, "putio:"):
		if !options.Putio {
			return fmt.Errorf("Put.io ID %q requires --putio", id)
		}
		value, err := strconv.ParseInt(strings.TrimPrefix(id, "putio:"), 10, 64)
		if err != nil || value <= 0 {
			return fmt.Errorf("invalid Put.io report ID %q", id)
		}
	case strings.HasPrefix(id, "local:"):
		if !options.Local {
			return fmt.Errorf("local ID %q requires --local", id)
		}
		digest := strings.TrimPrefix(id, "local:")
		if len(digest) != sha256HexLength || !isLowerHex(digest) {
			return fmt.Errorf("invalid local report ID %q", id)
		}
	default:
		return fmt.Errorf("invalid report ID %q", id)
	}
	return nil
}

const sha256HexLength = 64

func isLowerHex(value string) bool {
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func selectedSources(options DeleteOptions) []string {
	sources := make([]string, 0, 2)
	if options.Local {
		sources = append(sources, "local")
	}
	if options.Putio {
		sources = append(sources, "putio")
	}
	return sources
}

func (s *Service) deleteOne(ctx context.Context, id string, options DeleteOptions) DeleteResult {
	current, err := s.Reconcile(ctx)
	if err != nil {
		return DeleteResult{ID: id, Source: sourceFromID(id), Status: "failed", Reason: "revalidation_failed", Error: err.Error()}
	}
	if object, ok := findObject(current.Active, id); ok {
		return resultForObject(object, "skipped", "active_transfer", nil)
	}
	object, ok := findObject(current.Unmanaged, id)
	if !ok {
		return DeleteResult{ID: id, Source: sourceFromID(id), Status: "skipped", Reason: "not_unmanaged"}
	}
	if !options.Apply {
		return resultForObject(object, "would_delete", "", nil)
	}

	switch object.Source {
	case "putio":
		client, ok := s.client.(DeleteClient)
		if !ok {
			return resultForObject(object, "failed", "delete_unavailable", nil)
		}
		if object.ObjectID == nil || *object.ObjectID <= 0 {
			return resultForObject(object, "failed", "invalid_object_id", nil)
		}
		if err := client.DeleteFile(ctx, *object.ObjectID); err != nil {
			return resultForObject(object, "failed", "delete_failed", err)
		}
	case "local":
		if err := deleteLocalObject(s.localRoot, object); err != nil {
			return resultForObject(object, "failed", "delete_failed", err)
		}
	default:
		return resultForObject(object, "failed", "invalid_source", nil)
	}
	return resultForObject(object, "deleted", "", nil)
}

func findObject(objects []Object, id string) (Object, bool) {
	for _, object := range objects {
		if object.ID == id {
			return object, true
		}
	}
	return Object{}, false
}

func sourceFromID(id string) string {
	if separator := strings.IndexByte(id, ':'); separator > 0 {
		return id[:separator]
	}
	return ""
}

func resultForObject(object Object, status, reason string, err error) DeleteResult {
	result := DeleteResult{
		ID:       object.ID,
		Source:   object.Source,
		ObjectID: object.ObjectID,
		Path:     object.Path,
		Size:     object.Size,
		Status:   status,
		Reason:   reason,
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

func deleteLocalObject(rootPath string, object Object) error {
	rel, err := confinedRelativePath(rootPath, object.Path)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open local root: %w", err)
	}
	defer func() { _ = root.Close() }()

	info, err := root.Lstat(rel)
	if err != nil {
		return fmt.Errorf("revalidate local object: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink %q", object.Path)
	}
	if err := root.RemoveAll(rel); err != nil {
		return fmt.Errorf("remove local object: %w", err)
	}
	return nil
}
