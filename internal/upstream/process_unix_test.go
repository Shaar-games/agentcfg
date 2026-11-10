//go:build !windows

package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func hideHelperWindow(cmd *exec.Cmd) {}

func TestStdioWorkingDirectoryUpdatesInheritedPWD(t *testing.T) {
	t.Setenv("PWD", t.TempDir())
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit_override_%t", override), func(t *testing.T) {
			server := helperServer(t, "echo")
			server.Cwd = t.TempDir()
			want := server.Cwd
			if override {
				want = t.TempDir()
				server.Env["PWD"] = want
			}
			transport := startTestTransport(t, context.Background(), server)
			if err := transport.Write(rpcRequest("pwd", "probe", nil)); err != nil {
				t.Fatal(err)
			}
			var result struct {
				PWD string `json:"pwd"`
			}
			if err := json.Unmarshal(mustRead(t, transport)["result"], &result); err != nil {
				t.Fatal(err)
			}
			if result.PWD != want {
				t.Fatalf("child PWD = %q, want %q", result.PWD, want)
			}
		})
	}
}

func TestStdioSkipsPATHEntryWithoutEffectiveExecutePermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may execute a file when any execute bit is set")
	}
	executable, directory := copyHelper(t)
	blockedDirectory := t.TempDir()
	blocked := filepath.Join(blockedDirectory, filepath.Base(executable))
	// The current user owns this file but has no owner execute permission.
	// A group execute bit must not make this shadow a later usable PATH entry.
	if err := os.WriteFile(blocked, []byte("#!/bin/sh\nexit 99\n"), 0010); err != nil {
		t.Fatal(err)
	}
	server := helperServer(t, "echo")
	server.Command = filepath.Base(executable)
	server.Env["PATH"] = blockedDirectory + string(os.PathListSeparator) + directory
	transport := startTestTransport(t, context.Background(), server)
	if err := transport.Write(rpcRequest("permission", "probe", nil)); err != nil {
		t.Fatal(err)
	}
	var result struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(mustRead(t, transport)["result"], &result); err != nil {
		t.Fatal(err)
	}
	if result.PID != transport.PID() {
		t.Fatal("the usable PATH executable did not start")
	}
}

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
