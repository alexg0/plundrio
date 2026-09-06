//go:build !unix

package reconcile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalDeleteFailsClosedWithoutUnixIdentity(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "keep"), "keep")
	err := deleteLocalObject(root, Object{Path: "keep"})
	if err == nil || !strings.Contains(err.Error(), "requires Unix filesystem identity") {
		t.Fatalf("unsupported mutation = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep")); err != nil {
		t.Fatal(err)
	}
}
