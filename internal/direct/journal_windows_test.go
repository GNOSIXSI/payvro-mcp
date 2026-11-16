//go:build windows

package direct

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsTestIdem = "direct-idempotency-0001"
	windowsTestKey  = "synthetic-direct-key"
)

var windowsTestID atomic.Uint64

type windowsRoundTripFunc func(*http.Request) (*http.Response, error)

func (f windowsRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func windowsTestJournalPath(t *testing.T) string {
	t.Helper()
	root := filepath.Clean(getenvRequired(t, "LOCALAPPDATA"))
	id := windowsTestID.Add(1)
	dir := filepath.Join(root, fmt.Sprintf("payvro-journal-test-%d-%d", windows.GetCurrentProcessId(), id))
	t.Setenv("APPDATA", filepath.Join(root, fmt.Sprintf("payvro-test-roaming-empty-%d-%d", windows.GetCurrentProcessId(), id)))
	t.Cleanup(func() { _ = removeWindowsTestTree(dir) })
	return filepath.Join(dir, "direct-charges.jsonl")
}

func TestWindowsDefaultJournalUsesLocalAppDataAndDetectsRoamingHistory(t *testing.T) {
	local := filepath.Clean(getenvRequired(t, "LOCALAPPDATA"))
	t.Setenv("APPDATA", filepath.Join(local, "payvro-test-roaming-empty"))
	got, err := resolveJournalPath("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(getenvRequired(t, "LOCALAPPDATA"), "payvro", "direct-charges.jsonl")
	if !strings.EqualFold(got, want) {
		t.Fatalf("default journal=%q, want LOCALAPPDATA path %q", got, want)
	}

	legacyRoot := filepath.Join(filepath.Clean(getenvRequired(t, "LOCALAPPDATA")), fmt.Sprintf("payvro-legacy-test-%d-%d", windows.GetCurrentProcessId(), windowsTestID.Add(1)))
	if err := createPlainTestDirectory(legacyRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeWindowsTestTree(legacyRoot) })
	legacy := filepath.Join(legacyRoot, "payvro", "direct-charges.jsonl")
	if err := createPlainTestDirectory(filepath.Dir(legacy)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy history\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APPDATA", legacyRoot)
	if _, err := resolveJournalPath(""); !errors.Is(err, ErrJournalUnavailable) {
		t.Fatalf("existing roaming history was not surfaced as a migration conflict: %v", err)
	}
}

func TestWindowsJournalRejectsAmbiguousOrNonLocalPaths(t *testing.T) {
	root := filepath.Clean(getenvRequired(t, "LOCALAPPDATA"))
	t.Setenv("APPDATA", filepath.Join(root, "payvro-test-roaming-empty-paths"))
	cases := []string{
		`relative\journal.jsonl`,
		`\\server\share\journal.jsonl`,
		`\\?\C:\journal.jsonl`,
		`\\.\C:\journal.jsonl`,
		filepath.Join(root, "..", "Roaming", "journal.jsonl"),
		filepath.Join(root, "journal.jsonl:stream"),
		filepath.Join(root, "CON.jsonl"),
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			if _, err := resolveJournalPath(path); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("unsafe path %q returned %v", path, err)
			}
		})
	}
}

