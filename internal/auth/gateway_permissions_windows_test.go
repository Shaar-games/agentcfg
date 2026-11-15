//go:build windows

package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

func gatewayTestWindowsSID(t *testing.T) *windows.SID {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return user.User.Sid
}

func gatewayTestDescriptor(t *testing.T, current *windows.SID, acl string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString("O:" + current.String() + acl)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func TestGatewayConfigWindowsACLRules(t *testing.T) {
	current := gatewayTestWindowsSID(t)
	self := current.String()
	for _, tc := range []struct {
		name, acl           string
		use                 gatewayACLUse
		descendant, allowed bool
	}{
		{"private-file", "D:P(A;;FA;;;" + self + ")(A;;FA;;;SY)(A;;FA;;;BA)", gatewayExistingFile, false, true},
		{"public-read", "D:P(A;;FA;;;" + self + ")(A;;GR;;;WD)", gatewayExistingFile, false, false},
		{"public-write", "D:P(A;;FA;;;" + self + ")(A;;GW;;;WD)", gatewayExistingFile, false, false},
		{"public-dacl-control", "D:P(A;;FA;;;" + self + ")(A;;WD;;;WD)", gatewayExistingFile, false, false},
		{"metadata-only", "D:P(A;;FA;;;" + self + ")(A;;0x00120000;;;WD)", gatewayExistingFile, false, true},
		{"deny-does-not-certify-broad-allow", "D:P(D;;GR;;;WD)(A;;FA;;;" + self + ")(A;;GR;;;WD)", gatewayExistingFile, false, false},
		{"no-dacl", "", gatewayExistingFile, false, false},
		{"null-dacl", "D:NO_ACCESS_CONTROL", gatewayExistingFile, false, false},
		{"empty-dacl", "D:P", gatewayExistingFile, false, true},
		{"inherited-private", "D:P(A;OICI;FA;;;" + self + ")(A;OICI;FA;;;SY)", gatewayInheritedFile, false, true},
		{"creator-owner", "D:P(A;OICI;FA;;;CO)(A;OICI;FA;;;SY)", gatewayInheritedFile, true, true},
		{"inherited-public", "D:P(A;OICI;FA;;;" + self + ")(A;OIIO;GR;;;WD)", gatewayInheritedFile, false, false},
		{"inherit-only-is-not-effective", "D:P(A;;FA;;;" + self + ")(A;OIIO;GR;;;WD)", gatewayExistingFile, false, true},
		{"no-file-inheritance", "D:P(A;;FA;;;" + self + ")", gatewayInheritedFile, false, false},
		{"nonpropagating-descendant", "D:P(A;OICINP;FA;;;" + self + ")", gatewayInheritedFile, true, false},
		{"public-parent-listing", "D:P(A;OICI;FA;;;" + self + ")(A;;GR;;;WD)", gatewayExistingDirectory, false, true},
		{"public-parent-write", "D:P(A;OICI;FA;;;" + self + ")(A;;GW;;;WD)", gatewayExistingDirectory, false, false},
		{"public-new-directory-control", "D:P(A;OICI;FA;;;" + self + ")(A;CIIO;WD;;;WD)", gatewayInheritedDirectory, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			descriptor := gatewayTestDescriptor(t, current, tc.acl)
			err := checkGatewayWindowsACL(descriptor, current, tc.use, tc.descendant)
			runtime.KeepAlive(descriptor)
			if (err == nil) != tc.allowed {
				t.Fatalf("privacy result: allowed=%v error=%v", tc.allowed, err)
			}
		})
	}
}

func setGatewayTestDACL(t *testing.T, path, entries string) {
	t.Helper()
	descriptor := gatewayTestDescriptor(t, gatewayTestWindowsSID(t), "D:P"+entries)
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(descriptor)
}

func TestGatewayConfigPrivacyWindowsChecksFileAndAtomicInheritance(t *testing.T) {
	self := gatewayTestWindowsSID(t).String()
	private := "(A;OICI;FA;;;" + self + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	parent := t.TempDir()
	setGatewayTestDACL(t, parent, private)
	path := filepath.Join(parent, "mcp.json")
	data := []byte("temporary configuration fixture")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckGatewayConfigPrivacy(path); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckGatewayConfigPrivacy(path); err != nil {
		t.Fatal(err)
	}
	after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || before.String() != after.String() {
		t.Fatalf("privacy check changed ACL: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(data) {
		t.Fatalf("privacy check changed content: %v", err)
	}
	future := filepath.Join(parent, "missing", "mcp.json")
	if err := CheckGatewayConfigPrivacy(future); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0600, 0644} {
		if err := CheckGatewayConfigCopyPrivacy(future, mode); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(filepath.Dir(future)); !os.IsNotExist(err) {
		t.Fatalf("privacy check created a directory: %v", err)
	}
	setGatewayTestDACL(t, path, "(A;;FA;;;"+self+")(A;;GR;;;WD)")
	if err := CheckGatewayConfigPrivacy(path); err == nil {
		t.Fatal("public file accepted because its parent was private")
	}
	setGatewayTestDACL(t, path, "(A;;FA;;;"+self+")(A;;FA;;;SY)")
	setGatewayTestDACL(t, parent, private+"(A;OIIO;GR;;;WD)")
	if err := CheckGatewayConfigPrivacy(path); err == nil {
		t.Fatal("public atomic-write inheritance accepted")
	}
	if err := CheckGatewayConfigPrivacy(future); err == nil {
		t.Fatal("public future-file inheritance accepted")
	}
	for _, mode := range []os.FileMode{0600, 0644} {
		if err := CheckGatewayConfigCopyPrivacy(future, mode); err == nil {
			t.Fatal("copy mode bypassed public future-file inheritance")
		}
	}
}

func TestGatewayMovePrivacyWindowsChecksFutureParent(t *testing.T) {
	self := gatewayTestWindowsSID(t).String()
	private := "(A;OICI;FA;;;" + self + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	root := t.TempDir()
	setGatewayTestDACL(t, root, private)
	source := filepath.Join(root, "staged")
	destinationParent := filepath.Join(root, "destination")
	for _, path := range []string{source, destinationParent} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("temporary capability fixture")
	file := filepath.Join(source, "mcp.json")
	if err := os.WriteFile(file, data, 0644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(destinationParent, "native")
	if err := CheckGatewayConfigMovePrivacy(source, destination, "mcp.json"); err != nil {
		t.Fatal(err)
	}
	setGatewayTestDACL(t, destinationParent, private+"(A;;GW;;;WD)")
	if err := CheckGatewayConfigMovePrivacy(source, destination, "mcp.json"); err == nil {
		t.Fatal("native relocation ignored a publicly writable future parent")
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != string(data) {
		t.Fatal("relocation permission check changed the source")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("relocation permission check created its destination")
	}
}
