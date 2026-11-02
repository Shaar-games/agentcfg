//go:build !windows

package upstream

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

type unixProcess struct {
	cmd  *exec.Cmd
	once sync.Once
	err  error
}

func startProcess(command string, args, env []string, cwd string, stdin, stdout, stderr *os.File) (childProcess, error) {
	cmd := exec.Command(command, args...)
	cmd.Env, cmd.Dir = env, cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &unixProcess{cmd: cmd}, nil
}

func (p *unixProcess) pid() int    { return p.cmd.Process.Pid }
func (p *unixProcess) wait() error { return p.cmd.Wait() }
func (p *unixProcess) stop() error {
	p.once.Do(func() {
		p.err = syscall.Kill(-p.pid(), syscall.SIGKILL)
		if errors.Is(p.err, syscall.ESRCH) {
			p.err = nil
		}
	})
	return p.err
}