func TestWindowsJournalDirectDemoReplayRestartAndReadOnlyStatus(t *testing.T) {
	path := windowsTestJournalPath(t)
	var providerCalls atomic.Int32
	cfg := Config{Mode: "DEMO", MaxAmount: 14900, APIKeyID: "windows-test-account", JournalPath: path, Transport: windowsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("DEMO must not contact pey")
	})}
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	charge, err := first.CreateCharge(context.Background(), 14900, "native windows", "qr", windowsTestIdem)
	if err != nil || charge.Status != "DEMO_ONLY" || charge.Mode != "DEMO" {
		t.Fatalf("native DEMO create=%+v err=%v", charge, err)
	}

	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := second.CreateCharge(context.Background(), 14900, "native windows", "qr", windowsTestIdem)
	if err != nil || replay.ChargeID != charge.ChargeID {
		t.Fatalf("restart replay=%+v err=%v; original=%+v", replay, err, charge)
	}
	status, err := second.GetChargeStatus(context.Background(), windowsTestIdem)
	if err != nil || status.Status != "DEMO_ONLY" || status.ChargeID != charge.ChargeID {
		t.Fatalf("read-only status=%+v err=%v", status, err)
	}
	if providerCalls.Load() != 0 {
		t.Fatalf("DEMO made %d provider calls", providerCalls.Load())
	}
	if _, err := os.Lstat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock remained after operations: %v", err)
	}

	user, owner, err := processOwner()
	if err != nil {
		t.Fatal(err)
	}
	dirHandle, err := openDirectoryHandle(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(dirHandle)
	if err := validatePrivateHandle(dirHandle, owner, true); err != nil {
		t.Fatalf("created directory is not owner-only: %v", err)
	}
	fileHandle, _, err := openWindowsJournalFile(&journalDirectory{path: filepath.Dir(path), owner: owner}, path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(fileHandle)
	if err := validatePrivateHandle(fileHandle, owner, false); err != nil {
		t.Fatalf("created journal is not owner-only: %v", err)
	}
	lines, err := ScanJournal(path)
	runtime.KeepAlive(user)
	if err != nil || len(lines) != 2 {
		t.Fatalf("journal append/replay lines=%d err=%v; want reserve+finish only", len(lines), err)
	}
}

func TestWindowsHostileDirectoryAndFileACLStopBeforeProviderPOST(t *testing.T) {
	t.Run("inherited shared directory", func(t *testing.T) {
		path := windowsTestJournalPath(t)
		if err := createPlainTestDirectory(filepath.Dir(path)); err != nil {
			t.Fatal(err)
		}
		assertWindowsLiveCreateBlocked(t, path)
	})
	t.Run("unexpected Everyone ACE on existing journal", func(t *testing.T) {
		path := windowsTestJournalPath(t)
		dir, err := openJournalDirectory(filepath.Dir(path), true)
		if err != nil {
			t.Fatal(err)
		}
		dir.close()
		ownerUser, owner, err := processOwner()
		if err != nil {
			t.Fatal(err)
		}
		sd, err := windows.SecurityDescriptorFromString("O:" + owner.String() + "D:P(A;;FA;;;" + owner.String() + ")(A;;FA;;;WD)")
		if err != nil {
			t.Fatal(err)
		}
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			t.Fatal(err)
		}
		sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL, windows.FILE_SHARE_READ, &sa, windows.CREATE_NEW, winWriteThrough|winOpenReparse, 0)
		runtime.KeepAlive(ownerUser)
		runtime.KeepAlive(sd)
		if err != nil {
			t.Fatal(err)
		}
		_ = windows.CloseHandle(h)
		assertWindowsLiveCreateBlocked(t, path)
	})
}

func TestWindowsReadOnlyJournalAndFlushFailureNeverReachProvider(t *testing.T) {
	t.Run("readonly journal", func(t *testing.T) {
		path := windowsTestJournalPath(t)
		cfg := Config{Mode: "DEMO", MaxAmount: 14900, APIKeyID: "windows-test-account", JournalPath: path}
		svc, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.CreateCharge(context.Background(), 14900, "seed", "qr", windowsTestIdem); err != nil {
			t.Fatal(err)
		}
		p, _ := windows.UTF16PtrFromString(path)
		if err := windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_READONLY); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.GetChargeStatus(context.Background(), windowsTestIdem); !errors.Is(err, ErrJournalUnavailable) {
			t.Fatalf("read-only status err=%v; want fail closed", err)
		}
		var posts atomic.Int32
		live, err := New(Config{Mode: "LIVE", MaxAmount: 14900, APIKey: windowsTestKey, APIKeyID: "windows-test-account", JournalPath: path, Transport: windowsRoundTripFunc(func(*http.Request) (*http.Response, error) {
			posts.Add(1)
			return nil, errors.New("unexpected provider request")
		})})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := live.CreateCharge(context.Background(), 100, "readonly", "qr", "windows-readonly-0001"); !errors.Is(err, ErrJournalUnavailable) {
			t.Fatalf("readonly journal create error=%v", err)
		}
		if posts.Load() != 0 {
			t.Fatalf("readonly journal reached provider %d times", posts.Load())
		}
		attrs, err := windows.GetFileAttributes(p)
		if err != nil || attrs&windows.FILE_ATTRIBUTE_READONLY == 0 {
			t.Fatalf("read-only metadata changed: attrs=%#x err=%v", attrs, err)
		}
	})

	t.Run("FlushFileBuffers error", func(t *testing.T) {
		path := windowsTestJournalPath(t)
		oldFlush := journalWindowsFlush
		journalWindowsFlush = func(windows.Handle) error { return errors.New("synthetic flush failure") }
		defer func() { journalWindowsFlush = oldFlush }()
		var posts atomic.Int32
		live, err := New(Config{Mode: "LIVE", MaxAmount: 14900, APIKey: windowsTestKey, APIKeyID: "windows-test-account", JournalPath: path, Transport: windowsRoundTripFunc(func(*http.Request) (*http.Response, error) {
			posts.Add(1)
			return nil, errors.New("unexpected provider request")
		})})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := live.CreateCharge(context.Background(), 100, "flush failure", "qr", "windows-flush-0001"); !errors.Is(err, ErrJournalUnavailable) {
			t.Fatalf("flush failure create error=%v", err)
		}
		if posts.Load() != 0 {
			t.Fatalf("flush failure reached provider %d times", posts.Load())
		}
	})
}

