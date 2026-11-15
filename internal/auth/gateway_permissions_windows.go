//go:build windows

package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"agentcfg/internal/pathutil"
	"golang.org/x/sys/windows"
)

// CheckGatewayConfigPrivacy verifies DACLs without modifying them. Checking
// parent traversal is insufficient on Windows: the file and atomic-write
// temporary must both be private to the current account, SYSTEM and admins.
func CheckGatewayConfigPrivacy(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve gateway configuration permissions: %w", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("resolve gateway configuration owner: %w", err)
	}
	file, err := os.Lstat(absolute)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect gateway configuration permissions: %w", err)
	}
	if file != nil {
		if !file.Mode().IsRegular() || pathutil.IsRedirected(file) {
			return errors.New("native OAuth gateway configuration must be a regular file, not a redirected path")
		}
		if err := checkGatewayWindowsPathACL(absolute, user.User.Sid, gatewayExistingFile, false); err != nil {
			return err
		}
	}
	nearest := ""
	missingDirectories := false
	for parent := filepath.Dir(absolute); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect gateway configuration parent permissions: %w", err)
		}
		if info == nil {
			missingDirectories = true
		} else {
			if !info.IsDir() || pathutil.IsRedirected(info) {
				return errors.New("native OAuth gateway configuration parent must be a directory, not a redirected path")
			}
			if nearest == "" {
				nearest = parent
			}
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	if nearest == "" {
		return errors.New("native OAuth gateway configuration parent permissions cannot be verified")
	}
	if err := checkGatewayWindowsPathACL(nearest, user.User.Sid, gatewayExistingDirectory, false); err != nil {
		return err
	}
	// Atomic writes create a new file in the parent even when the target exists.
	if err := checkGatewayWindowsPathACL(nearest, user.User.Sid, gatewayInheritedFile, missingDirectories); err != nil {
		return err
	}
	if missingDirectories {
		return checkGatewayWindowsPathACL(nearest, user.User.Sid, gatewayInheritedDirectory, true)
	}
	return nil
}

// CheckGatewayConfigCopyPrivacy checks the inherited DACL of a future copy.
// POSIX file modes do not establish confidentiality on Windows.
func CheckGatewayConfigCopyPrivacy(path string, _ os.FileMode) error {
	return CheckGatewayConfigPrivacy(path)
}

// A rename retains the existing DACLs. Check those files/directories and the
// destination parent, rather than relying on their old private ancestry.
func CheckGatewayConfigMovePrivacy(sourceRoot, destinationRoot, relative string) error {
	file, parents, movedParents, err := gatewayMovePaths(sourceRoot, destinationRoot, relative)
	if err != nil {
		return err
	}
	if err := CheckGatewayConfigPrivacy(file); err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	for _, parent := range parents[:movedParents+1] {
		if err := checkGatewayWindowsPathACL(parent, user.User.Sid, gatewayExistingDirectory, false); err != nil {
			return err
		}
	}
	return nil
}

type gatewayACLUse uint8

const (
	gatewayExistingFile gatewayACLUse = iota
	gatewayExistingDirectory
	gatewayInheritedFile
	gatewayInheritedDirectory
)

func checkGatewayWindowsPathACL(path string, current *windows.SID, use gatewayACLUse, descendant bool) error {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read native OAuth gateway configuration ACL: %w", err)
	}
	defer runtime.KeepAlive(descriptor)
	return checkGatewayWindowsACL(descriptor, current, use, descendant)
}

func gatewayTrustedSID(sid, current *windows.SID, inherited bool) bool {
	return sid != nil && sid.IsValid() && (sid.Equals(current) ||
		sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) ||
		(inherited && sid.IsWellKnown(windows.WinCreatorOwnerSid)))
}

func checkGatewayWindowsACL(descriptor *windows.SECURITY_DESCRIPTOR, current *windows.SID, use gatewayACLUse, descendant bool) error {
	owner, _, err := descriptor.Owner()
	if err != nil || !gatewayTrustedSID(owner, current, false) {
		return errors.New("native OAuth gateway configuration ACL is owned by another account or cannot be verified")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return errors.New("native OAuth gateway configuration requires an explicit private DACL")
	}
	inherited := use == gatewayInheritedFile || use == gatewayInheritedDirectory
	// File data, mutation and control rights are sensitive. Read-control and
	// synchronization alone reveal no URL contents. Unknown ACEs fail closed.
	risk := windows.ACCESS_MASK(windows.GENERIC_ALL | windows.GENERIC_READ | windows.GENERIC_WRITE |
		windows.GENERIC_EXECUTE | windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
		windows.FILE_READ_EA | windows.FILE_WRITE_EA | windows.FILE_EXECUTE | windows.FILE_WRITE_ATTRIBUTES |
		windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.MAXIMUM_ALLOWED)
	if use == gatewayExistingDirectory || use == gatewayInheritedDirectory {
		// Listing/traversal does not expose a private child. Mutating the
		// directory, deleting children or changing its ACL does undermine it.
		risk = windows.ACCESS_MASK(windows.GENERIC_ALL | windows.GENERIC_WRITE | windows.FILE_WRITE_DATA |
			windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES |
			0x40 /* FILE_DELETE_CHILD */ | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.MAXIMUM_ALLOWED)
	}
	inheritanceEstablished := false
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil || ace == nil {
			return errors.New("native OAuth gateway configuration ACL could not be inspected")
		}
		flags := ace.Header.AceFlags
		if inherited {
			flag := uint8(windows.OBJECT_INHERIT_ACE)
			if use == gatewayInheritedDirectory {
				flag = windows.CONTAINER_INHERIT_ACE
			}
			if flags&flag == 0 {
				continue
			}
		} else if flags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			// Ignoring denies is conservative: it may reject an elaborate ACL
			// but never treats a broad allow as proof of confidentiality.
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceSize < uint16(unsafe.Sizeof(*ace)) {
			return errors.New("native OAuth gateway configuration contains an unsupported ACL entry; privacy cannot be verified")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		trusted := gatewayTrustedSID(sid, current, inherited)
		if !trusted && ace.Mask&risk != 0 {
			return errors.New("native OAuth gateway configuration ACL grants access to another account; restrict it before syncing")
		}
		if trusted && (!descendant || flags&windows.NO_PROPAGATE_INHERIT_ACE == 0) {
			inheritanceEstablished = true
		}
	}
	if inherited && !inheritanceEstablished {
		return errors.New("native OAuth gateway configuration parent does not establish private inherited permissions for new files")
	}
	return nil
}
