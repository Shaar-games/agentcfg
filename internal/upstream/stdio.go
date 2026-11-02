package upstream

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"agentmux/internal/config"
	"agentmux/internal/wire"
)

type childProcess interface {
	pid() int
	wait() error
	stop() error
}

type stdioTransport struct {
	process   childProcess
	input     *os.File
	output    *os.File
	errors    *os.File
	reader    *wire.Reader
	writer    *wire.Writer
	cancel    context.CancelFunc
	done      chan struct{}
	logsDone  chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func startStdio(parent context.Context, s config.Server, stderr io.Writer) (Transport, error) {
	env, err := mergeEnvironment(os.Environ(), s.Env)
	if err != nil {
		return nil, err
	}
	cwd := s.Cwd
	if cwd == "" {
		cwd = "."
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	if err := preparePluginDirectory(cwd, s.Env, env); err != nil {
		return nil, err
	}
	cwd, err = workingDirectory(cwd)
	if err != nil {
		return nil, err
	}
	command, err := resolveCommand(s.Command, cwd, env)
	if err != nil {
		return nil, err
	}
	var opened []*os.File
	defer func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			opened = append(opened, r, w)
		}
		return r, w, err
	}
	childIn, input, err := pipe()
	if err != nil {
		return nil, err
	}
	output, childOut, err := pipe()
	if err != nil {
		return nil, err
	}
	logs, childErr, err := pipe()
	if err != nil {
		return nil, err
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	process, err := startProcess(command, s.Args, env, cwd, childIn, childOut, childErr)
	if err != nil {
		return nil, fmt.Errorf("start MCP process: %w", err)
	}
	_ = childIn.Close()
	_ = childOut.Close()
	_ = childErr.Close()
	opened = nil
	ctx, cancel := context.WithCancel(parent)
	t := &stdioTransport{process: process, input: input, output: output, errors: logs,
		reader: wire.NewReader(output), writer: wire.NewWriter(input), cancel: cancel,
		done: make(chan struct{}), logsDone: make(chan struct{})}
	if stderr == nil {
		stderr = io.Discard
	}
	go func() {
		defer close(t.logsDone)
		_, _ = io.Copy(stderr, logs)
	}()
	go func() {
		_ = process.wait()
		// A root that exits must not leave children holding the MCP pipes open.
		_ = process.stop()
		_ = input.Close()
		close(t.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = t.Close()
		case <-t.done:
			// Keep buffered stdout available until Read reaches EOF or Close.
		}
	}()
	return t, nil
}

func preparePluginDirectory(cwd string, configured map[string]string, env []string) error {
	configuredData := false
	for key := range configured {
		if environmentKey(key) == environmentKey("PLUGIN_DATA") {
			configuredData = true
		}
	}
	data := environmentValue(env, "PLUGIN_DATA")
	if !configuredData || data == "" {
		return nil
	}
	if !filepath.IsAbs(data) {
		data = filepath.Join(cwd, data)
	}
	if err := os.MkdirAll(data, 0700); err != nil {
		return fmt.Errorf("create PLUGIN_DATA directory: %w", err)
	}
	relative, err := filepath.Rel(data, cwd)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
		if err := os.MkdirAll(cwd, 0700); err != nil {
			return fmt.Errorf("create working directory within PLUGIN_DATA: %w", err)
		}
	}
	return nil
}

func (t *stdioTransport) PID() int                    { return t.process.pid() }
func (t *stdioTransport) Read() (wire.Message, error) { return t.reader.Read() }
func (t *stdioTransport) Write(m wire.Message) error  { return t.writer.Write(m) }

func (t *stdioTransport) Close() error {
	t.closeOnce.Do(func() {
		t.cancel()
		t.closeErr = t.process.stop()
		_ = t.input.Close()
		_ = t.output.Close()
		_ = t.errors.Close()
		<-t.done
		<-t.logsDone
	})
	return t.closeErr
}
