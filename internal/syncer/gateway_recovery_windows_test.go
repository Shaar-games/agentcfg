//go:build windows

package syncer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agentcfg/internal/auth"
	"agentcfg/internal/config"
	"agentcfg/internal/fsstate"
)

func gatewayRecoveryFixture(t *testing.T) (*config.Config, Options, target, []byte, string) {
	t.Helper()
	cfg, opts, native := gatewaySyncFixture(t, "cursor")
	cfg = gatewayFixtureSettings(t, cfg, true)
	applyGatewayFixture(t, cfg, opts)
	entries, before := gatewayNativeEntries(t, native)
	resource := entries[gatewayFixtureServer].(map[string]any)["url"].(string)
	return cfg, opts, native, before, resource
}

func publicGatewayRecoveryDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	setGatewayFixtureDACL(t, path, "(A;OICI;GR;;;WD)")
}

func assertNoGatewayRecoveryCopy(t *testing.T, path, resource string) {
	t.Helper()
	err := filepath.WalkDir(path, func(path string, entry os.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		leaked := bytes.Contains(data, []byte(resource))
		var point initRestorePoint
		if json.Unmarshal(data, &point) == nil {
			for _, native := range point.Native {
				leaked = leaked || bytes.Contains(native.Before, []byte(resource))
			}
		}
		if leaked {
			t.Errorf("capability copied to an unprotected recovery file: %s", filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGatewayRecoveryCopiesRejectPublicWindowsACL(t *testing.T) {
	for _, location := range []string{"canonical", "backups"} {
		for _, transition := range []string{"update", "disable", "remove-last-http"} {
			t.Run(location+"/"+transition, func(t *testing.T) {
				cfg, opts, native, before, resource := gatewayRecoveryFixture(t)
				public := filepath.Join(cfg.Root, ".agentcfg")
				if location == "backups" {
					public = filepath.Join(public, "backups")
				}
				publicGatewayRecoveryDirectory(t, public)
				if err := auth.CheckGatewayConfigPrivacy(native.path); err != nil {
					t.Fatalf("native fixture must remain private: %v", err)
				}
				switch transition {
				case "update":
					cfg.Settings.Gateway.Listen = "127.0.0.1:43878"
					cfg = gatewayFixtureSettings(t, cfg, true)
				case "disable":
					cfg = gatewayFixtureSettings(t, cfg, false)
				case "remove-last-http":
					writeTestFile(t, filepath.Join(cfg.Root, ".agentcfg", "mcp.json"), `{"mcpServers":{"local":{"command":"inert-fixture-command"}}}`)
					var err error
					cfg, err = config.Load(cfg.Root)
					if err != nil {
						t.Fatal(err)
					}
				}
				plan, err := BuildPlan(cfg, opts)
				if err == nil {
					err = plan.Apply()
				}
				if err == nil {
					t.Error("sync accepted a public backup destination for the previous capability")
				}
				assertNoGatewayRecoveryCopy(t, filepath.Join(cfg.Root, ".agentcfg", "backups"), resource)
				assertAdoptionBytes(t, native.path, before)
			})
		}
	}
}

func TestGatewayRestoreJournalRejectsPublicWindowsACL(t *testing.T) {
	t.Run("new baseline", func(t *testing.T) {
		cfg, opts, native, before, resource := gatewayRecoveryFixture(t)
		if err := CreateInitRestorePoint(cfg.Root); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(cfg.Root, ".agentcfg", initRestoreFile)
		markerBefore, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		publicGatewayRecoveryDirectory(t, filepath.Join(cfg.Root, ".agentcfg"))
		cfg = gatewayFixtureSettings(t, cfg, false)
		plan, err := BuildPlan(cfg, opts)
		if err == nil {
			err = plan.Apply()
		}
		if err == nil {
			t.Error("sync stored a capability in the public restore journal")
		}
		assertNoGatewayRecoveryCopy(t, marker, resource)
		assertNoGatewayRecoveryCopy(t, filepath.Join(cfg.Root, ".agentcfg", "backups"), resource)
		assertAdoptionBytes(t, native.path, before)
		assertAdoptionBytes(t, marker, markerBefore)
	})
	t.Run("previous journal backup", func(t *testing.T) {
		cfg, opts, native, before, resource := gatewayRecoveryFixture(t)
		relative, err := filepath.Rel(cfg.Root, native.path)
		if err != nil {
			t.Fatal(err)
		}
		point := initRestorePoint{Version: 1, Native: []restoreEntry{{Path: filepath.ToSlash(relative), BeforeExists: true,
			Before: before, BeforeHash: rawDigest(before), AfterHash: rawDigest(before)}}}
		encoded, err := json.MarshalIndent(point, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(cfg.Root, ".agentcfg", initRestoreFile)
		writeTestFile(t, marker, string(encoded))
		cfg.Settings.Gateway.Listen = "127.0.0.1:43878"
		cfg = gatewayFixtureSettings(t, cfg, true)
		plan, err := BuildPlan(cfg, opts)
		if err != nil {
			t.Fatal(err)
		}
		backupRoot := filepath.Join(cfg.Root, ".agentcfg", "backups", "public-next")
		publicGatewayRecoveryDirectory(t, backupRoot)
		if _, err := plan.captureInitRestore(backupRoot); err == nil {
			t.Error("previous journal copied its base64-encoded capability into a public backup")
		}
		assertNoGatewayRecoveryCopy(t, backupRoot, resource)
		assertAdoptionBytes(t, marker, encoded)
	})
}

func TestGatewayRecoveryPrivacyIsRecheckedAfterPreview(t *testing.T) {
	cfg, opts, native, before, resource := gatewayRecoveryFixture(t)
	cfg = gatewayFixtureSettings(t, cfg, false)
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(cfg.Root, ".agentcfg", "backups")
	publicGatewayRecoveryDirectory(t, backups)
	if err := plan.Apply(); err == nil {
		t.Fatal("apply accepted backup permissions changed after preview")
	}
	assertNoGatewayRecoveryCopy(t, backups, resource)
	assertAdoptionBytes(t, native.path, before)
}

func TestGatewaySensitiveWritesRejectPublicWindowsACL(t *testing.T) {
	for name, data := range gatewayPrivacySamples() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			privateGatewayFixtureHome(t, root)
			public := filepath.Join(root, "public")
			publicGatewayRecoveryDirectory(t, public)
			path := filepath.Join(public, "backup")
			if err := atomicWrite(path, data, 0600); err == nil {
				t.Fatal("escaped capability was written into a public backup")
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("rejected sensitive write created its destination: %v", err)
			}
			entries, err := os.ReadDir(public)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected sensitive write left temporary files: %v", err)
			}
			ordinary := []byte(`{"url":"https://example.test/mcp"}`)
			if err := atomicWrite(path, ordinary, 0600); err != nil {
				t.Fatalf("ordinary content acquired a gateway permission restriction: %v", err)
			}
			assertAdoptionBytes(t, path, ordinary)
		})
	}
}

func TestGatewayRollbackRejectsPublicWindowsACL(t *testing.T) {
	cfg, opts, native, _, _ := gatewayRecoveryFixture(t)
	cfg = gatewayFixtureSettings(t, cfg, false)
	plan, err := BuildPlan(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	var change *Change
	for i := range plan.Changes {
		if plan.Changes[i].Path == native.path {
			change = &plan.Changes[i]
		}
	}
	if change == nil {
		t.Fatal("missing gateway deactivation change")
	}
	undo, err := plan.applyPreparedChange(*change, filepath.Join(cfg.Root, ".agentcfg", "backups", "rollback", "mcp.json"))
	if err != nil || undo == nil {
		t.Fatalf("prepare rollback fixture: %v", err)
	}
	publicGatewayRecoveryDirectory(t, filepath.Dir(native.path))
	if err := undo(); err == nil {
		t.Fatal("rollback published the original capability in a now-public native file")
	}
	assertAdoptionBytes(t, native.path, change.after)
}

func TestGatewayLayoutCopiesRejectPublicWindowsACL(t *testing.T) {
	for name, data := range gatewayPrivacySamples() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			privateGatewayFixtureHome(t, root)
			source := filepath.Join(root, "settings.dat")
			writeTestFile(t, source, string(data))
			if err := os.Chmod(source, 0644); err != nil {
				t.Fatal(err)
			}
			public := filepath.Join(root, "public")
			publicGatewayRecoveryDirectory(t, public)
			destination := filepath.Join(public, "baseline")
			if _, err := fsstate.Copy(source, destination, layoutOptions(true)); err == nil {
				t.Fatal("native layout copied a capability into a public destination")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("layout guard ran after creation of the destination: %v", err)
			}
			private := filepath.Join(root, "private-copy")
			if _, err := fsstate.Copy(source, private, layoutOptions(true)); err != nil {
				t.Fatalf("private layout copy was refused: %v", err)
			}
			assertAdoptionBytes(t, private, data)
			ordinary := []byte("ordinary native instructions")
			writeTestFile(t, source, string(ordinary))
			if _, err := fsstate.Copy(source, destination, layoutOptions(true)); err != nil {
				t.Fatalf("ordinary layout copy acquired a gateway restriction: %v", err)
			}
			assertAdoptionBytes(t, destination, ordinary)
		})
	}
}
