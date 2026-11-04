//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestInitMigratesAndRestoresSkillJunctions(t *testing.T) {
	testInitSkillLinks(t, nativeLayoutJunction)
}

func nativeLayoutJunction(t *testing.T, target, path string) {
	command := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", path, target)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("temporary skill junction: %v: %s", err, output)
	}
}

func TestInitSkillDirectoryJunctionMatrix(t *testing.T) {
	testSkillDirectoryMatrix(t, nativeLayoutJunction)
}

func TestInitMixedNativeJunctionLayout(t *testing.T) {
	testInitMixedNativeLayout(t, func(t *testing.T, target, path string) {
		if info, err := os.Stat(target); err == nil && info.IsDir() {
			nativeLayoutJunction(t, target, path)
		} else if err := os.Symlink(target, path); err != nil {
			t.Skipf("file symlink unavailable: %v", err)
		}
	})
}
