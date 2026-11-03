package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentcfg/internal/config"
)

func TestAdoptionHashServerIdentity(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "work")
	local := func() config.Server {
		return config.Server{
			Name: "native", Source: "original/mcp.json", Type: "stdio",
			Command: "example-tool", Args: []string{"a", "bc"},
			Env: map[string]string{"MODE": "read", "LANG": "C"}, Cwd: cwd,
			Harnesses: []string{"codex"}, Sharing: "shared",
		}
	}
	remote := func() config.Server {
		return config.Server{
			Name: "native", Source: "original/mcp.json", Type: "streamable-http",
			URL: "https://example.invalid/mcp", Headers: map[string]string{"X-Account": "one"},
			Harnesses: []string{"codex"}, Sharing: "shared",
			OAuth: &config.OAuthConfig{
				Account: "personal", ClientID: "client", Issuer: "https://issuer.invalid",
				ClientSecretEnv: "AGENTCFG_TEST_CLIENT_SECRET",
				RedirectURL:     "http://127.0.0.1/callback", Scopes: []string{"tools:read"},
			},
		}
	}
	type mutation struct {
		name   string
		change func(*config.Server)
	}
	for _, group := range []struct {
		name    string
		server  func() config.Server
		changes []mutation
	}{
		{"stdio", local, []mutation{
			{"transport", func(s *config.Server) { s.Type = "other" }},
			{"command", func(s *config.Server) { s.Command = "different-tool" }},
			{"argument boundaries", func(s *config.Server) { s.Args = []string{"ab", "c"} }},
			{"argument order", func(s *config.Server) { s.Args = []string{"bc", "a"} }},
			{"environment", func(s *config.Server) { s.Env["MODE"] = "write" }},
			{"working directory", func(s *config.Server) { s.Cwd = filepath.Join(cwd, "other") }},
		}},
		{"http", remote, []mutation{
			{"url", func(s *config.Server) { s.URL += "/other" }},
			{"headers", func(s *config.Server) { s.Headers["X-Account"] = "two" }},
			{"oauth removal", func(s *config.Server) { s.OAuth = nil }},
			{"oauth account", func(s *config.Server) { s.OAuth.Account = "work" }},
			{"oauth client", func(s *config.Server) { s.OAuth.ClientID = "other-client" }},
			{"oauth issuer", func(s *config.Server) { s.OAuth.Issuer = "https://other.invalid" }},
			{"oauth secret source", func(s *config.Server) { s.OAuth.ClientSecretEnv = "OTHER_TEST_SECRET" }},
			{"oauth metadata", func(s *config.Server) { s.OAuth.ClientMetadataURL = "https://client.invalid/client.json" }},
			{"oauth redirect", func(s *config.Server) { s.OAuth.RedirectURL += "/other" }},
			{"oauth scopes", func(s *config.Server) { s.OAuth.Scopes = []string{"tools:write"} }},
			{"oauth registration", func(s *config.Server) { s.OAuth.DynamicRegistration = true }},
		}},
	} {
		t.Run(group.name, func(t *testing.T) {
			original := group.server()
			want := ServerSHA256(original)
			adoptionHashCheckSHA256(t, want)
			if !reflect.DeepEqual(original, group.server()) {
				t.Fatal("hashing mutated the caller's server definition")
			}
			routed := group.server()
			routed.Name, routed.Source, routed.Sharing = "renamed", "imported/mcp.json", "isolated"
			routed.Harnesses = []string{"none"}
			if got := ServerSHA256(routed); got != want {
				t.Fatal("display/source identity or routing policy changed the upstream hash")
			}
			for _, change := range group.changes {
				t.Run(change.name, func(t *testing.T) {
					changed := group.server()
					change.change(&changed)
					if ServerSHA256(changed) == want {
						t.Fatal("upstream definition changed without invalidating its hash")
					}
				})
			}
		})
	}
}

func TestAdoptionHashTreeContentAndStructure(t *testing.T) {
	makeTree := func(t *testing.T, reverse bool) string {
		t.Helper()
		root := t.TempDir()
		files := []struct{ name, content string }{
			{"SKILL.md", "---\r\nname: example\r\ndescription: Example\r\n---\r\nBody.\r\n"},
			{filepath.Join("references", "rules.txt"), "unchanged\x00bytes"},
		}
		for i := range files {
			index := i
			if reverse {
				index = len(files) - 1 - i
			}
			writeTestFile(t, filepath.Join(root, files[index].name), files[index].content)
		}
		return root
	}
	want := adoptionHashTree(t, makeTree(t, false))
	if got := adoptionHashTree(t, makeTree(t, true)); got != want {
		t.Fatal("root location or file creation order changed the content hash")
	}
	for _, change := range []struct {
		name   string
		change func(*testing.T, string)
	}{
		{"empty directory", func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "empty"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty file", func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, "empty"), "")
		}},
		{"renamed file", func(t *testing.T, root string) {
			if err := os.Rename(filepath.Join(root, "SKILL.md"), filepath.Join(root, "renamed.md")); err != nil {
				t.Fatal(err)
			}
		}},
		{"changed bytes", func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, "references", "rules.txt"), "changed\x00bytes")
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			root := makeTree(t, false)
			change.change(t, root)
			if adoptionHashTree(t, root) == want {
				t.Fatal("changed tree retained the scanned content hash")
			}
		})
	}
}

