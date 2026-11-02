//go:build windows

package ipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"os/user"
	"syscall"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func userSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	current, err := user.Current()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + current.Uid + ")")
}

func secureDirectory(path string) error {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(path16)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("runtime directory must not be a junction or reparse point: %s", path)
	}
	sd, err := userSecurityDescriptor()
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

func Listen(endpoint Endpoint) (net.Listener, error) {
	current, err := user.Current()
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(endpoint.Address, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + current.Uid + ")",
		InputBufferSize:    64 << 10, OutputBufferSize: 64 << 10,
	})
}

func Dial(ctx context.Context, endpoint Endpoint) (net.Conn, error) {
	return winio.DialPipeContext(ctx, endpoint.Address)
}

func Detach(cmd *exec.Cmd) {
	// Detach from the stdio proxy's console. No window or TCP port is created.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200 | 0x01000000, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP | CREATE_BREAKAWAY_FROM_JOB
	}
}

// RetryInInheritedJob is a constrained fallback for Windows hosts (notably
// CI runners) whose existing Job Object forbids CREATE_BREAKAWAY_FROM_JOB.
// The fallback does not guarantee that the broker survives that outer job;
// callers must report that limitation rather than advertising full detachment.
func RetryInInheritedJob(cmd *exec.Cmd, cause error) bool {
	if !errors.Is(cause, windows.ERROR_ACCESS_DENIED) && !errors.Is(cause, windows.ERROR_PRIVILEGE_NOT_HELD) {
		return false
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&0x01000000 == 0 {
		return false
	}
	cmd.SysProcAttr.CreationFlags &^= 0x01000000
	return true
}
