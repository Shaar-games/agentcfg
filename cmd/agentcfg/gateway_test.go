package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentcfg/internal/auth"
	"agentcfg/internal/broker"
	"agentcfg/internal/config"
	"agentcfg/internal/gateway"
	"agentcfg/internal/ipc"
)

func nativeGatewayCLIRoot(t *testing.T) (string, string) {
	t.Helper()
	root := integrationRoot(t, "shared", "5s")
	t.Setenv("AGENTCFG_OAUTH_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{73}, 32)))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	settings := config.Settings{Harnesses: []string{"cursor"}, Gateway: config.GatewaySettings{Enabled: true, Listen: address}}
	settings.Gateway.RouteKey, err = auth.GatewayKey(t.Context(), root, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(root, ".agentcfg", "agentcfg.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(root, ".agentcfg", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["mcpServers"]["remote"] = map[string]any{"type": "streamable-http", "url": "https://fixture.invalid/mcp"}
	data, _ = json.Marshal(document)
	if err := os.WriteFile(filepath.Join(root, ".agentcfg", "mcp.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint, err := ipc.ForService(root, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(endpoint.Directory); !os.IsNotExist(err) {
		t.Fatal("gateway fixture runtime already exists")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if status, err := gateway.GetStatus(ctx, root); err == nil && status.Running {
			if err := gateway.Stop(ctx, root); err != nil {
				t.Error(err)
				return
			}
		}
		_ = os.RemoveAll(endpoint.Directory)
	})
	return root, config.GatewayResourceURL(settings.Gateway, "remote", "cursor")
}

func TestCLIGatewayBackgroundLifecycle(t *testing.T) {
	binary := cliBinary(t)
	root, resource := nativeGatewayCLIRoot(t)
	original := map[string][]byte{}
	for _, name := range []string{"mcp.json", "agentcfg.json"} {
		data, err := os.ReadFile(filepath.Join(root, ".agentcfg", name))
		if err != nil {
			t.Fatal(err)
		}
		original[name] = data
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	runCLI := func(args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, binary, args...)
		command.Env = os.Environ()
		command.Dir = root
		return command.CombinedOutput()
	}
	// Concurrent launches must converge on the same private service and port.
	type launchResult struct {
		output []byte
		err    error
	}
	launched := make(chan launchResult, 2)
	for range 2 {
		go func() { output, err := runCLI("gateway", "start"); launched <- launchResult{output, err} }()
	}
	for range 2 {
		result := <-launched
		if result.err != nil {
			t.Fatalf("background start: %v\n%s", result.err, result.output)
		}
	}
	status, err := gateway.GetStatus(ctx, root)
	if err != nil || !status.Running || status.PID == os.Getpid() {
		t.Fatalf("background status=%+v (%v)", status, err)
	}
	pid := status.PID
	if output, err := runCLI("gateway", "start"); err != nil {
		t.Fatalf("idempotent start: %v\n%s", err, output)
	}
	if status, err := gateway.GetStatus(ctx, root); err != nil || status.PID != pid {
		t.Fatalf("second start replaced process: %+v %v", status, err)
	}
	if output, err := runCLI("gateway", "url", "--server", "remote", "--harness", "cursor"); err != nil || strings.TrimSpace(string(output)) != resource {
		t.Fatalf("native URL: %q %v", output, err)
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get(resource)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 401 || !strings.Contains(response.Header.Get("WWW-Authenticate"), status.URL+"/.well-known/oauth-protected-resource/") {
		t.Fatalf("native OAuth challenge: status=%d headers=%v", response.StatusCode, response.Header)
	}
	output, err := runCLI("status", "--json")
	var overall struct {
		Gateway *gateway.Status `json:"gateway"`
	}
	if err != nil || json.Unmarshal(output, &overall) != nil || overall.Gateway == nil || overall.Gateway.PID != pid {
		t.Fatalf("CLI status missed gateway: %q %v", output, err)
	}
	// A live stdio client remains usable after stopping only the HTTP gateway.
	peer := startCLIPeer(t, binary, root, "cursor")
	peer.initialize(t)
	_ = peer.read(t)
	if output, err := runCLI("gateway", "stop"); err != nil {
		t.Fatalf("gateway stop: %v\n%s", err, output)
	}
	if state, err := broker.GetStatus(ctx, root); err != nil || !state.Running {
		t.Fatalf("gateway stop affected stdio broker: %+v %v", state, err)
	}
	peer.call(t, 2, "still-alive")
	_ = resultPID(t, peer.read(t), "still-alive")
	if output, err := runCLI("gateway", "start"); err != nil {
		t.Fatalf("restart after port release: %v\n%s", err, output)
	}
	if output, err := runCLI("stop", "--yes"); err != nil {
		t.Fatalf("global stop: %v\n%s", err, output)
	}
	if state, err := gateway.GetStatus(ctx, root); err != nil || state.Running {
		t.Fatalf("global stop left gateway active: %+v %v", state, err)
	}
	for name, before := range original {
		after, err := os.ReadFile(filepath.Join(root, ".agentcfg", name))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("gateway lifecycle changed canonical %s: %v", name, err)
		}
	}
}

func TestGatewayCLIReadOnlyAndErrors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "cache"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	for _, args := range [][]string{{"help"}, {"status"}} {
		var output bytes.Buffer
		if err := runGateway(t.Context(), root, args, &output, io.Discard); err != nil {
			t.Fatal(err)
		}
		if output.Len() == 0 {
			t.Fatal("missing gateway help/status")
		}
	}
	for _, args := range [][]string{{"unknown"}, {"url"}, {"url", "--server", "missing"}, {"start"}, {"run"}, {"status", "extra"}} {
		if err := runGateway(t.Context(), root, args, io.Discard, io.Discard); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("read-only/disabled gateway commands wrote to fresh root: %v %v", entries, err)
	}
}
