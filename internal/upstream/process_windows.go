//go:build windows

package upstream

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcess struct {
	process   windows.Handle
	job       windows.Handle
	processID int
	once      sync.Once
	err       error
}

func startProcess(command string, args, env []string, cwd string, stdin, stdout, stderr *os.File) (childProcess, error) {
	application, line, err := windowsCommand(command, args)
	if err != nil {
		return nil, err
	}
	application16, err := windows.UTF16PtrFromString(application)
	if err != nil {
		return nil, errors.New("invalid MCP executable path")
	}
	line16, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return nil, errors.New("invalid MCP process argument")
	}
	cwd16, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		return nil, errors.New("invalid MCP working directory")
	}
	environment := utf16.Encode([]rune(strings.Join(env, "\x00") + "\x00\x00"))
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create process job: %w", err)
	}
	keepJob := false
	defer func() {
		if !keepJob {
			_ = windows.CloseHandle(job)
		}
	}()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	if err != nil {
		return nil, fmt.Errorf("configure process job: %w", err)
	}

	// Inherit only private duplicate handles, never arbitrary broker handles.
	handles := make([]windows.Handle, 0, 3)
	defer func() {
		for _, h := range handles {
			_ = windows.CloseHandle(h)
		}
	}()
	for _, file := range []*os.File{stdin, stdout, stderr} {
		var duplicate windows.Handle
		err = windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(file.Fd()),
			windows.CurrentProcess(), &duplicate, 0, true, windows.DUPLICATE_SAME_ACCESS)
		if err != nil {
			return nil, fmt.Errorf("duplicate process pipe: %w", err)
		}
		handles = append(handles, duplicate)
	}
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, fmt.Errorf("create process attributes: %w", err)
	}
	defer attributes.Delete()
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return nil, fmt.Errorf("restrict inherited process handles: %w", err)
	}
	startup := windows.StartupInfoEx{ProcThreadAttributeList: attributes.List()}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Flags = windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW
	startup.ShowWindow = windows.SW_HIDE
	startup.StdInput, startup.StdOutput, startup.StdErr = handles[0], handles[1], handles[2]
	var info windows.ProcessInformation
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW |
		windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err := windows.CreateProcess(application16, line16, nil, nil, true, flags,
		&environment[0], cwd16, &startup.StartupInfo, &info); err != nil {
		return nil, fmt.Errorf("create suspended MCP process: %w", err)
	}
	runtime.KeepAlive(handles)
	runtime.KeepAlive(environment)
	defer windows.CloseHandle(info.Thread)
	// No server instruction can execute before job assignment succeeds. If the
	// host job forbids nesting, fail closed and reap this still-suspended process.
	failed := func(cause error) (childProcess, error) {
		_ = windows.TerminateProcess(info.Process, 1)
		_, _ = windows.WaitForSingleObject(info.Process, windows.INFINITE)
		_ = windows.CloseHandle(info.Process)
		return nil, cause
	}
	if err := windows.AssignProcessToJobObject(job, info.Process); err != nil {
		return failed(fmt.Errorf("attach MCP process to kill-on-close job: %w", err))
	}
	if _, err := windows.ResumeThread(info.Thread); err != nil {
		return failed(fmt.Errorf("resume MCP process: %w", err))
	}
	keepJob = true
	return &windowsProcess{process: info.Process, job: job, processID: int(info.ProcessId)}, nil
}

func (p *windowsProcess) pid() int { return p.processID }

func (p *windowsProcess) wait() error {
	defer windows.CloseHandle(p.process)
	_, err := windows.WaitForSingleObject(p.process, windows.INFINITE)
	return err
}

func (p *windowsProcess) stop() error {
	p.once.Do(func() { p.err = windows.CloseHandle(p.job) })
	return p.err
}

func windowsCommand(command string, args []string) (string, string, error) {
	ext := strings.ToLower(filepath.Ext(command))
	if ext != ".cmd" && ext != ".bat" {
		return command, windows.ComposeCommandLine(append([]string{command}, args...)), nil
	}
	// cmd.exe has an incompatible parser. A conservative quoted subset keeps
	// % expansion, delayed expansion and quote breaking out of the command line.
	// Executable servers retain the full native argument space.
	quote := func(value string) (string, error) {
		if strings.ContainsAny(value, "\"%!\r\n\x00") {
			return "", errors.New("batch MCP arguments cannot contain quotes, percent signs, exclamation marks or newlines; invoke the underlying executable for these arguments")
		}
		return "\"" + value + "\"", nil
	}
	quoted, err := quote(command)
	if err != nil {
		return "", "", err
	}
	parts := []string{quoted}
	for _, arg := range args {
		// Trailing backslashes have incompatible cmd/CRT quoting rules; reject
		// instead of silently changing a value forwarded by a .cmd wrapper.
		if strings.HasSuffix(arg, "\\") {
			return "", "", errors.New("batch MCP arguments cannot end with a backslash; invoke the underlying executable or remove the trailing separator")
		}
		quoted, err = quote(arg)
		if err != nil {
			return "", "", err
		}
		parts = append(parts, quoted)
	}
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return "", "", fmt.Errorf("find system command interpreter: %w", err)
	}
	interpreter := filepath.Join(systemDirectory, "cmd.exe")
	line := windows.ComposeCommandLine([]string{interpreter}) + " /d /q /v:off /s /c \"" + strings.Join(parts, " ") + "\""
	return interpreter, line, nil
}
