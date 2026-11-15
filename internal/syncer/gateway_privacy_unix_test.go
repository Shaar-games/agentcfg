//go:build !windows

package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/auth"
	"agentcfg/internal/pathutil"
)

func privateGatewayFixtureHome(t *testing.T, home string) {
	t.Helper()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// The general sync fixture creates directories as 0755. Credential storage
	// requires each canonical component itself to be private, independently of
	// the home ancestor that protects an ordinary native configuration.
	canonical := filepath.Join(home, ".agentcfg")
	if err := os.MkdirAll(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayProjectionAcceptsReadableFileInsidePrivateHome(t *testing.T) {
	cfg, opts, native := gatewaySyncFixture(t, "cursor")
	cfg = gatewayFixtureSettings(t, cfg, true)
	if err := os.Chmod(cfg.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(native.path, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatalf("private ancestor did not protect the native configuration: %v", err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(native.path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatal("projection silently changed native permissions")
	}
}

func TestGatewayProjectionRefusesPrivacyChangeSincePlanning(t *testing.T) {
	cfg, opts, native := gatewaySyncFixture(t, "cursor")
	cfg = gatewayFixtureSettings(t, cfg, true)
	// The immediate parent is Go's temporary directory for this test. Only that
	// test-owned directory and this fixture may have their permissions changed.
	testParent := filepath.Dir(cfg.Root)
	for ancestor := filepath.Dir(testParent); ; ancestor = filepath.Dir(ancestor) {
		info, err := os.Stat(ancestor)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o011 != 0o011 {
			t.Skip("runner temporary directory has a private ancestor; public-path case is covered by auth permission-chain tests")
		}
		if filepath.Dir(ancestor) == ancestor {
			break
		}
	}
	parentInfo, err := os.Stat(testParent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(testParent, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(testParent, parentInfo.Mode().Perm()) })
	for path, mode := range map[string]os.FileMode{cfg.Root: 0o700, filepath.Dir(native.path): 0o755, native.path: 0o644} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	_, before := gatewayNativeEntries(t, native)
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.Root, 0o700) })
	privacyErr := auth.CheckGatewayConfigPrivacy(native.path)
	if privacyErr == nil {
		t.Fatal("public fixture was not rejected by the privacy guard")
	}
	if err := plan.Apply(); err == nil || !strings.Contains(err.Error(), privacyErr.Error()) {
		t.Fatalf("permissions changed after preview were not refused: %v", err)
	}
	assertAdoptionBytes(t, native.path, before)
	if _, err := BuildPlan(cfg, opts); err == nil {
		t.Fatal("public native configuration accepted a secret gateway route")
	}
}

func TestGatewayLayoutPublicationKeepsPrivateAncestry(t *testing.T) {
	for _, test := range []struct {
		name                 string
		homeMode, nativeMode os.FileMode
		allowed              bool
	}{
		{"private home", 0700, 0755, true},
		{"private moved directory", 0755, 0700, true},
		{"lost private ancestor", 0755, 0755, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, err := pathutil.Resolve(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if test.homeMode == 0755 {
				testParent := filepath.Dir(home)
				for ancestor := filepath.Dir(testParent); ; ancestor = filepath.Dir(ancestor) {
					info, err := os.Stat(ancestor)
					if err != nil {
						t.Fatal(err)
					}
					if info.Mode().Perm()&0011 != 0011 {
						t.Skip("runner has a private temporary ancestor; future permission-chain rules are covered by auth tests")
					}
					if filepath.Dir(ancestor) == ancestor {
						break
					}
				}
				info, err := os.Stat(testParent)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(testParent, 0755); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(testParent, info.Mode().Perm()) })
			}
			if err := os.Chmod(home, test.homeMode); err != nil {
				t.Fatal(err)
			}
			canonical := filepath.Join(home, ".agentcfg")
			if err := os.Mkdir(canonical, 0700); err != nil {
				t.Fatal(err)
			}
			if err := CreateInitRestorePoint(home); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(canonical, initRestoreFile)
			markerBefore, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(home, "private", "actual")
			if err := os.MkdirAll(source, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(source, test.nativeMode); err != nil {
				t.Fatal(err)
			}
			data := gatewayPrivacySamples()["json unicode slash"]
			file := filepath.Join(source, "mcp.json")
			if err := os.WriteFile(file, data, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(file, 0644); err != nil {
				t.Fatal(err)
			}
			if err := auth.CheckGatewayConfigPrivacy(file); err != nil {
				t.Fatalf("original configuration must be private: %v", err)
			}
			native := filepath.Join(home, ".cursor")
			if err := os.Symlink(source, native); err != nil {
				t.Fatal(err)
			}
			err = MaterializeInitLinks(context.Background(), home, []string{"cursor"}, nil)
			if (err == nil) != test.allowed {
				t.Fatalf("publication privacy: allowed=%v error=%v", test.allowed, err)
			}
			assertAdoptionBytes(t, file, data)
			if !test.allowed {
				if got, err := os.Readlink(native); err != nil || got != source {
					t.Fatal("unsafe publication detached the original private alias")
				}
				assertAdoptionBytes(t, marker, markerBefore)
				return
			}
			if info, err := os.Lstat(native); err != nil || !info.IsDir() {
				t.Fatal("safe native layout was not materialized")
			}
			if err := auth.CheckGatewayConfigPrivacy(filepath.Join(native, "mcp.json")); err != nil {
				t.Fatalf("published native copy lost confidentiality: %v", err)
			}
		})
	}
}
