//go:build !windows

package direct

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type journal struct{ path string }

var errReferenceCollision = errors.New("local QR reference collision")

// journalDirectorySync is replaceable in tests to prove failures stop before POST.
var journalDirectorySync = syncDirectory

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

type journalEvent struct {
	Version        int    `json:"version"`
	Account        string `json:"account"`
	KeyHash        string `json:"keyHash"`
	IntentHash     string `json:"intentHash"`
	State          string `json:"state"`
	Mode           string `json:"mode"`
	Method         string `json:"method"`
	AmountCentavos int64  `json:"amountCentavos"`
	Reference      string `json:"reference,omitempty"`
	Charge         Charge `json:"charge,omitempty"`
	RejectedCode   string `json:"rejectedCode,omitempty"`
	CreatedAt      int64  `json:"createdAt"`
}

func (j *journal) Reserve(account, keyHash, intentHash, method string, amount int64, mode string) (journalEvent, bool, error) {
	var result journalEvent
	fresh := false
	err := j.locked(func(events []journalEvent) ([]journalEvent, error) {
		for i := len(events) - 1; i >= 0; i-- {
			e := events[i]
			if e.Account == account && e.KeyHash == keyHash {
				if e.IntentHash != intentHash {
					return events, ErrIdempotencyConflict
				}
				result = e
				return events, nil
			}
		}
		result = journalEvent{Version: 1, Account: account, KeyHash: keyHash, IntentHash: intentHash, State: "reserved", Mode: mode, Method: method, AmountCentavos: amount, CreatedAt: time.Now().Unix()}
		events = append(events, result)
		fresh = true
		return events, nil
	}, true)
	return result, fresh, err
}

func (j *journal) SetReference(account, keyHash, reference string) error {
	return j.locked(func(events []journalEvent) ([]journalEvent, error) {
		for _, existing := range events {
			if existing.Account == account && existing.Method == "qr" && existing.Reference == reference {
				return events, errReferenceCollision
			}
		}
		for i := len(events) - 1; i >= 0; i-- {
			e := events[i]
			if e.Account == account && e.KeyHash == keyHash {
				if e.State != "reserved" {
					return events, ErrUncertain
				}
				e.Reference = reference
				e.CreatedAt = time.Now().Unix()
				return append(events, e), nil
			}
		}
		return events, ErrJournalUnavailable
	}, true)
}

func (j *journal) Finish(account, keyHash, intentHash, state string, charge Charge, rejectedCode string) error {
	if state != "created" && state != "uncertain" && state != "rejected" {
		return ErrJournalUnavailable
	}
	return j.locked(func(events []journalEvent) ([]journalEvent, error) {
		for i := len(events) - 1; i >= 0; i-- {
			e := events[i]
			if e.Account == account && e.KeyHash == keyHash {
				if e.IntentHash != intentHash {
					return events, ErrIdempotencyConflict
				}
				if e.State != "reserved" {
					return events, ErrUncertain
				}
				e.State = state
				e.Charge = charge
				e.RejectedCode = rejectedCode
				e.CreatedAt = time.Now().Unix()
				return append(events, e), nil
			}
		}
		return events, ErrJournalUnavailable
	}, true)
}

func (j *journal) Find(account, keyHash string) (journalEvent, bool, error) {
	// The append-only file is read without creating a lock file so status lookup
	// remains read-only. A concurrent partial append fails closed on the newline check.
	path, err := filepath.Abs(j.path)
	if err != nil {
		return journalEvent{}, false, ErrJournalUnavailable
	}
	events, err := readJournal(path)
	if err != nil {
		return journalEvent{}, false, ErrJournalUnavailable
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Account == account && events[i].KeyHash == keyHash {
			return events[i], true, nil
		}
	}
	return journalEvent{}, false, nil
}

