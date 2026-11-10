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

func TestWindowsNPXSelectsBatchCompanion(t *testing.T) {
	executable, directory := copyHelper(t)
	// npm installs the POSIX shim and the Windows batch shim side by side.
	// The extensionless POSIX script must never hide the executable PATHEXT entry.
	bare := filepath.Join(directory, "npx")
	batch := bare + ".cmd"
	if err := os.WriteFile(bare, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(batch, []byte("@echo off\r\n\""+executable+"\" %*\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"npx", bare} {
		t.Run(command, func(t *testing.T) {
			server := helperServer(t, "echo")
			server.Command = command
			server.Cwd = directory
			server.Env["PATH"] = directory
			server.Env["PATHEXT"] = ".COM;.EXE;.BAT;.CMD"
			server.Args = append(server.Args, "--yes", "@scope/package", "path with spaces")
			transport := startTestTransport(t, context.Background(), server)
			if err := transport.Write(rpcRequest("npx", "probe", nil)); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Args []string `json:"args"`
			}
			if err := json.Unmarshal(mustRead(t, transport)["result"], &result); err != nil {
				t.Fatal(err)
			}
			if want := []string{"--yes", "@scope/package", "path with spaces"}; !reflect.DeepEqual(result.Args, want) {
				t.Fatalf("batch changed npx arguments: got %#v, want %#v", result.Args, want)
			}
		})
	}
}

func TestWindowsCommandPATHEXTPriority(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"npx", "npx.exe", "npx.cmd", "tool.runner.cmd"} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		command string
		pathExt string
		want    string
	}{
		{"npx", ".CMD;.EXE", "npx.cmd"},
		{"npx", ".EXE;.CMD", "npx.exe"},
		{"npx", "", "npx.exe"},
		{"npx.exe", ".CMD;.EXE", "npx.exe"},
		{"npx.cmd", ".EXE;.CMD", "npx.cmd"},
		{"tool.runner", ".EXE;.CMD", "tool.runner.cmd"},
	} {
		t.Run(test.command+"_"+test.pathExt, func(t *testing.T) {
			env := []string{"Path=" + directory, "PATHEXT=" + test.pathExt}
			got, err := resolveCommand(test.command, directory, env)
			if err != nil {
				t.Fatal(err)
			}
			gotInfo, err := os.Stat(got)
			if err != nil {
				t.Fatal(err)
			}
			wantInfo, err := os.Stat(filepath.Join(directory, test.want))
			if err != nil || !os.SameFile(gotInfo, wantInfo) {
				t.Fatalf("command resolved to %q, want %q: %v", got, test.want, err)
			}
		})
	}
}

func TestWindowsCommandIgnoresEmptyPATHComponents(t *testing.T) {
	cwd, bin := t.TempDir(), t.TempDir()
	for _, directory := range []string{cwd, bin} {
		if err := os.WriteFile(filepath.Join(directory, "npx.exe"), nil, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{";" + bin, ";;" + bin + ";", bin + ";"} {
		got, err := resolveCommand("npx", cwd, []string{"PATH=" + path, "PATHEXT=.EXE"})
		if err != nil || got != filepath.Join(bin, "npx.EXE") {
			t.Fatalf("PATH=%q resolved to %q instead of configured bin: %v", path, got, err)
		}
	}
	for _, path := range []string{"", ";", ";;"} {
		if got, err := resolveCommand("npx", cwd, []string{"PATH=" + path}); err == nil {
			t.Fatalf("empty PATH components unexpectedly resolved project executable %q", got)
		}
	}
	got, err := resolveCommand("npx", cwd, []string{"PATH=.", "PATHEXT=.EXE"})
	if err != nil || got != filepath.Join(cwd, "npx.EXE") {
		t.Fatalf("explicit current-directory PATH stopped working: %q, %v", got, err)
	}
}
