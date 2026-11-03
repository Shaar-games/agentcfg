package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"agentcfg/internal/config"
	"agentcfg/internal/wire"
)

// The actual Go test executable is the fixture server; no installed MCP or
// external service is started. Exit avoids testing's PASS output on MCP stdout.
func TestUpstreamHelper(t *testing.T) {
	if os.Getenv("AGENTCFG_UPSTREAM_HELPER") != "1" {
		return
	}
	separator := 0
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == 0 || separator+1 >= len(os.Args) {
		os.Exit(2)
	}
	mode := os.Args[separator+1]
	writer := wire.NewWriter(os.Stdout)
	switch mode {
	case "malformed":
		fmt.Fprintln(os.Stdout, "this is not JSON")
		os.Exit(0)
	case "oversized":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", wire.MaxFrameSize+2))
		os.Exit(0)
	case "exit":
		_ = writer.Write(wire.Result(wire.Raw(1), "last message"))
		os.Exit(0)
	case "tree":
		executable, _ := os.Executable()
		child := exec.Command(executable, "-test.run=^TestUpstreamHelper$", "--", "sleep")
		child.Env = os.Environ()
		child.Stdout, child.Stderr = io.Discard, io.Discard
		hideHelperWindow(child)
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		_ = writer.Write(wire.Result(wire.Raw(1), map[string]int{"child": child.Process.Pid}))
		time.Sleep(time.Hour)
		os.Exit(0)
	case "blocked":
		_ = writer.Write(wire.Result(wire.Raw(1), "ready"))
		time.Sleep(time.Hour)
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	reader := wire.NewReader(os.Stdin)
	for {
		message, err := reader.Read()
		if err != nil {
			os.Exit(0)
		}
		if message.Method() == "probe" {
			cwd, _ := os.Getwd()
			_ = writer.Write(wire.Result(message.ID(), map[string]any{
				"pid": os.Getpid(), "cwd": cwd, "env": os.Getenv("AGENTCFG_TEST_VALUE"),
				"args": os.Args[separator+2:], "pluginData": os.Getenv("PLUGIN_DATA"),
			}))
		} else {
			_ = writer.Write(wire.Result(message.ID(), message["params"]))
		}
	}
}

func helperServer(t *testing.T, mode string) config.Server {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return config.Server{Type: "stdio", Command: executable,
		Args: []string{"-test.run=^TestUpstreamHelper$", "--", mode},
		Env:  map[string]string{"AGENTCFG_UPSTREAM_HELPER": "1"}}
}

func startTestTransport(t *testing.T, ctx context.Context, server config.Server) Transport {
	t.Helper()
	transport, err := Start(ctx, server, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	return transport
}

func readResult(t *testing.T, transport Transport) (wire.Message, error) {
	t.Helper()
	type result struct {
		message wire.Message
		err     error
	}
	done := make(chan result, 1)
	go func() { message, err := transport.Read(); done <- result{message, err} }()
	select {
	case value := <-done:
		return value.message, value.err
	case <-time.After(5 * time.Second):
		t.Fatal("transport Read did not complete")
		return nil, io.ErrNoProgress
	}
}

func mustRead(t *testing.T, transport Transport) wire.Message {
	t.Helper()
	message, err := readResult(t, transport)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func closeTransport(t *testing.T, transport Transport) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- transport.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transport Close did not complete")
	}
}

func rpcRequest(id any, method string, params any) wire.Message {
	return wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(id), "method": wire.Raw(method), "params": wire.Raw(params)}
}

func copyHelper(t *testing.T) (string, string) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "server with spaces")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	name := "upstream helper"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	return path, directory
}

func TestStdioArgumentsEnvironmentCwdAndPID(t *testing.T) {
	path, directory := copyHelper(t)
	server := helperServer(t, "echo")
	server.Command, server.Cwd = filepath.Base(path), directory
	args := []string{"two words", "quote\"inside", "x&y|z", "100%", "bang!", "C:\\tail\\"}
	server.Args = append(server.Args, args...)
	server.Env["PATH"] = directory
	server.Env["AGENTCFG_TEST_VALUE"] = "resolved value"
	server.Env["PLUGIN_DATA"] = filepath.Join(directory, "plugin data")
	transport := startTestTransport(t, context.Background(), server)
	if transport.PID() <= 0 {
		t.Fatal("missing process PID")
	}
	if err := transport.Write(rpcRequest("probe-1", "probe", nil)); err != nil {
		t.Fatal(err)
	}
	message := mustRead(t, transport)
	var result struct {
		PID  int      `json:"pid"`
		Cwd  string   `json:"cwd"`
		Env  string   `json:"env"`
		Args []string `json:"args"`
	}
	if err := json.Unmarshal(message["result"], &result); err != nil {
		t.Fatal(err)
	}
	physicalDirectory, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	probeDirectory, err := os.Stat(result.Cwd)
	if err != nil {
		t.Fatalf("probe working directory %q: %v", result.Cwd, err)
	}
	if result.PID != transport.PID() || !os.SameFile(physicalDirectory, probeDirectory) || result.Env != "resolved value" || !reflect.DeepEqual(result.Args, args) {
		t.Fatalf("unexpected probe: %+v", result)
	}
	if info, err := os.Stat(server.Env["PLUGIN_DATA"]); err != nil || !info.IsDir() {
		t.Fatal("PLUGIN_DATA not created at execution")
	}
	closeTransport(t, transport)
	select {
	case <-transport.(*stdioTransport).done:
	default:
		t.Fatal("process was not reaped")
	}
}

