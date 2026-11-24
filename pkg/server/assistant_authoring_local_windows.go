package server

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"github.com/SocialGouv/iterion/pkg/authoringlock"
	"golang.org/x/sys/windows"
)

func renameAuthoringAt(from *os.File, fromName string, to *os.File, toName string) error {
	return renameAuthoringAtReplacing(from, fromName, to, toName, false)
}

// replaceAuthoringAt renames fromName over toName inside dir, replacing it in
// one step — the one authoring rename that replaces (replaceFile).
func replaceAuthoringAt(dir *os.File, fromName, toName string) error {
	return renameAuthoringAtReplacing(dir, fromName, dir, toName, true)
}

func renameAuthoringAtReplacing(from *os.File, fromName string, to *os.File, toName string, replace bool) error {
	if toName == "" || toName == "." || toName == ".." || strings.ContainsAny(toName, "/\\:") {
		return os.ErrInvalid
	}
	// Open the source object itself. Reparse objects may be displaced and
	// retained, but are never followed or accepted as regular candidate bytes.
	source, err := authoringlock.WindowsOpenAt(from, fromName, windows.DELETE, windows.FILE_OPEN, windows.FILE_OPEN_FOR_BACKUP_INTENT, false)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	// FILE_RENAME_INFORMATION's first union is ReplaceIfExists: zero (no
	// replace) unless the caller replaces. Both supported architectures align
	// HANDLE to 8 bytes. Native NT relative rename keeps the destination
	// bound to its verified parent handle.
	info := struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [256]uint16
	}{RootDirectory: windows.Handle(to.Fd())}
	if replace {
		info.Flags = 1
	}
	name, err := windows.UTF16FromString(toName)
	if err != nil {
		return err
	}
	if len(name) > len(info.FileName) {
		return os.ErrInvalid
	}
	copy(info.FileName[:], name)
	info.FileNameLength = uint32((len(name) - 1) * 2)
	err = windows.NtSetInformationFile(windows.Handle(source.Fd()), &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Offsetof(info.FileName))+info.FileNameLength, windows.FileRenameInformation)
	return authoringlock.WindowsErrno(err)
}
func requireAuthoringSameFilesystem(a, b *os.File) error {
	var left, right windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(a.Fd()), &left); err != nil {
		return err
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(b.Fd()), &right); err != nil {
		return err
	}
	if left.VolumeSerialNumber != right.VolumeSerialNumber {
		return fmt.Errorf("authoring recovery storage must share the destination filesystem: %w", windows.ERROR_NOT_SAME_DEVICE)
	}
	return nil
}

// authoringFileLinks is how many names the open file has (its link count).
func authoringFileLinks(f *os.File) (uint64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.NumberOfLinks), nil
}
