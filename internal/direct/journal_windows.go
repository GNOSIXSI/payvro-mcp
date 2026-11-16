//go:build windows

package direct

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type journal struct{ path string }

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

var (
	errReferenceCollision = errors.New("local QR reference collision")
	// Kept as an indirection so the caller's fail-closed behavior can be exercised
	// when the OS reports a flush failure; production always uses FlushFileBuffers.
	journalWindowsFlush = windows.FlushFileBuffers
)

const (
	winWriteThrough    = 0x80000000
	winOpenReparse     = 0x00200000
	winBackupSemantics = 0x02000000
	winAttrTagInfo     = 9
	winFileAllAccess   = windows.ACCESS_MASK(0x001F01FF)
	winMaxJournalSize  = uint64(maxJournalBytes)
)

type fileAttributeTagInfo struct {
	Attributes uint32
	ReparseTag uint32
}

type journalDirectory struct {
	path      string
	handles   []windows.Handle
	owner     *windows.SID
	tokenUser *windows.Tokenuser
}

func (d *journalDirectory) close() {
	for i := len(d.handles) - 1; i >= 0; i-- {
		_ = windows.CloseHandle(d.handles[i])
	}
	d.handles = nil
	runtime.KeepAlive(d.tokenUser)
}

type journalLock struct {
	handle windows.Handle
}

func (l *journalLock) release() error {
	if l == nil || l.handle == windows.InvalidHandle {
		return nil
	}
	// Delete by the exact handle acquired with CREATE_NEW. Never unlink a path
	// after closing and risk removing a replacement lock owned by another run.
	disposition := struct{ DeleteFile byte }{DeleteFile: 1}
	err := windows.SetFileInformationByHandle(l.handle, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&disposition)), uint32(unsafe.Sizeof(disposition)))
	closeErr := windows.CloseHandle(l.handle)
	l.handle = windows.InvalidHandle
	if err != nil {
		return err
	}
	return closeErr
}

func processOwner() (*windows.Tokenuser, *windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, nil, ErrJournalUnavailable
	}
	return user, user.User.Sid, nil
}

func ownerOnlyDescriptor(owner *windows.SID) (*windows.SECURITY_DESCRIPTOR, error) {
	if owner == nil || !owner.IsValid() {
		return nil, ErrJournalUnavailable
	}
	sid := owner.String()
	if sid == "" {
		return nil, ErrJournalUnavailable
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;;FA;;;" + sid + ")")
	if err != nil || sd == nil || !validOwnerOnlyDescriptor(sd, owner) {
		return nil, ErrJournalUnavailable
	}
	return sd, nil
}

