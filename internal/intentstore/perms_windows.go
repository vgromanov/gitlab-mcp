//go:build windows

package intentstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func checkDirAccess(path string, _ os.FileInfo) error {
	return verifyOwnerOnly(path, true)
}

func checkFileAccess(path string, _ os.FileInfo) error {
	return verifyOwnerOnly(path, false)
}

func establishPrivate(path string, dir bool) error {
	if err := setOwnerOnly(path, dir); err != nil {
		return ErrUnsafePermissions
	}
	return verifyOwnerOnly(path, dir)
}

// rejectSymlinkComponents refuses a symlink in any existing component.
// Lstat is applied to each component, so an intermediate reparse point is
// the final component of that lookup and is not followed.
func rejectSymlinkComponents(path string) error {
	clean := filepath.Clean(path)
	for _, acc := range windowsPathPrefixes(clean) {
		info, err := os.Lstat(acc)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if isSymlink(info) {
			return ErrSymlink
		}
	}
	return nil
}

func createExclusive(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func identifyFile(path string) (fileID, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileID{}, err
	}
	if isSymlink(info) {
		return fileID{}, ErrSymlink
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fileID{}, err
	}
	h, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return fileID{}, err
	}
	defer windows.CloseHandle(h)
	var infoHandle windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &infoHandle); err != nil {
		return fileID{}, err
	}
	if infoHandle.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fileID{}, ErrSymlink
	}
	return fileID{
		dev: uint64(infoHandle.VolumeSerialNumber),
		ino: uint64(infoHandle.FileIndexHigh)<<32 | uint64(infoHandle.FileIndexLow),
		ok:  true,
	}, nil
}

// makeParents creates the missing components of path one at a time. Each
// component is opened with FILE_FLAG_OPEN_REPARSE_POINT and rejected if it is
// a reparse point, and the handle is held without FILE_SHARE_DELETE so the
// validated name cannot be renamed away and replaced by a junction while the
// next component is created beneath it. New directories are created with an
// owner-only descriptor, so no ACL is ever applied to a pre-existing path.
func makeParents(path string) error {
	var pins []windows.Handle
	defer func() {
		for _, h := range pins {
			windows.CloseHandle(h)
		}
	}()
	for _, acc := range windowsPathPrefixes(filepath.Clean(path)) {
		h, err := openDirNoReparse(acc)
		created := false
		if err != nil && errors.Is(err, os.ErrNotExist) {
			if err := createPrivateDir(acc); err != nil {
				return err
			}
			created = true
			h, err = openDirNoReparse(acc)
		}
		if err != nil {
			return err
		}
		pins = append(pins, h)
		if created {
			if err := verifyOwnerOnly(acc, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func openDirNoReparse(path string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(
		name,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return 0, ErrSymlink
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		windows.CloseHandle(h)
		return 0, ErrUnsafePermissions
	}
	return h, nil
}

// createPrivateDir creates a single directory with a protected owner-only
// DACL set atomically at creation. CreateDirectory does not follow a reparse
// point at the final component and fails if the name already exists.
func createPrivateDir(path string) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	str := sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + str + "D:P(A;OICI;GA;;;" + str + ")")
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(name, sa)
	runtime.KeepAlive(sd)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

func currentUserSID() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	sid, err := user.User.Sid.Copy()
	runtime.KeepAlive(user)
	return sid, err
}

func setOwnerOnly(path string, dir bool) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	inherit := uint32(windows.NO_INHERITANCE)
	if dir {
		inherit = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	var pin runtime.Pinner
	pin.Pin(sid)
	defer pin.Unpin()
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inherit,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		sid,
		nil,
		dacl,
		nil,
	)
}

func verifyOwnerOnly(path string, dir bool) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		return ErrUnsafePermissions
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PRESENT == 0 || control&windows.SE_DACL_PROTECTED == 0 {
		return ErrUnsafePermissions
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return ErrUnsafePermissions
	}
	user, err := currentUserSID()
	if err != nil {
		return ErrUnsafePermissions
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		return ErrUnsafePermissions
	}
	canWrite := false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return ErrUnsafePermissions
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if !sid.Equals(user) {
				return ErrUnsafePermissions
			}
			if ace.Mask&windowsWriteMask != 0 {
				canWrite = true
			}
		case windows.ACCESS_DENIED_ACE_TYPE:
		default:
			return ErrUnsafePermissions
		}
	}
	if canWrite {
		return nil
	}
	if dir {
		return ErrUnsafePermissions
	}
	return ErrReadOnly
}

const windowsWriteMask = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_GENERIC_WRITE | windows.GENERIC_WRITE | windows.GENERIC_ALL
