package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/elsbrock/plundrio/internal/reconcile"
	"github.com/spf13/cobra"
)

func TestWriteDeleteReportPreservesJSONOnPartialFailure(t *testing.T) {
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	report := reconcile.DeleteReport{
		SchemaVersion: reconcile.SchemaVersion,
		Results:       []reconcile.DeleteResult{{ID: "putio:2", Source: "putio", Status: "failed", Error: "denied"}},
		Summary:       reconcile.DeleteSummary{SelectedCount: 1, Failed: 1},
	}

	err := writeDeleteReport(cmd, report)
	var partial partialDeleteError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %v, want partialDeleteError", err)
	}
	var decoded reconcile.DeleteReport
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not complete JSON: %v\n%s", err, output.String())
	}
	if decoded.Results[0].Error != "denied" {
		t.Fatalf("decoded result = %+v", decoded.Results[0])
	}
}