func (j *journal) locked(change func([]journalEvent) ([]journalEvent, error), write bool) error {
	if j.path == "" {
		return ErrJournalUnavailable
	}
	path, err := filepath.Abs(j.path)
	if err != nil {
		return ErrJournalUnavailable
	}
	dir := filepath.Dir(path)
	if err := ensurePrivateJournalDir(dir); err != nil {
		return ErrJournalUnavailable
	}
	lockPath := path + ".lock"
	lock, err := acquireLock(lockPath)
	if err != nil {
		return ErrJournalUnavailable
	}
	defer func() { _ = lock.Close(); _ = os.Remove(lockPath) }()
	if err := hardenJournal(path); err != nil {
		return ErrJournalUnavailable
	}
	events, err := readJournal(path)
	if err != nil {
		return ErrJournalUnavailable
	}
	updated, err := change(events)
	if err != nil {
		return err
	}
	if write && len(updated) > len(events) {
		if err := appendEvent(path, updated[len(updated)-1], len(events)); err != nil {
			return ErrJournalUnavailable
		}
	}
	return nil
}

func acquireLock(path string) (*os.File, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			if _, err = fmt.Fprintf(f, "%d\n", os.Getpid()); err == nil {
				err = f.Sync()
			}
			if err == nil {
				err = journalDirectorySync(filepath.Dir(path))
			}
			if err != nil {
				_ = f.Close()
				_ = os.Remove(path)
				return nil, err
			}
			return f, nil
		}
		if !errors.Is(err, os.ErrExist) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func ensurePrivateJournalDir(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(abs)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0700 {
			return ErrJournalUnavailable
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Only create the dedicated journal directory itself. Never chmod or
	// recursively create arbitrary pre-existing/shared path components.
	parent := filepath.Dir(abs)
	parentInfo, err := os.Lstat(parent)
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return ErrJournalUnavailable
	}
	if err := os.Mkdir(abs, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		// Another process may have created it; accept only an already-private dir.
		info, statErr := os.Lstat(abs)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0700 {
			return ErrJournalUnavailable
		}
		if err := journalDirectorySync(parent); err != nil {
			return err
		}
		return journalDirectorySync(abs)
	}
	if err := journalDirectorySync(parent); err != nil {
		return err
	}
	if err := journalDirectorySync(abs); err != nil {
		return err
	}
	return nil
}

func hardenJournal(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ErrJournalUnavailable
	}
	if info.Mode().Perm() == 0600 {
		return nil
	}
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func readJournal(path string) ([]journalEvent, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || !os.SameFile(info, pathInfo) || info.Size() > maxJournalBytes || info.Mode().Perm() != 0600 {
		return nil, ErrJournalUnavailable
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || parent.Mode()&os.ModeSymlink != 0 || !parent.IsDir() || parent.Mode().Perm() != 0700 {
		return nil, ErrJournalUnavailable
	}
	if info.Size() > 0 {
		last := []byte{0}
		_, readErr := f.ReadAt(last, info.Size()-1)
		if readErr != nil || last[0] != '\n' {
			return nil, ErrJournalUnavailable
		}
	}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1024), 20*1024)
	var events []journalEvent
	for s.Scan() {
		var e journalEvent
		if json.Unmarshal(s.Bytes(), &e) != nil || e.Version != 1 || len(e.Account) != 64 || len(e.KeyHash) != 64 || len(e.IntentHash) != 64 || !validJournalState(e.State) || (e.Mode != "DEMO" && e.Mode != "LIVE") || (e.Method != "qr" && e.Method != "link") || e.AmountCentavos <= 0 {
			return nil, ErrJournalUnavailable
		}
		events = append(events, e)
		if len(events) > maxJournalEntries {
			return nil, ErrJournalUnavailable
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func appendEvent(path string, e journalEvent, count int) error {
	if count >= maxJournalEntries {
		return ErrJournalUnavailable
	}
	encoded, err := json.Marshal(e)
	if err != nil || len(encoded) > 16*1024 {
		return ErrJournalUnavailable
	}
	_, statErr := os.Lstat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return statErr
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	info, err := f.Stat()
	if err != nil || info.Size()+int64(len(encoded))+1 > maxJournalBytes {
		_ = f.Close()
		return ErrJournalUnavailable
	}
	if _, err := f.Write(append(encoded, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if created {
		return journalDirectorySync(filepath.Dir(path))
	}
	return nil
}

func validJournalState(state string) bool {
	switch state {
	case "reserved", "created", "uncertain", "rejected":
		return true
	}
	return false
}
