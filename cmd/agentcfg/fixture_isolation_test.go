package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/wire"
)

func TestCLIFixturesUseTemporaryCache(t *testing.T) {
	inheritedCache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", inheritedCache)
	t.Setenv("LOCALAPPDATA", inheritedCache)
	t.Run("current-process", func(t *testing.T) {
		home := integrationRoot(t, "shared", "5s")
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		assertFixtureCacheWithinHome(t, cache, home)
	})
	t.Run("subprocess", func(t *testing.T) {
		home := t.TempDir()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("AGENTCFG_CACHE_FIXTURE", "1")
		peer := startCLIPeerCommand(t, executable, home, []string{"-test.run=^TestCLICacheFixture$"})
		message := peer.read(t)
		var cache string
		if err := json.Unmarshal(message["result"], &cache); err != nil {
			t.Fatal(err)
		}
		assertFixtureCacheWithinHome(t, cache, home)
	})
}

func assertFixtureCacheWithinHome(t *testing.T, cache, home string) {
	t.Helper()
	relative, err := filepath.Rel(home, cache)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("fixture cache %q is outside temporary home %q: %v", cache, home, err)
	}
}

func TestCLICacheFixture(t *testing.T) {
	if os.Getenv("AGENTCFG_CACHE_FIXTURE") != "1" {
		return
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		os.Exit(10)
	}
	if err := wire.NewWriter(os.Stdout).Write(wire.Result(json.RawMessage(`1`), cache)); err != nil {
		os.Exit(11)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