func TestWindowsConcurrentSameIntentUsesSingleJournalReservation(t *testing.T) {
	path := windowsTestJournalPath(t)
	cfg := Config{Mode: "DEMO", MaxAmount: 14900, APIKeyID: "windows-test-account", JournalPath: path}
	const workers = 8
	charges := make(chan Charge, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			svc, err := New(cfg)
			if err != nil {
				errs <- err
				return
			}
			charge, err := svc.CreateCharge(context.Background(), 14900, "native concurrency", "qr", "windows-concurrent-0001")
			charges <- charge
			errs <- err
		}()
	}
	var wantID string
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent create: %v", err)
		}
		charge := <-charges
		if charge.Status != "DEMO_ONLY" || wantID != "" && charge.ChargeID != wantID {
			t.Fatalf("concurrent create returned conflicting charge: %+v, want ID %q", charge, wantID)
		}
		wantID = charge.ChargeID
	}
	lines, err := ScanJournal(path)
	if err != nil || len(lines) != 2 {
		t.Fatalf("concurrent journal lines=%d err=%v; want one reservation and one finish", len(lines), err)
	}
}

func TestWindowsReparseJournalDirectoryIsRejected(t *testing.T) {
	targetPath := windowsTestJournalPath(t)
	targetDir := filepath.Dir(targetPath)
	if _, err := openJournalDirectory(targetDir, true); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(filepath.Dir(targetDir), filepath.Base(targetDir)+"-junction")
	target, err := windows.UTF16PtrFromString(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	link, err := windows.UTF16PtrFromString(linkDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.CreateSymbolicLink(link, target, windows.SYMBOLIC_LINK_FLAG_DIRECTORY); err != nil {
		t.Skipf("Windows symlink creation unavailable for this account/policy: %v", err)
	}
	t.Cleanup(func() { _ = removeWindowsTestTree(linkDir) })
	journalPath := filepath.Join(linkDir, "direct-charges.jsonl")
	svc, err := New(Config{Mode: "DEMO", MaxAmount: 14900, APIKeyID: "windows-test-account", JournalPath: journalPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateCharge(context.Background(), 14900, "reparse", "qr", "windows-reparse-0001"); !errors.Is(err, ErrJournalUnavailable) {
		t.Fatalf("reparse path create error=%v", err)
	}
}

func assertWindowsLiveCreateBlocked(t *testing.T, path string) {
	t.Helper()
	var posts atomic.Int32
	svc, err := New(Config{Mode: "LIVE", MaxAmount: 14900, APIKey: windowsTestKey, APIKeyID: "windows-test-account", JournalPath: path, Transport: windowsRoundTripFunc(func(*http.Request) (*http.Response, error) {
		posts.Add(1)
		return nil, errors.New("unexpected provider request")
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateCharge(context.Background(), 100, "hostile ACL", "qr", "windows-hostile-acl-0001"); !errors.Is(err, ErrJournalUnavailable) {
		t.Fatalf("unsafe journal create error=%v", err)
	}
	if posts.Load() != 0 {
		t.Fatalf("unsafe journal reached provider %d times", posts.Load())
	}
}

func getenvRequired(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("%s is required for a native Windows journal test", key)
	}
	return value
}

func createPlainTestDirectory(path string) error {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.CreateDirectory(ptr, nil)
}

func removeWindowsTestTree(path string) error {
	_ = filepath.WalkDir(path, func(p string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			if ptr, ptrErr := windows.UTF16PtrFromString(p); ptrErr == nil {
				_ = windows.SetFileAttributes(ptr, windows.FILE_ATTRIBUTE_NORMAL)
			}
		}
		return nil
	})
	return os.RemoveAll(path)
}
