package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agentmux/internal/broker"
	"agentmux/internal/ipc"
	"agentmux/internal/wire"
)

var buildOnce sync.Once
var testBinary string
var buildError error

func TestMain(m *testing.M) {
	code := m.Run()
	if testBinary != "" {
		_ = os.RemoveAll(filepath.Dir(testBinary))
	}
	os.Exit(code)
}

func cliBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "agentmux-cli-test-")
		if err != nil {
			buildError = err
			return
		}
		testBinary = filepath.Join(dir, "agentmux")
		if runtime.GOOS == "windows" {
			testBinary += ".exe"
		}
		command := exec.Command("go", "build", "-o", testBinary, ".")
		if output, err := command.CombinedOutput(); err != nil {
			buildError = fmt.Errorf("build CLI: %w\n%s", err, output)
		}
	})
	if buildError != nil {
		t.Fatal(buildError)
	}
	return testBinary
}

// TestMCPFixture becomes a real stdio MCP subprocess only when explicitly
// selected by the integration fixture. It never runs external services.
func TestMCPFixture(t *testing.T) {
	if os.Getenv("AGENTMUX_MCP_FIXTURE") != "1" {
		return
	}
	file, err := os.OpenFile(os.Getenv("AGENTMUX_START_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(10)
	}
	_, _ = fmt.Fprintln(file, os.Getpid())
	_ = file.Close()
	reader, writer := wire.NewReader(os.Stdin), wire.NewWriter(os.Stdout)
	for {
		message, err := reader.Read()
		if err != nil {
			os.Exit(0)
		}
		switch message.Method() {
		case "initialize":
			_ = writer.Write(wire.Result(message.ID(), map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "agentmux-test-fixture", "version": "1"}}))
		case "tools/list":
			_ = writer.Write(wire.Result(message.ID(), map[string]any{"tools": []any{}}))
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(message["params"], &params)
			if params.Name == "exit-fixture" {
				os.Exit(7)
			}
			_ = writer.Write(wire.Result(message.ID(), map[string]any{"content": []any{map[string]string{"type": "text", "text": strconv.Itoa(os.Getpid())}}, "structuredContent": map[string]any{"pid": os.Getpid(), "echo": params.Name}}))
		case "ping":
			_ = writer.Write(wire.Result(message.ID(), map[string]any{}))
		}
	}
}

func integrationRoot(t *testing.T, sharing, idle string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".agentmux"), 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	document := map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"type": "stdio", "command": executable, "args": []string{"-test.run=^TestMCPFixture$"}, "env": map[string]string{"AGENTMUX_MCP_FIXTURE": "1", "AGENTMUX_START_LOG": filepath.Join(root, "starts.txt")}, "sharing": sharing}}}
	data, _ := json.Marshal(document)
	if err := os.WriteFile(filepath.Join(root, ".agentmux", "mcp.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"harnesses": []string{"cursor", "codex"}, "runtime": map[string]string{"idleTimeout": idle, "requestTimeout": "3s"}}
	data, _ = json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(root, ".agentmux", "agentmux.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = broker.Stop(ctx, root)
		for i := 0; i < 30; i++ {
			status, _ := broker.GetStatus(ctx, root)
			if !status.Running {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if endpoint, err := ipc.ForRoot(root); err == nil {
			_ = os.RemoveAll(endpoint.Directory)
		}
	})
	return root
}

type cliPeer struct {
	command   *exec.Cmd
	input     io.WriteCloser
	reader    *wire.Reader
	writer    *wire.Writer
	done      chan error
	stderr    bytes.Buffer
	closeOnce sync.Once
}

func startCLIPeer(t *testing.T, binary, root, harness string) *cliPeer {
	t.Helper()
	p := &cliPeer{done: make(chan error, 1)}
	p.command = exec.Command(binary, "connect", "--root", root, "--server", "fixture", "--harness", harness)
	input, err := p.command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := p.command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.input, p.writer, p.reader = input, wire.NewWriter(input), wire.NewReader(output)
	p.command.Stderr = &p.stderr
	if err := p.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.command.Wait() }()
	t.Cleanup(func() { p.close(t) })
	return p
}

func (p *cliPeer) close(t *testing.T) {
	t.Helper()
	p.closeOnce.Do(func() {
		_ = p.input.Close()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			_ = p.command.Process.Kill()
			<-p.done
			t.Error("proxy did not exit on stdin EOF")
		}
	})
}

