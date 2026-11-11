package tui

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/config"
	"agentcfg/internal/pathutil"

	"github.com/gofrs/flock"
)

// A sync plan protects its native projections with sync.lock. TUI mutations
// must not change their canonical inputs while that plan is being applied.
func TestSettingsAndSkillEditsRespectActiveSyncLock(t *testing.T) {
	t.Run("harness and server", func(t *testing.T) {
		root := settingsFixture(t)
		original := `{"harnesses":["codex"],"serverOverrides":{}}`
		putSettings(t, root, original)
		lock := flock.New(filepath.Join(root, ".agentcfg", "sync.lock"))
		locked, err := lock.TryLock()
		if err != nil || !locked {
			t.Fatalf("acquire synchronization lock: %v", err)
		}
		defer lock.Close()
		if err := setHarnessEnabled(root, "codex", true, true); !errors.Is(err, errNoChange) {
			t.Fatalf("no-op attempted to acquire the active synchronization lock: %v", err)
		}
		if err := setHarnessEnabled(root, "cursor", true, false); err == nil || !strings.Contains(err.Error(), "lock") {
			t.Fatalf("TUI changed harnesses during synchronization: %v", err)
		}
		if err := setServerSharing(root, "alpha", "isolated", "shared"); err == nil || !strings.Contains(err.Error(), "lock") {
			t.Fatalf("TUI changed MCP sharing during synchronization: %v", err)
		}
		data, err := os.ReadFile(settingsFile(root))
		if err != nil || !bytes.Equal(data, []byte(original)) {
			t.Fatalf("sync-locked canonical settings changed: %q (%v)", data, err)
		}
		if err := lock.Unlock(); err != nil {
			t.Fatal(err)
		}
		if err := setHarnessEnabled(root, "cursor", true, false); err != nil {
			t.Fatalf("harness edit after synchronization: %v", err)
		}
	})

	t.Run("skill", func(t *testing.T) {
		original := "---\nname: review\ndescription: Review code\nmetadata:\n  harness: codex\n---\n# Keep me\n"
		root, source, skill := skillSourceFixture(t, original)
		lock := flock.New(filepath.Join(root, ".agentcfg", "sync.lock"))
		locked, err := lock.TryLock()
		if err != nil || !locked {
			t.Fatalf("acquire synchronization lock: %v", err)
		}
		defer lock.Close()
		if err := setSkillHarness(root, skill, "codex", true); !errors.Is(err, errNoChange) {
			t.Fatalf("skill no-op attempted to acquire the active synchronization lock: %v", err)
		}
		if err := setSkillHarness(root, skill, "codex", false); err == nil || !strings.Contains(err.Error(), "lock") {
			t.Fatalf("TUI changed skill source during synchronization: %v", err)
		}
		data, err := os.ReadFile(source)
		if err != nil || !bytes.Equal(data, []byte(original)) {
			t.Fatalf("sync-locked SKILL.md changed: %q (%v)", data, err)
		}
		if err := lock.Unlock(); err != nil {
			t.Fatal(err)
		}
		if err := setSkillHarness(root, skill, "codex", false); err != nil {
			t.Fatalf("skill edit after synchronization: %v", err)
		}
	})
}

func TestConcurrentSettingsEditsDoNotMixSnapshots(t *testing.T) {
	root := settingsFixture(t)
	original := `{"harnesses":["codex"],"runtime":{"idleTimeout":"7m"}}`
	putSettings(t, root, original)
	// updateSettings resolves the path before reading; match its spelling even
	// when the temporary directory uses a symlink or Windows short-name alias.
	settingsPath, err := pathutil.Resolve(settingsFile(root))
	if err != nil {
		t.Fatal(err)
	}
	// The other writer may replace the file during the unlocked fsstate read,
	// before the settings comparison detects the stale value.
	replacedSnapshot := "entry replaced during snapshot: " + settingsPath
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, choice := range []string{"cursor", "gemini"} {
		go func(selected string) {
			<-start
			results <- setHarnessSelection(root, []string{selected}, []string{"codex"})
		}(choice)
	}
	close(start)
	first, second := <-results, <-results
	writes := 0
	for _, err := range []error{first, second} {
		if err == nil {
			writes++
		} else if !strings.Contains(err.Error(), "changed") && err.Error() != replacedSnapshot {
			t.Fatalf("expected a stale snapshot error, got %v", err)
		}
	}
	if writes != 1 {
		t.Fatalf("wanted exactly one successful writer: %v / %v", first, second)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Settings.Harnesses) != 1 || (cfg.Settings.Harnesses[0] != "cursor" && cfg.Settings.Harnesses[0] != "gemini") || cfg.Settings.Runtime.IdleTimeout != "7m" {
		t.Fatalf("concurrent edit mixed or discarded settings: %+v", cfg.Settings)
	}
	backups, err := filepath.Glob(settingsFile(root) + ".bak-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected one backup for one successful write: %v (%v)", backups, err)
	}
	if got, err := os.ReadFile(backups[0]); err != nil || string(got) != original {
		t.Fatalf("backup no longer matches the approved preimage: %q (%v)", got, err)
	}
}