func validOwnerOnlyDescriptor(sd *windows.SECURITY_DESCRIPTOR, owner *windows.SID) bool {
	if sd == nil || owner == nil || !owner.IsValid() {
		return false
	}
	actualOwner, ownerDefaulted, err := sd.Owner()
	if err != nil || ownerDefaulted || actualOwner == nil || !actualOwner.Equals(owner) {
		return false
	}
	dacl, daclDefaulted, err := sd.DACL()
	if err != nil || daclDefaulted || dacl == nil || dacl.AceCount != 1 {
		return false
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PRESENT == 0 || control&windows.SE_DACL_PROTECTED == 0 || control&(windows.SE_DACL_AUTO_INHERITED|windows.SE_DACL_AUTO_INHERIT_REQ) != 0 {
		return false
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if windows.GetAce(dacl, 0, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != winFileAllAccess || ace.Header.AceSize < 16 {
		return false
	}
	aclSize := *(*uint16)(unsafe.Add(unsafe.Pointer(dacl), 2))
	if aclSize < 8 || ace.Header.AceSize > aclSize-8 || int(ace.Header.AceSize) != 8+owner.Len() {
		return false
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSID.IsValid() || aceSID.Len()+8 != int(ace.Header.AceSize) || !aceSID.Equals(owner) {
		return false
	}
	runtime.KeepAlive(sd)
	return true
}

func openDirectoryChain(path string) ([]windows.Handle, *windows.Tokenuser, *windows.SID, error) {
	if !validWindowsLocalPath(path) {
		return nil, nil, nil, ErrJournalUnavailable
	}
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	root := volume + `\`
	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil || windows.GetDriveType(rootPtr) != windows.DRIVE_FIXED {
		return nil, nil, nil, ErrJournalUnavailable
	}
	user, owner, err := processOwner()
	if err != nil {
		return nil, nil, nil, err
	}
	parts, err := filepath.Rel(root, clean)
	if err != nil || parts == "." || filepath.IsAbs(parts) {
		return nil, user, owner, ErrJournalUnavailable
	}
	handles := make([]windows.Handle, 0, strings.Count(parts, `\`)+2)
	fail := func(err error) ([]windows.Handle, *windows.Tokenuser, *windows.SID, error) {
		for i := len(handles) - 1; i >= 0; i-- {
			_ = windows.CloseHandle(handles[i])
		}
		runtime.KeepAlive(user)
		return nil, nil, nil, err
	}
	rootHandle, err := openDirectoryHandle(root)
	if err != nil {
		return fail(err)
	}
	handles = append(handles, rootHandle)
	if err := requireLocalACLVolume(rootHandle); err != nil {
		return fail(err)
	}
	current := root
	for _, component := range strings.Split(parts, `\`) {
		if component == "" || component == "." || component == ".." {
			return fail(ErrJournalUnavailable)
		}
		current = filepath.Join(current, component)
		h, err := openDirectoryHandle(current)
		if err != nil {
			return fail(err)
		}
		handles = append(handles, h)
	}
	runtime.KeepAlive(user)
	return handles, user, owner, nil
}

func openDirectoryHandle(path string) (windows.Handle, error) {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, ErrJournalUnavailable
	}
	h, err := windows.CreateFile(ptr, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES|windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, winOpenReparse|winBackupSemantics, 0)
	if err != nil {
		return windows.InvalidHandle, err
	}
	if err := validateDirectoryHandle(h); err != nil {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, err
	}
	return h, nil
}

func requireLocalACLVolume(h windows.Handle) error {
	var fsName [32]uint16
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, &flags, &fsName[0], uint32(len(fsName))); err != nil {
		return ErrJournalUnavailable
	}
	fs := strings.ToUpper(windows.UTF16ToString(fsName[:]))
	if fs != "NTFS" || flags&windows.FILE_PERSISTENT_ACLS == 0 {
		return ErrJournalUnavailable
	}
	return nil
}

func validateDirectoryHandle(h windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrJournalUnavailable
	}
	var tag fileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(h, winAttrTagInfo, (*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err != nil || tag.Attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || tag.Attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return ErrJournalUnavailable
	}
	return nil
}

func openJournalDirectory(path string, create bool) (*journalDirectory, error) {
	if !validWindowsLocalPath(path) {
		return nil, ErrJournalUnavailable
	}
	parentPath := filepath.Dir(path)
	parentHandles, user, owner, err := openDirectoryChain(parentPath)
	if err != nil {
		if !create && isWindowsPathMissing(err) {
			return nil, nil
		}
		return nil, ErrJournalUnavailable
	}
	d := &journalDirectory{path: filepath.Clean(path), handles: parentHandles, owner: owner, tokenUser: user}
	if create {
		sd, sdErr := ownerOnlyDescriptor(owner)
		if sdErr != nil {
			d.close()
			return nil, ErrJournalUnavailable
		}
		ptr, ptrErr := windows.UTF16PtrFromString(d.path)
		if ptrErr != nil {
			d.close()
			return nil, ErrJournalUnavailable
		}
		sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
		createErr := windows.CreateDirectory(ptr, &sa)
		runtime.KeepAlive(sd)
		if createErr != nil && !errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) && !errors.Is(createErr, windows.ERROR_FILE_EXISTS) {
			d.close()
			return nil, ErrJournalUnavailable
		}
	}
	leaf, err := openDirectoryHandle(d.path)
	if err != nil {
		d.close()
		if !create && isWindowsPathMissing(err) {
			return nil, nil
		}
		return nil, ErrJournalUnavailable
	}
	d.handles = append(d.handles, leaf)
	if err := validatePrivateHandle(leaf, owner, true); err != nil {
		d.close()
		return nil, ErrJournalUnavailable
	}
	return d, nil
}

func isWindowsPathMissing(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func validatePrivateHandle(h windows.Handle, owner *windows.SID, directory bool) error {
	var attrs fileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(h, winAttrTagInfo, (*byte)(unsafe.Pointer(&attrs)), uint32(unsafe.Sizeof(attrs))); err != nil || attrs.Attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || attrs.Attributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		return ErrJournalUnavailable
	}
	if directory {
		if attrs.Attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			return ErrJournalUnavailable
		}
	} else if attrs.Attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return ErrJournalUnavailable
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil || info.NumberOfLinks != 1 {
		return ErrJournalUnavailable
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil || !validOwnerOnlyDescriptor(sd, owner) {
		return ErrJournalUnavailable
	}
	runtime.KeepAlive(sd)
	return nil
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
	path, err := filepath.Abs(j.path)
	if err != nil || !validWindowsLocalPath(path) {
		return journalEvent{}, false, ErrJournalUnavailable
	}
	d, err := openJournalDirectory(filepath.Dir(path), false)
	if err != nil {
		return journalEvent{}, false, ErrJournalUnavailable
	}
	if d == nil {
		return journalEvent{}, false, nil
	}
	defer d.close()
	events, err := readJournalInDirectory(d, path)
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

func (j *journal) locked(change func([]journalEvent) ([]journalEvent, error), write bool) (resultErr error) {
	if j.path == "" {
		return ErrJournalUnavailable
	}
	path, err := filepath.Abs(j.path)
	if err != nil || !validWindowsLocalPath(path) {
		return ErrJournalUnavailable
	}
	d, err := openJournalDirectory(filepath.Dir(path), true)
	if err != nil || d == nil {
		return ErrJournalUnavailable
	}
	defer d.close()
	lock, err := acquireWindowsLock(d, path+".lock")
	if err != nil {
		return ErrJournalUnavailable
	}
	defer func() {
		if releaseErr := lock.release(); releaseErr != nil {
			resultErr = ErrJournalUnavailable
		}
	}()
	events, err := readJournalInDirectory(d, path)
	if err != nil {
		return ErrJournalUnavailable
	}
	updated, err := change(events)
	if err != nil {
		return err
	}
	if write && len(updated) > len(events) {
		if err := appendWindowsEvent(d, path, updated[len(updated)-1], len(events)); err != nil {
			return ErrJournalUnavailable
		}
	}
	return nil
}

func acquireWindowsLock(d *journalDirectory, path string) (*journalLock, error) {
	sd, err := ownerOnlyDescriptor(d.owner)
	if err != nil {
		return nil, err
	}
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	deadline := time.Now().Add(3 * time.Second)
	for {
		h, createErr := windows.CreateFile(ptr, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE|windows.READ_CONTROL, 0, &sa, windows.CREATE_NEW, winWriteThrough|winOpenReparse, 0)
		if createErr == nil {
			runtime.KeepAlive(sd)
			lock := &journalLock{handle: h}
			if err := validatePrivateHandle(h, d.owner, false); err != nil {
				_ = lock.release()
				return nil, err
			}
			if err := writeWindowsHandle(h, []byte(fmt.Sprintf("%d\n", os.Getpid()))); err != nil {
				_ = lock.release()
				return nil, err
			}
			if err := journalWindowsFlush(h); err != nil {
				_ = lock.release()
				return nil, err
			}
			return lock, nil
		}
		runtime.KeepAlive(sd)
		if !errors.Is(createErr, windows.ERROR_FILE_EXISTS) && !errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) && !errors.Is(createErr, windows.ERROR_SHARING_VIOLATION) {
			return nil, createErr
		}
		if time.Now().After(deadline) {
			return nil, createErr
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readJournal(path string) ([]journalEvent, error) {
	abs, err := filepath.Abs(path)
	if err != nil || !validWindowsLocalPath(abs) {
		return nil, ErrJournalUnavailable
	}
	d, err := openJournalDirectory(filepath.Dir(abs), false)
	if err != nil {
		return nil, ErrJournalUnavailable
	}
	if d == nil {
		return nil, nil
	}
	defer d.close()
	return readJournalInDirectory(d, abs)
}

func readJournalInDirectory(d *journalDirectory, path string) ([]journalEvent, error) {
	h, _, err := openWindowsJournalFile(d, path, false, false)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return nil, ErrJournalUnavailable
	}
	size := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	if size > winMaxJournalSize || size > uint64(math.MaxInt) {
		return nil, ErrJournalUnavailable
	}
	data := make([]byte, int(size))
	if len(data) > 0 {
		var read uint32
		for read < uint32(len(data)) {
			var n uint32
			err := windows.ReadFile(h, data[read:], &n, nil)
			if err != nil || n == 0 {
				return nil, ErrJournalUnavailable
			}
			read += n
		}
		if data[len(data)-1] != '\n' {
			return nil, ErrJournalUnavailable
		}
	}
	return parseJournal(data)
}

func openWindowsJournalFile(d *journalDirectory, path string, create, write bool) (windows.Handle, bool, error) {
	if !strings.EqualFold(filepath.Dir(filepath.Clean(path)), d.path) {
		return windows.InvalidHandle, false, ErrJournalUnavailable
	}
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, false, ErrJournalUnavailable
	}
	access := uint32(windows.GENERIC_READ | windows.READ_CONTROL)
	flags := uint32(winOpenReparse)
	if write {
		// FlushFileBuffers requires GENERIC_WRITE. The exclusive journal lock
		// serializes this seek-to-end and write sequence across processes.
		access |= windows.GENERIC_WRITE
		flags |= winWriteThrough
	}
	h, err := windows.CreateFile(ptr, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, flags, 0)
	if err == nil {
		if err := validatePrivateHandle(h, d.owner, false); err != nil {
			_ = windows.CloseHandle(h)
			return windows.InvalidHandle, false, err
		}
		return h, false, nil
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || !create || !write {
		return windows.InvalidHandle, false, err
	}
	sd, err := ownerOnlyDescriptor(d.owner)
	if err != nil {
		return windows.InvalidHandle, false, err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err = windows.CreateFile(ptr, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &sa, windows.CREATE_NEW, flags, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		return windows.InvalidHandle, false, err
	}
	if err := validatePrivateHandle(h, d.owner, false); err != nil {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, false, err
	}
	return h, true, nil
}

func appendWindowsEvent(d *journalDirectory, path string, e journalEvent, count int) error {
	if count >= maxJournalEntries {
		return ErrJournalUnavailable
	}
	encoded, err := json.Marshal(e)
	if err != nil || len(encoded) > 16*1024 {
		return ErrJournalUnavailable
	}
	h, created, err := openWindowsJournalFile(d, path, true, true)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	size := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	if size+uint64(len(encoded))+1 > winMaxJournalSize {
		return ErrJournalUnavailable
	}
	if created && size != 0 {
		return ErrJournalUnavailable
	}
	if !created && size > 0 {
		// A pre-existing partial/corrupt line must never be appended over.
		if _, err := readJournalInDirectory(d, path); err != nil {
			return ErrJournalUnavailable
		}
	}
	if _, err := windows.Seek(h, 0, io.SeekEnd); err != nil {
		return err
	}
	line := append(encoded, '\n')
	if err := writeWindowsHandle(h, line); err != nil {
		return err
	}
	if err := journalWindowsFlush(h); err != nil {
		return err
	}
	return nil
}

func writeWindowsHandle(h windows.Handle, data []byte) error {
	for len(data) > 0 {
		var n uint32
		err := windows.WriteFile(h, data, &n, nil)
		if err != nil || n == 0 {
			if err != nil {
				return err
			}
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func parseJournal(data []byte) ([]journalEvent, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != '\n' {
		return nil, ErrJournalUnavailable
	}
	s := bufio.NewScanner(strings.NewReader(string(data)))
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

func validJournalState(state string) bool {
	switch state {
	case "reserved", "created", "uncertain", "rejected":
		return true
	}
	return false
}
