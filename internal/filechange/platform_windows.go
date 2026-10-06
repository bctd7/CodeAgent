//go:build windows

package filechange

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func platformMode(mode fs.FileMode) fs.FileMode {
	if mode&0200 != 0 {
		return 0666
	}
	return 0444
}

func readFlags() int { return os.O_RDONLY }

func reparse(info fs.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func fileIdentity(f *os.File) (string, uint64, error) {
	return handleIdentity(windows.Handle(f.Fd()))
}

func handleIdentity(h windows.Handle) (string, uint64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return "", 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "", 0, fmt.Errorf("%w: reparse point", ErrConflict)
	}
	id := fmt.Sprintf("%08x:%08x%08x:%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow, info.CreationTime.HighDateTime, info.CreationTime.LowDateTime)
	return id, uint64(info.NumberOfLinks), nil
}

func privateDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	// 禁止继承父目录的宽权限；只允许当前用户及 SYSTEM，并让新文件继承。
	return windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
}

func privateDirectory(path string) error {
	sd, err := privateDescriptor()
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	err = windows.CreateDirectory(name, &sa)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	if err := checkDirectoryPath(path); err != nil {
		return err
	}
	return setPrivateACL(path, sd)
}

func setPrivateACL(path string, sd *windows.SECURITY_DESCRIPTOR) error {
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

func privateFile(f *os.File) error {
	sd, err := privateDescriptor()
	if err != nil {
		return err
	}
	return setPrivateACL(f.Name(), sd)
}

func lockJournal(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrBusy
	}
	return err
}

// Windows 不对目录调用 FlushFileBuffers；文件在替换前已 Sync。
// 这保证正常进程退出后的重开，不承诺断电或所有文件系统上的重命名原子性。
func syncDirectory(*os.Root) error { return nil }

// 以父目录句柄打开非目录目标并按句柄删除，不使用也能删除目录的 os.Remove。
func removeFile(parent *os.Root, name, expectedID string) error {
	handle, err := openMutationFile(parent, name, expectedID)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE)
	return windows.NtSetInformationFile(handle, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)), windows.FileDispositionInformationEx)
}

// 只读属性与内容一并受前置条件保护；使用系统标志覆盖只读目标，不提前 chmod 用户文件。
func replaceFile(parent *os.Root, from, to, expectedID string) error {
	if filepath.Base(to) != to || to == "." || to == ".." {
		return ErrInvalid
	}
	handle, err := openMutationFile(parent, from, expectedID)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	dir, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	name, err := windows.UTF16FromString(to)
	if err != nil || len(name) > 256 {
		return fmt.Errorf("%w: replacement file name", ErrInvalid)
	}
	// FILE_RENAME_INFORMATION 的原生布局；这里只接收单个文件名，不拼接绝对路径。
	info := struct {
		Flags     uint32
		Parent    windows.Handle
		NameBytes uint32
		Name      [256]uint16
	}{Flags: windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS | windows.FILE_RENAME_IGNORE_READONLY_ATTRIBUTE, Parent: windows.Handle(dir.Fd()), NameBytes: uint32((len(name) - 1) * 2)}
	copy(info.Name[:], name)
	const fileRenameInformationEx = 65
	return windows.NtSetInformationFile(handle, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), fileRenameInformationEx)
}

func openMutationFile(parent *os.Root, name, expectedID string) (windows.Handle, error) {
	if filepath.Base(name) != name || name == "." || name == ".." || expectedID == "" {
		return 0, ErrInvalid
	}
	dir, err := parent.Open(".")
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(dir.Fd()), ObjectName: objectName}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, &attrs, &status, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return 0, err
	}
	id, links, err := handleIdentity(handle)
	if err != nil {
		windows.CloseHandle(handle)
		return 0, err
	}
	if id != expectedID || links != 1 {
		windows.CloseHandle(handle)
		return 0, fmt.Errorf("%w: mutation target identity changed", ErrConflict)
	}
	return handle, nil
}