func (p *cliPeer) read(t *testing.T) wire.Message {
	t.Helper()
	type outcome struct {
		message wire.Message
		err     error
	}
	result := make(chan outcome, 1)
	go func() { m, err := p.reader.Read(); result <- outcome{m, err} }()
	select {
	case value := <-result:
		if value.err != nil {
			t.Fatalf("proxy read: %v", value.err)
		}
		if value.message["error"] != nil {
			t.Fatalf("MCP error: %s", value.message["error"])
		}
		return value.message
	case <-time.After(20 * time.Second):
		t.Fatal("proxy response timed out")
		return nil
	}
}

func (p *cliPeer) initialize(t *testing.T) {
	t.Helper()
	request := wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(1), "method": wire.Raw("initialize"), "params": wire.Raw(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test-harness", "version": "1"}})}
	if err := p.writer.Write(request); err != nil {
		t.Fatal(err)
	}
}

func (p *cliPeer) call(t *testing.T, id int, name string) {
	t.Helper()
	if err := p.writer.Write(wire.Message{"jsonrpc": wire.Raw("2.0"), "id": wire.Raw(id), "method": wire.Raw("tools/call"), "params": wire.Raw(map[string]string{"name": name})}); err != nil {
		t.Fatal(err)
	}
}

func resultPID(t *testing.T, message wire.Message, wantName string) int {
	t.Helper()
	var result struct {
		Structured struct {
			PID  int    `json:"pid"`
			Echo string `json:"echo"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(message["result"], &result); err != nil {
		t.Fatal(err)
	}
	if result.Structured.Echo != wantName || result.Structured.PID <= 0 {
		t.Fatalf("misrouted response: %s", message["result"])
	}
	return result.Structured.PID
}

func TestCLISharedProcessAcrossTwoHarnesses(t *testing.T) {
	binary := cliBinary(t)
	root := integrationRoot(t, "shared", "5s")
	a := startCLIPeer(t, binary, root, "cursor")
	b := startCLIPeer(t, binary, root, "codex")
	a.initialize(t)
	b.initialize(t)
	_ = a.read(t)
	_ = b.read(t)
	a.call(t, 2, "cursor-call")
	b.call(t, 2, "codex-call")
	pidA := resultPID(t, a.read(t), "cursor-call")
	pidB := resultPID(t, b.read(t), "codex-call")
	if pidA != pidB {
		t.Fatalf("two upstream processes: %d and %d", pidA, pidB)
	}
	status, err := broker.GetStatus(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running || len(status.Backends) != 1 || status.Backends[0].Clients != 2 || status.Backends[0].PID != pidA {
		t.Fatalf("unexpected status: %+v", status)
	}
	starts, err := os.ReadFile(filepath.Join(root, "starts.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(string(starts))) != 1 {
		t.Fatalf("startup race spawned extra backends: %s", starts)
	}
	a.close(t)
	b.call(t, 3, "surviving-harness")
	if resultPID(t, b.read(t), "surviving-harness") != pidA {
		t.Fatal("backend restarted after first harness disconnected")
	}
	t.Logf("Verified broker PID %d: 2 harness proxies -> 1 MCP PID %d; same request IDs routed correctly; second harness survives first exit", status.PID, pidA)
}

func TestCLIIsolatedModeUsesDistinctProcesses(t *testing.T) {
	binary := cliBinary(t)
	root := integrationRoot(t, "isolated", "5s")
	a, b := startCLIPeer(t, binary, root, "cursor"), startCLIPeer(t, binary, root, "codex")
	a.initialize(t)
	b.initialize(t)
	_ = a.read(t)
	_ = b.read(t)
	a.call(t, 2, "first")
	b.call(t, 2, "second")
	first, second := resultPID(t, a.read(t), "first"), resultPID(t, b.read(t), "second")
	if first == second {
		t.Fatal("isolated servers share a process")
	}
	t.Logf("Isolated MCP process IDs: %d and %d", first, second)
}

func TestCLIIdleBrokerStopsAutomatically(t *testing.T) {
	binary := cliBinary(t)
	root := integrationRoot(t, "shared", "500ms")
	p := startCLIPeer(t, binary, root, "cursor")
	p.initialize(t)
	_ = p.read(t)
	p.close(t)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := broker.GetStatus(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Running {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("broker remained alive after idle timeout")
}

func TestInitDoesNotOverwriteCanonicalFiles(t *testing.T) {
	root := t.TempDir()
	var output, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"init", "--root", root}, nil, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".agentmux", "mcp.json")
	original := []byte(`{"mcpServers":{},"my-manual-content":true}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"init", "--root", root}, nil, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(path)
	if !bytes.Equal(actual, original) {
		t.Fatal("init replaced user's content")
	}
}
