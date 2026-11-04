//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestInitMigratesAndRestoresSkillJunctions(t *testing.T) {
	testInitSkillLinks(t, func(t *testing.T, target, path string) {
		command := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", path, target)
		command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("temporary skill junction: %v: %s", err, output)
		}
	})
}