func TestAdoptionHashTreeFramesFileContents(t *testing.T) {
	twoFiles, oneFile := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(twoFiles, "a"), "prefix")
	writeTestFile(t, filepath.Join(twoFiles, "b"), "payload")
	info, err := os.Stat(filepath.Join(twoFiles, "b"))
	if err != nil {
		t.Fatal(err)
	}
	// Under the old unframed format, the bytes in a can impersonate b's
	// metadata and contents. Preserve that format only for ownership checks.
	writeTestFile(t, filepath.Join(oneFile, "a"), fmt.Sprintf("prefixb\x00%d\x00payload", info.Mode()))
	leftLegacy, err := treeFingerprint(twoFiles)
	if err != nil {
		t.Fatal(err)
	}
	rightLegacy, err := treeFingerprint(oneFile)
	if err != nil {
		t.Fatal(err)
	}
	if leftLegacy != rightLegacy {
		t.Fatal("fixture does not exercise the legacy content/structure ambiguity")
	}
	if adoptionHashTree(t, twoFiles) == adoptionHashTree(t, oneFile) {
		t.Fatal("file contents impersonated another file in the adoption proof")
	}
}

func TestAdoptionHashTreeIgnoresPermissions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "SKILL.md")
	writeTestFile(t, path, "identical content")
	want := adoptionHashTree(t, root)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	oldFingerprint, err := adoptionTreeFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Error(err)
		}
	})
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() == after.Mode() {
		t.Skip("filesystem does not expose the permission change")
	}
	if adoptionHashTree(t, root) != want {
		t.Fatal("permission-only change invalidated identical skill content")
	}
	current, err := adoptionTreeFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := treeFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if current != legacy || current == oldFingerprint {
		t.Fatal("ownership fingerprint no longer preserves permission-sensitive legacy semantics")
	}
}

func TestAdoptionHashLegacyFingerprintCompatibility(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprintf("populated=%t", populated), func(t *testing.T) {
			root := t.TempDir()
			if populated {
				writeTestFile(t, filepath.Join(root, "SKILL.md"), "skill\x00content")
				writeTestFile(t, filepath.Join(root, "references", "b"), "second")
				writeTestFile(t, filepath.Join(root, "references", "a"), "")
				if err := os.Mkdir(filepath.Join(root, "empty"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			want, err := treeFingerprint(root)
			if err != nil {
				t.Fatal(err)
			}
			got, err := adoptionTreeFingerprint(root)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("adoption fingerprint %q differs from existing ownership format %q", got, want)
			}
		})
	}
}

func TestAdoptionHashTreeRejectsLinks(t *testing.T) {
	for _, kind := range []string{"root", "nested directory", "nested file", "parent"} {
		t.Run(kind, func(t *testing.T) {
			physical, container := t.TempDir(), t.TempDir()
			writeTestFile(t, filepath.Join(physical, "skill", "SKILL.md"), "outside")
			path, target, link := container, filepath.Join(physical, "skill"), filepath.Join(container, "link")
			switch kind {
			case "root":
				path = link
			case "nested file":
				target = filepath.Join(target, "SKILL.md")
			case "parent":
				target = physical
				path = filepath.Join(link, "skill")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			adoptionHashRequireRejected(t, path, "")
		})
	}
}

func TestAdoptionHashTreeRejectsNonDirectoryRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	writeTestFile(t, path, "not a directory")
	adoptionHashRequireRejected(t, path, "physical directory")
}

func TestAdoptionHashTreeFileSizeLimit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "asset")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file exercises the real public boundary without allocating or
	// writing a 16 MiB fixture in the test process.
	err = file.Truncate(16 << 20)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("create boundary fixture: truncate=%v close=%v", err, closeErr)
	}
	adoptionHashTree(t, root)
	if err := os.Truncate(path, (16<<20)+1); err != nil {
		t.Fatal(err)
	}
	adoptionHashRequireRejected(t, root, "byte limit")
}

func TestAdoptionHashTreeDepthLimit(t *testing.T) {
	root := t.TempDir()
	nested := root
	for range 64 {
		nested = filepath.Join(nested, "d")
		if err := os.Mkdir(nested, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	adoptionHashTree(t, root)
	writeTestFile(t, filepath.Join(nested, "one-level-too-deep"), "")
	adoptionHashRequireRejected(t, root, "depth limit")
}

func TestAdoptionHashWalkerBudgets(t *testing.T) {
	path := t.TempDir()
	writeTestFile(t, filepath.Join(path, "asset"), "ab")
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// Seed already-visited totals to test the global limits with two entries
	// rather than materializing 10,000 files or a 64 MiB directory tree.
	for _, test := range []struct {
		name      string
		count     int
		total     int64
		wantError string
	}{
		{"exactly 10000 entries", 9998, 0, ""},
		{"over 10000 entries", 9999, 0, "entry/depth limit"},
		{"exactly 64 MiB total", 0, (64 << 20) - 2, ""},
		{"over 64 MiB total", 0, (64 << 20) - 1, "byte limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			count, total := test.count, test.total
			err := hashSkillTree(root, ".", "", sha256.New(), sha256.New(), 0, &count, &total)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("want %q rejection, got %v", test.wantError, err)
			}
		})
	}
}

func adoptionHashCheckSHA256(t *testing.T, value string) {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		t.Fatalf("invalid SHA-256 fingerprint %q", value)
	}
}

func adoptionHashTree(t *testing.T, path string) string {
	t.Helper()
	value, err := SkillTreeSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	adoptionHashCheckSHA256(t, value)
	return value
}

func adoptionHashRequireRejected(t *testing.T, path, reason string) {
	t.Helper()
	for _, test := range []struct {
		name string
		hash func(string) (string, error)
	}{
		{"content hash", SkillTreeSHA256},
		{"ownership fingerprint", adoptionTreeFingerprint},
	} {
		value, err := test.hash(path)
		if err == nil || value != "" || (reason != "" && !strings.Contains(err.Error(), reason)) {
			t.Errorf("%s: want refusal containing %q and no fingerprint, got %q, %v", test.name, reason, value, err)
		}
	}
}
