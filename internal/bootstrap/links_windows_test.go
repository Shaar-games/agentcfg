//go:build windows

package bootstrap

import (
	"os/exec"
	"syscall"
	"testing"
)

func dirLink(t *testing.T, link, target string) {
	t.Helper()
	command := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("temporary junction: %v: %s", err, output)
	}
}
