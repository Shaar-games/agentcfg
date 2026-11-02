//go:build !windows

package upstream

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func hideHelperWindow(cmd *exec.Cmd) {}

func watchHelperProcess(t *testing.T, pid int) func() {
	t.Helper()
	if pid <= 0 {
		t.Fatal("missing child PID")
	}
	return func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return
			}
			// A terminated grandchild can briefly await the host init's reaper.
			if state, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
				if end := strings.LastIndexByte(string(state), ')'); end >= 0 && strings.HasPrefix(string(state[end+1:]), " Z ") {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("descendant %d survived Close", pid)
	}
}