func TestStdioConcurrentWrites(t *testing.T) {
	transport := startTestTransport(t, context.Background(), helperServer(t, "echo"))
	const count = 40
	var writers sync.WaitGroup
	writers.Add(count)
	for i := 0; i < count; i++ {
		go func(id int) {
			defer writers.Done()
			if err := transport.Write(rpcRequest(id, "echo", map[string]int{"value": id})); err != nil {
				t.Error(err)
			}
		}(i)
	}
	seen := make(map[string]bool)
	for i := 0; i < count; i++ {
		message := mustRead(t, transport)
		seen[string(message.ID())] = true
	}
	writers.Wait()
	if len(seen) != count {
		t.Fatalf("received %d distinct frames", len(seen))
	}
}

func TestStdioMalformedAndOversizedFrames(t *testing.T) {
	for _, mode := range []string{"malformed", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			transport := startTestTransport(t, context.Background(), helperServer(t, mode))
			if _, err := readResult(t, transport); err == nil {
				t.Fatal("invalid frame was accepted")
			}
			closeTransport(t, transport)
		})
	}
}

func TestStdioPreservesOutputOnExit(t *testing.T) {
	transport := startTestTransport(t, context.Background(), helperServer(t, "exit"))
	select {
	case <-transport.(*stdioTransport).done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit")
	}
	if !bytes.Equal(mustRead(t, transport)["result"], wire.Raw("last message")) {
		t.Fatal("final output lost")
	}
	if _, err := readResult(t, transport); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestStdioCloseUnblocksReadAndWrite(t *testing.T) {
	transport := startTestTransport(t, context.Background(), helperServer(t, "blocked"))
	mustRead(t, transport)
	writeDone, readDone := make(chan error, 1), make(chan error, 1)
	go func() { writeDone <- transport.Write(rpcRequest(2, "echo", strings.Repeat("a", 2<<20))) }()
	go func() { _, err := transport.Read(); readDone <- err }()
	time.Sleep(50 * time.Millisecond)
	var closers sync.WaitGroup
	for i := 0; i < 4; i++ {
		closers.Add(1)
		go func() { defer closers.Done(); _ = transport.Close() }()
	}
	closeTransport(t, transport)
	closers.Wait()
	for _, done := range []chan error{writeDone, readDone} {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("blocked I/O succeeded after close")
			}
		case <-time.After(time.Second):
			t.Fatal("I/O stayed blocked")
		}
	}
}

func TestStdioContextAndProcessTree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := startTestTransport(t, ctx, helperServer(t, "tree"))
	var result struct {
		Child int `json:"child"`
	}
	if err := json.Unmarshal(mustRead(t, transport)["result"], &result); err != nil {
		t.Fatal(err)
	}
	verifyChildGone := watchHelperProcess(t, result.Child)
	cancel()
	closeTransport(t, transport)
	verifyChildGone()
}

func TestEnvironmentDeterministicAndValidation(t *testing.T) {
	base := []string{"A=old", "B=keep"}
	if runtime.GOOS == "windows" {
		base = append(base, "=C:=C:\\somewhere")
	}
	want, err := mergeEnvironment(base, map[string]string{"A": "new", "Z": "last"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := mergeEnvironment(base, map[string]string{"Z": "last", "A": "new"})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("environment ordering changed")
		}
	}
	if environmentValue(want, "A") != "new" || environmentValue(want, "B") != "keep" {
		t.Fatal("environment override failed")
	}
	if _, err := mergeEnvironment(nil, map[string]string{"BAD=NAME": "value"}); err == nil {
		t.Fatal("invalid environment accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Start(ctx, helperServer(t, "echo"), nil); err != context.Canceled {
		t.Fatalf("cancelled Start: %v", err)
	}
	if _, err := Start(context.Background(), config.Server{Type: "sse"}, nil); err == nil {
		t.Fatal("legacy SSE accepted")
	}
}

func TestPluginDataAndContainedCwdCreatedOnlyOnExecution(t *testing.T) {
	directory := t.TempDir()
	server := helperServer(t, "echo")
	server.Env["PLUGIN_DATA"] = filepath.Join(directory, "plugin data")
	server.Cwd = filepath.Join(server.Env["PLUGIN_DATA"], "nested cwd")
	if _, err := os.Stat(server.Cwd); !os.IsNotExist(err) {
		t.Fatal("fixture cwd already exists")
	}
	transport := startTestTransport(t, context.Background(), server)
	if info, err := os.Stat(server.Cwd); err != nil || !info.IsDir() {
		t.Fatal("contained cwd was not created")
	}
	closeTransport(t, transport)
	outside := helperServer(t, "echo")
	outside.Env["PLUGIN_DATA"] = server.Env["PLUGIN_DATA"]
	outside.Cwd = filepath.Join(directory, "outside data")
	if transport, err := Start(context.Background(), outside, nil); err == nil {
		transport.Close()
		t.Fatal("unrelated missing cwd was created")
	}
	if _, err := os.Stat(outside.Cwd); !os.IsNotExist(err) {
		t.Fatal("unrelated directory changed")
	}
	inherited := filepath.Join(directory, "inherited data")
	t.Setenv("PLUGIN_DATA", inherited)
	transport = startTestTransport(t, context.Background(), helperServer(t, "echo"))
	closeTransport(t, transport)
	if _, err := os.Stat(inherited); !os.IsNotExist(err) {
		t.Fatal("inherited PLUGIN_DATA should not create directories")
	}
}
