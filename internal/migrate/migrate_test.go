package migrate_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/migrate"
	"agentcfg/internal/syncer"
)

func fixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewAndApplyPreserveLegacyAndEncryptedCredentials(t *testing.T) {
	home := t.TempDir()
	current := filepath.Join(home, ".agentcfg")
	legacy := filepath.Join(home, ".agentmux")
	mcp := []byte(`{"mcpServers":{"review":{"type":"stdio","command":"node","cwd":"./.agentmux"}}}`)
	manifest := []byte(`{"version":1,"configs":{},"skills":{}}`)
	fixtureFile(t, filepath.Join(current, "mcp.json"), mcp)
	fixtureFile(t, filepath.Join(current, "agentmux.json"), []byte(`{"harnesses":["codex"]}`))
	fixtureFile(t, filepath.Join(current, "sync-user.json"), manifest)
	fixtureFile(t, filepath.Join(current, "skills", "review", "SKILL.md"), []byte("---\nname: review\ndescription: Test\n---\n"))
	key := bytes.Repeat([]byte{0x47}, 32)
	store, err := auth.NewEncryptedFileStore(home, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), "review:account", []byte("encrypted-token-secret")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(current, legacy); err != nil {
		t.Fatal(err)
	}
	report, err := migrate.Preview(home)
	if err != nil {
		t.Fatal(err)
	}
	if report.Files < 5 || report.Directories < 3 {
		t.Fatalf("incomplete preview: %+v", report)
	}
	if _, err := os.Lstat(current); !os.IsNotExist(err) {
		t.Fatalf("preview changed the filesystem: %v", err)
	}
	if _, err := migrate.Apply(home); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(current, "mcp.json")); err != nil || !bytes.Equal(data, mcp) {
		t.Fatalf("MCP source not preserved: %v, %q", err, data)
	}
	if data, err := os.ReadFile(filepath.Join(current, "sync-user.json")); err != nil || !bytes.Equal(data, manifest) {
		t.Fatalf("ownership manifest not preserved: %v, %q", err, data)
	}
	if _, err := os.Stat(filepath.Join(current, "agentcfg.json")); err != nil {
		t.Fatalf("settings filename not migrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(current, "agentmux.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy settings filename left behind: %v", err)
	}
	if loaded, err := config.Load(home); err != nil || loaded.Settings.Harnesses[0] != "codex" {
		t.Fatalf("migrated canonical settings invalid: %v", err)
	}
	newStore, err := auth.NewEncryptedFileStore(home, key)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := newStore.Load(context.Background(), "review:account")
	if err != nil || string(secret) != "encrypted-token-secret" {
		t.Fatalf("cannot decrypt migrated OAuth secret: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "agentmux.json")); err != nil {
		t.Fatalf("legacy settings lost: %v", err)
	}
	if _, err := migrate.Apply(home); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second migration should reject existing target: %v", err)
	}
}

func TestPreviewRefusesExistingDestinationWithoutOverwrite(t *testing.T) {
	home := t.TempDir()
	fixtureFile(t, filepath.Join(home, ".agentmux", "agentmux.json"), []byte("{}"))
	fixtureFile(t, filepath.Join(home, ".agentcfg", "agentcfg.json"), []byte("do not overwrite"))
	if _, err := migrate.Preview(home); err == nil {
		t.Fatal("migration should refuse conflicting canonical roots")
	}
	if data, err := os.ReadFile(filepath.Join(home, ".agentcfg", "agentcfg.json")); err != nil || string(data) != "do not overwrite" {
		t.Fatalf("existing target modified: %v", err)
	}
}

func TestPreviewRefusesRedirectedSourceAndDoesNotStage(t *testing.T) {
	home := t.TempDir()
	fixtureFile(t, filepath.Join(home, ".agentmux", "mcp.json"), []byte("{}"))
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, ".agentmux", "plugins")); err != nil {
		t.Skipf("filesystem does not allow creating symlinks: %v", err)
	}
	if _, err := migrate.Preview(home); err == nil || !strings.Contains(err.Error(), "linked/reparse") {
		t.Fatalf("redirected path accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agentcfg")); !os.IsNotExist(err) {
		t.Fatalf("migration unexpectedly created target: %v", err)
	}
}

func TestPreviewRefusesLegacyRootLink(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, ".agentmux")); err != nil {
		t.Skipf("filesystem does not allow symlinks: %v", err)
	}
	if _, err := migrate.Preview(home); err == nil {
		t.Fatal("linked legacy directory accepted")
	}
}

func TestMigratedOwnershipManifestReusesNativeEntries(t *testing.T) {
	home := t.TempDir()
	current := filepath.Join(home, ".agentcfg")
	fixtureFile(t, filepath.Join(current, "agentcfg.json"), []byte(`{"harnesses":["claude-code"]}`))
	fixtureFile(t, filepath.Join(current, "mcp.json"), []byte(`{"mcpServers":{"local":{"type":"stdio","command":"node","args":["server.js"]}}}`))
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	oldBinary := filepath.Join(home, "agentmux.exe")
	newBinary := filepath.Join(home, "agentcfg.exe")
	oldPlan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Binary: oldBinary, Scope: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if err := oldPlan.Apply(); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(home, ".claude.json")
	before, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(before, []byte("agentmux.exe")) {
		t.Fatalf("legacy sync did not write expected proxy: %s", before)
	}
	if err := os.Rename(current, filepath.Join(home, ".agentmux")); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(home); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(native)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("migration edited native client file: %v", err)
	}
	cfg, err = config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := syncer.BuildPlan(cfg, syncer.Options{Root: home, Home: home, Binary: newBinary, Scope: "user"})
	if err != nil {
		t.Fatalf("migrated ownership records treated native proxies as unowned: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected a planned replacement of old proxy binary without applying it")
	}
}
