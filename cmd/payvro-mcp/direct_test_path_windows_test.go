//go:build windows

package main_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func sdkDirectTestJournalPath(t *testing.T) string {
	t.Helper()
	root := os.Getenv("LOCALAPPDATA")
	if root == "" {
		t.Fatal("LOCALAPPDATA is required for native Windows SDK DIRECT test")
	}
	name := fmt.Sprintf("payvro-sdk-test-%d-%s", os.Getpid(), t.Name())
	dir := filepath.Join(root, name)
	t.Setenv("APPDATA", filepath.Join(root, name+"-roaming-empty"))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "direct.jsonl")
}
