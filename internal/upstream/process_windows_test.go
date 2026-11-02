//go:build windows

package upstream

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func hideHelperWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

func watchHelperProcess(t *testing.T, pid int) func() {
	t.Helper()
	if pid <= 0 {
		t.Fatal("missing child PID")
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	return func() {
		status, err := windows.WaitForSingleObject(handle, 2000)
		if err != nil || status != windows.WAIT_OBJECT_0 {
			t.Fatalf("descendant %d survived Close: status=%d err=%v", pid, status, err)
		}
	}
}

func TestWindowsBatchWrapperAndInjectionBoundary(t *testing.T) {
	executable, directory := copyHelper(t)
	for _, extension := range []string{".cmd", ".bat"} {
		t.Run(extension, func(t *testing.T) {
			path := filepath.Join(directory, "wrapper with spaces"+extension)
			if err := os.WriteFile(path, []byte("@echo off\r\n\""+executable+"\" %*\r\n"), 0600); err != nil {
				t.Fatal(err)
			}
			server := helperServer(t, "echo")
			server.Command = path
			args := []string{"two words", "x&y|z", "(paren)^caret", "", "key=value"}
			server.Args = append(server.Args, args...)
			transport := startTestTransport(t, context.Background(), server)
			if err := transport.Write(rpcRequest("probe", "probe", nil)); err != nil {
				t.Fatal(err)
			}
			var result struct {
				PID  int      `json:"pid"`
				Args []string `json:"args"`
			}
			if err := json.Unmarshal(mustRead(t, transport)["result"], &result); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result.Args, args) {
				t.Fatalf("batch changed args: %#v", result.Args)
			}
			verifyGone := watchHelperProcess(t, result.PID)
			closeTransport(t, transport)
			verifyGone()
			for _, argument := range []string{"\"&echo injected", "%PATH%", "!variable!", "line\nbreak", "trailing\\"} {
				if _, _, err := windowsCommand(path, []string{argument}); err == nil {
					t.Errorf("unsafe batch argument accepted: %q", argument)
				}
			}
		})
	}
}
