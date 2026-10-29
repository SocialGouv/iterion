package authoringlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WindowsErrno unwraps an NT status into the errno callers compare against
// (os.ErrExist, os.ErrNotExist). It is exported for the other holder of the
// same primitives: the server's publication renames.
func WindowsErrno(err error) error {
	var status windows.NTStatus
	if errors.As(err, &status) {
		return status.Errno()
	}
	return err
}

// OpenDirectory opens a directory without sharing DELETE: on Windows this
// pins the directory against rename and removal while the handle is held.
// OPEN_REPARSE_POINT prevents following a substituted junction at the leaf;
// identity is checked afterwards. Every child operation is then relative to
// this verified handle, with a single basename and OBJ_DONT_REPARSE: no
// path-based replacement is used.
func OpenDirectory(path string, expected os.FileInfo) (*os.File, error) {
	if expected == nil {
		return nil, fmt.Errorf("%w: %s", ErrDirectoryChanged, path)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	info, err := f.Stat()
	if err != nil || !os.SameFile(expected, info) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		_ = f.Close()
		return nil, errors.Join(err, fmt.Errorf("%w: %s is a reparse point or changed", ErrDirectoryChanged, path))
	}
	return f, nil
}

func windowsPrivateSD() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	// Protected DACL: only this user and SYSTEM, inherited by private children.
	return windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
}

// WindowsOpenAt opens or creates one named child of an opened directory,
// natively relative to its handle. It is exported for the other holder of
// the same primitive: the server's publication renames, which open their
// source object the same way.
func WindowsOpenAt(parent *os.File, name string, access, disposition, options uint32, private bool) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:") {
		return nil, os.ErrInvalid
	}
	ntName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(parent.Fd()), ObjectName: ntName, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	if private {
		attrs.SecurityDescriptor, err = windowsPrivateSD()
		if err != nil {
			return nil, err
		}
	}
	var h windows.Handle
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE|windows.READ_CONTROL, &attrs, &windows.IO_STATUS_BLOCK{}, nil, windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disposition, options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return nil, WindowsErrno(err)
	}
	return os.NewFile(uintptr(h), filepath.Join(parent.Name(), name)), nil
}

// MkdirAt creates a control directory relative to an opened parent, with the
// private descriptor its children inherit.
func MkdirAt(parent *os.File, name string) error {
	f, err := WindowsOpenAt(parent, name, windows.FILE_LIST_DIRECTORY, windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE, true)
	if err != nil {
		return err
	}
	return f.Close()
}

// OpenFileAt opens a control file relative to an opened parent, translating
// the portable flags the contract uses into their NT dispositions.
func OpenFileAt(parent *os.File, name string, flags int, mode os.FileMode) (*os.File, error) {
	access := uint32(windows.FILE_GENERIC_READ)
	if flags&os.O_WRONLY != 0 {
		access = windows.FILE_GENERIC_WRITE
	}
	if flags&os.O_RDWR != 0 {
		access = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE
	}
	disposition := uint32(windows.FILE_OPEN)
	if flags&os.O_CREATE != 0 {
		disposition = windows.FILE_OPEN_IF
	}
	if flags&os.O_EXCL != 0 {
		disposition = windows.FILE_CREATE
	}
	return WindowsOpenAt(parent, name, access, disposition, windows.FILE_NON_DIRECTORY_FILE, mode == privateMode)
}

// ValidateOwnership holds an opened control object to belonging to this user
// and to granting nobody else write access — and, when private, no access at
// all. Its message is the one the server's save answers with.
func ValidateOwnership(f *os.File, private bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if !owner.Equals(user.User.Sid) {
		return fmt.Errorf("authoring directory belongs to another owner: %s", f.Name())
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if acl == nil {
		return errors.New("authoring control has an unrestricted DACL")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("unsupported authoring control ACL entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(user.User.Sid) || sid.Equals(system) || (!private && sid.Equals(admins)) {
			continue
		}
		if private || ace.Mask&(windows.GENERIC_ALL|windows.GENERIC_WRITE|windows.DELETE|windows.WRITE_DAC|windows.WRITE_OWNER|windows.FILE_WRITE_DATA|windows.FILE_APPEND_DATA|0x0040 /* FILE_DELETE_CHILD */) != 0 {
			return errors.New("authoring control ACL grants access to another principal")
		}
	}
	return nil
}

// SyncDirectory is a no-op here: Windows has no supported directory fsync
// through these handles. Each journal and candidate is flushed; namespace
// transitions use native same-volume rename. Records aid recovery after
// interruption, without promising power-loss replay.
func SyncDirectory(*os.File) error { return nil }

// NormalizeName drops the trailing spaces and dots Windows itself strips
// from a name, so two spellings of one file fold to one lock.
func NormalizeName(name string) string { return strings.TrimRight(name, " .") }

// OpenFlags is how the lock file itself is opened.
func OpenFlags() int { return os.O_RDWR }
