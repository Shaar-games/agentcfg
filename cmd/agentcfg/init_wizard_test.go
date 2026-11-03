package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitReadOnlyPreviewDoesNotWriteWithoutApproval(t *testing.T) {
	for _, opts := range []initOptions{{Plain: true}, {Plain: true, Yes: true}, {Apply: true}} {
		t.Run(strings.Join([]string{asBool(opts.Plain), asBool(opts.Yes), asBool(opts.Apply)}, "-"), func(t *testing.T) {
			home := t.TempDir()
			var output bytes.Buffer
			proposal := initProposal{DetectedHarnesses: []string{"codex"}, Servers: map[string]map[string]any{
				"sample": {"type": "stdio", "command": "node", "harnesses": []string{"codex"}},
			}}
			err := runInitWizard(context.Background(), home, proposal, opts, nil, &output)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
				t.Fatalf("preview created canonical files: %v", err)
			}
			if !strings.Contains(output.String(), "Preview only") {
				t.Fatalf("missing preview status: %s", output.String())
			}
		})
	}
}

func asBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestInitCLIPlainInspectNativeHarnessDoesNotCreateSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(home, ".codex", "config.toml")
	data := []byte("[mcp_servers.sample]\ncommand = 'node'\n")
	if err := os.WriteFile(native, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"init", "--plain"}, nil, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "codex") {
		t.Fatalf("did not recognize installed Codex native config: %s", output.String())
	}
	if _, err := os.Lstat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatalf("read-only preview wrote canonical sources: %v", err)
	}
	after, err := os.ReadFile(native)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("native config was changed: %q, %v", after, err)
	}
}

func TestInitCLIExplicitApprovalWritesOnlyCanonical(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.Mkdir(filepath.Join(home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(home, ".cursor", "mcp.json")
	nativeData := []byte("{\"mcpServers\":{\"keep\":{\"command\":\"node\"}}}")
	if err := os.WriteFile(native, nativeData, 0o600); err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"init", "--yes"}, nil, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Created canonical agentcfg") {
		t.Fatalf("missing initialization result: %s", output.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".agentcfg", "agentcfg.json")); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(native)
	if err != nil || !bytes.Equal(after, nativeData) {
		t.Fatalf("init without --apply edited native config: %q, %v", after, err)
	}
}

func TestInitRejectsPlainWithApplyWithoutWriting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var output, diagnostics bytes.Buffer
	err := run(context.Background(), []string{"init", "--plain", "--apply", "--yes"}, nil, &output, &diagnostics)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("--plain --apply not rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatalf("invalid argument combination wrote files: %v", err)
	}
}
