//go:build windows

package syncer

import (
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

// Only homes created by the gateway test fixtures may be passed here.
func privateGatewayFixtureHome(t *testing.T, home string) {
	t.Helper()
	setGatewayFixtureDACL(t, home, "")
}

func setGatewayFixtureDACL(t *testing.T, path, extra string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)" + extra)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
	runtime.KeepAlive(descriptor)
	if err != nil {
		t.Fatal(err)
	}
}
