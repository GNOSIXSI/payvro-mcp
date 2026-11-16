//go:build !windows

package tools_test

import (
	"os"
	"path/filepath"
	"testing"
)

func directTestJournalPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "journal.jsonl")
}
