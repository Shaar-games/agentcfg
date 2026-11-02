package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
)

// A sync plan protects its native projections with sync.lock. TUI mutations
// must not change their canonical inputs while that plan is being applied.
func TestSettingsAndSkillEditsRespectActiveSyncLock(t *testing.T) {
	t.Run("harness and server", func(t *testing.T) {
		root := settingsFixture(t)
		original := `{"harnesses":["codex"],"serverOverrides":{}}`
		putSettings(t, root, original)
		lock := flock.New(filepath.Join(root, ".agentmux", "sync.lock"))
		locked, err := lock.TryLock()
		if err != nil || !locked {
			t.Fatalf("acquire synchronization lock: %v", err)
		}
		defer lock.Close()
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
		lock := flock.New(filepath.Join(root, ".agentmux", "sync.lock"))
		locked, err := lock.TryLock()
		if err != nil || !locked {
			t.Fatalf("acquire synchronization lock: %v", err)
		}
		defer lock.Close()
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
