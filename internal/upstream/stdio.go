package upstream

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"agentcfg/internal/config"
	"agentcfg/internal/wire"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type childProcess interface {
	pid() int
	wait() error
	stop() error
}

type stdioTransport struct {
	process    childProcess
	input      *os.File
	output     *os.File
	errors     *os.File
	ctx        context.Context
	connection mcp.Connection
	readErr    error // Read is single-consumer; SDK's EOF/error channel is not replayed.
	cancel     context.CancelFunc
	done       chan struct{}
	logsDone   chan struct{}
	closeOnce  sync.Once
	closeErr   error
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
	if _, overridden := s.Env["PWD"]; runtime.GOOS != "windows" && !overridden {
		// An explicit Cmd.Env prevents os/exec from updating PWD for Cmd.Dir.
		// Keep the inherited shell directory consistent with this client's cwd,
		// while preserving a deliberate per-server PWD override.
		env, err = mergeEnvironment(env, map[string]string{"PWD": cwd})
		if err != nil {
			return nil, err
		}
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
	connection, err := (&mcp.IOTransport{
		Reader: &singleMessageReader{ReadCloser: output},
		// The reader enforces the payload limit; allow delimiter overhead
		// in the SDK's byte budget between consecutive maximum-size objects.
		Writer: input, MaxLineLength: wire.MaxFrameSize + 2,
	}).Connect(ctx)
	if err != nil {
		cancel()
		_ = process.stop()
		_ = input.Close()
		_ = output.Close()
		_ = logs.Close()
		_ = process.wait()
		return nil, fmt.Errorf("connect SDK stdio transport: %w", err)
	}
	t := &stdioTransport{process: process, input: input, output: output, errors: logs,
		ctx: ctx, connection: connection, cancel: cancel,
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

func (t *stdioTransport) PID() int { return t.process.pid() }

func (t *stdioTransport) Read() (wire.Message, error) {
	if t.readErr != nil {
		return nil, t.readErr
	}
	message, err := t.connection.Read(t.ctx)
	if err != nil {
		if t.ctx.Err() != nil || err == io.EOF {
			t.readErr = io.EOF
		} else {
			t.readErr = fmt.Errorf("read MCP stdio through SDK (maximum 32 MiB): %w", err)
		}
		return nil, t.readErr
	}
	return encodeSDKMessage(message)
}

func (t *stdioTransport) Write(message wire.Message) error {
	if t.ctx.Err() != nil {
		return ErrClosed
	}
	decoded, err := decodeSDKMessage(message)
	if err != nil {
		return err
	}
	return t.connection.Write(t.ctx, decoded)
}

func (t *stdioTransport) Close() error {
	t.closeOnce.Do(func() {
		t.cancel()
		t.closeErr = t.process.stop()
		_ = t.connection.Close()
		_ = t.errors.Close()
		<-t.done
		<-t.logsDone
	})
	return t.closeErr
}
