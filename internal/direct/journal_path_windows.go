//go:build windows

package direct

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

func resolveJournalPath(configured string) (string, error) {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" || !validWindowsLocalPath(localAppData) {
		return "", ErrInvalidConfig
	}
	localRoot := filepath.Clean(localAppData)
	if configured == "" {
		configured = filepath.Join(localRoot, "payvro", "direct-charges.jsonl")
	}
	if !validWindowsLocalPath(configured) || filepath.Ext(configured) != ".jsonl" || strings.HasPrefix(filepath.Base(configured), ".env") {
		return "", ErrInvalidConfig
	}
	journalPath := filepath.Clean(configured)
	relative, err := filepath.Rel(localRoot, journalPath)
	parentRelative, parentErr := filepath.Rel(localRoot, filepath.Dir(journalPath))
	if err != nil || parentErr != nil || parentRelative == "." || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", ErrInvalidConfig
	}
	if err := rejectLegacyRoamingJournal(); err != nil {
		return "", err
	}
	return journalPath, nil
}

func validWindowsLocalPath(path string) bool {
	if path == "" || !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return false
	}
	clean := filepath.Clean(path)
	if clean != path || len(clean) < 3 || clean[1] != ':' || clean[2] != '\\' || !isASCIIAlpha(clean[0]) {
		return false
	}
	if strings.Contains(clean[2:], ":") || strings.ContainsAny(clean, "\x00\r\n") {
		return false
	}
	for _, component := range strings.Split(clean[3:], `\`) {
		if component == "" {
			continue
		}
		if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") || strings.ContainsAny(component, `<>"|?*`) {
			return false
		}
		for _, r := range component {
			if unicode.IsControl(r) {
				return false
			}
		}
		if isWindowsDeviceName(component) {
			return false
		}
	}
	return true
}

func isASCIIAlpha(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

func isWindowsDeviceName(component string) bool {
	base := strings.ToUpper(strings.TrimRight(strings.SplitN(component, ".", 2)[0], " ."))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$", "CLOCK$", "COM¹", "COM²", "COM³", "LPT¹", "LPT²", "LPT³":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
}

func rejectLegacyRoamingJournal() error {
	roaming := os.Getenv("APPDATA")
	local := os.Getenv("LOCALAPPDATA")
	if roaming == "" || local == "" {
		return ErrInvalidConfig
	}
	if !validWindowsLocalPath(roaming) {
		return ErrInvalidConfig
	}
	if strings.EqualFold(filepath.Clean(roaming), filepath.Clean(local)) {
		return nil
	}
	legacy := filepath.Join(filepath.Clean(roaming), "payvro", "direct-charges.jsonl")
	for _, candidate := range []string{legacy, legacy + ".lock"} {
		if _, err := os.Lstat(candidate); err == nil {
			return ErrJournalUnavailable
		} else if !os.IsNotExist(err) {
			return ErrJournalUnavailable
		}
	}
	return nil
}
