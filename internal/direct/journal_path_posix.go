//go:build !windows

package direct

import (
	"os"
	"path/filepath"
	"strings"
)

func resolveJournalPath(configured string) (string, error) {
	if configured == "" {
		root, err := os.UserConfigDir()
		if err != nil || root == "" {
			return "", ErrInvalidConfig
		}
		configured = filepath.Join(root, "payvro", "direct-charges.jsonl")
	}
	journalPath, err := filepath.Abs(configured)
	if err != nil || filepath.Ext(journalPath) != ".jsonl" || strings.HasPrefix(filepath.Base(journalPath), ".env") {
		return "", ErrInvalidConfig
	}
	return journalPath, nil
}
