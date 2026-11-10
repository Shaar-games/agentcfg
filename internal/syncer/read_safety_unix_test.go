//go:build unix

package syncer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentcfg/internal/config"
	"golang.org/x/sys/unix"
)

func TestSyncPlanningRejectsFIFOWithoutBlocking(t *testing.T) {
	for _, relative := range []string{".cursor/mcp.json", ".agentcfg/sync-user.json", ".agentcfg/AGENTS.md"} {
		t.Run(relative, func(t *testing.T) {
			home := t.TempDir()
			safetySource(t, home)
			cfg, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := BuildPlan(cfg, Options{Root: home, Home: home, Scope: "user", Binary: filepath.Join(home, "agentcfg")})
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("FIFO was accepted as a regular configuration")
				}
			case <-time.After(time.Second):
				// Release a broken blocking reader before failing; no background
				// goroutine or open FIFO is left behind by the regression test.
				writer, openErr := os.OpenFile(path, os.O_WRONLY|unix.O_NONBLOCK, 0)
				if openErr != nil {
					t.Fatalf("sync planning blocked on FIFO; unblock: %v", openErr)
				}
				_, _ = writer.Write([]byte("{}\n"))
				_ = writer.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("sync planning kept blocking after FIFO was released")
				}
				t.Fatal("sync planning blocked reading a FIFO")
			}
		})
	}
}
