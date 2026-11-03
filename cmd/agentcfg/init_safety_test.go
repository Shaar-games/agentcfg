package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"agentcfg/internal/config"
)

func TestInitRejectsInvalidHarnessesBeforeCreatingSources(t *testing.T) {
	for _, input := range []string{"codex,unknown", "vscode", "codex,,cursor"} {
		t.Run(input, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			var output, diagnostics bytes.Buffer
			err := run(context.Background(), []string{"init", "--harnesses", input}, nil, &output, &diagnostics)
			if err == nil {
				t.Fatalf("invalid user-level harness selection %q was accepted", input)
			}
			if _, statErr := os.Lstat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(statErr) {
				t.Fatalf("init created sources after invalid selection: %v", statErr)
			}
		})
	}
}

func TestInitInvalidHarnessesPreserveExistingSources(t *testing.T) {
	home := t.TempDir()
	canonical := filepath.Join(home, ".agentcfg")
	if err := os.Mkdir(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(canonical, "agentcfg.json")
	original := []byte(`{"harnesses":["codex"],"runtime":{"idleTimeout":"3m"}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var output, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"init", "--harnesses", "codex,unknown"}, nil, &output, &diagnostics); err == nil {
		t.Fatal("invalid harness selection accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("invalid selection modified existing settings: %q (%v)", got, err)
	}
	if _, err := os.Lstat(filepath.Join(canonical, "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("invalid selection created another source: %v", err)
	}
}

func TestInitSupportsExplicitlyDisabledHarnesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var output, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"init", "--harnesses", "", "--yes"}, nil, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home)
	if err != nil || cfg.Settings.Harnesses == nil || len(cfg.Settings.Harnesses) != 0 {
		t.Fatalf("explicit empty harness selection not persisted: %+v (%v)", cfg, err)
	}
}

func TestInitRefusesLinkedCanonicalDirectory(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	location := filepath.Join(home, ".agentcfg")
	linkInitDirectory(t, location, outside)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	var output, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"init", "--yes"}, nil, &output, &diagnostics); err == nil {
		t.Fatal("init followed a linked canonical source outside the user home")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("linked directory outside home was written: %v (%v)", entries, err)
	}
}

func TestInitRefusesLinkedSkillsOrPluginsBeforeCreatingSources(t *testing.T) {
	for _, name := range []string{"skills", "plugins"} {
		t.Run(name, func(t *testing.T) {
			home, outside := t.TempDir(), t.TempDir()
			canonical := filepath.Join(home, ".agentcfg")
			if err := os.Mkdir(canonical, 0o700); err != nil {
				t.Fatal(err)
			}
			linkInitDirectory(t, filepath.Join(canonical, name), outside)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			var output, diagnostics bytes.Buffer
			if err := run(context.Background(), []string{"init", "--yes"}, nil, &output, &diagnostics); err == nil {
				t.Fatalf("init accepted linked %s directory", name)
			}
			for _, source := range []string{"mcp.json", "agentcfg.json"} {
				if _, err := os.Lstat(filepath.Join(canonical, source)); !os.IsNotExist(err) {
					t.Fatalf("init created %s before rejecting linked directory: %v", source, err)
				}
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("linked %s directory outside home was written: %v (%v)", name, entries, err)
			}
		})
	}
}

func linkInitDirectory(t *testing.T, location, outside string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", location, outside).CombinedOutput()
		if err != nil {
			t.Skipf("Windows directory junction unavailable: %v: %s", err, output)
		}
	} else if err := os.Symlink(outside, location); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
}
