//go:build !windows

package auth

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type gatewayUnixTestInfo struct {
	mode os.FileMode
	uid  uint32
}

func (i gatewayUnixTestInfo) Name() string       { return "fixture" }
func (i gatewayUnixTestInfo) Size() int64        { return 0 }
func (i gatewayUnixTestInfo) Mode() os.FileMode  { return i.mode }
func (i gatewayUnixTestInfo) ModTime() time.Time { return time.Time{} }
func (i gatewayUnixTestInfo) IsDir() bool        { return i.mode.IsDir() }
func (i gatewayUnixTestInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }

func TestGatewayConfigUnixPrivacyRules(t *testing.T) {
	const uid = uint32(1000)
	file := func(mode os.FileMode, owner uint32) os.FileInfo { return gatewayUnixTestInfo{mode: mode, uid: owner} }
	dir := func(mode os.FileMode, owner uint32) os.FileInfo { return file(os.ModeDir|mode, owner) }
	root := dir(0755, 0)
	for _, tc := range []struct {
		name    string
		file    os.FileInfo
		parents []os.FileInfo
		allowed bool
	}{
		{"private-file-public-parent", file(0600, uid), []os.FileInfo{dir(0755, uid), root}, true},
		{"public-file-public-parent", file(0644, uid), []os.FileInfo{dir(0755, uid), root}, false},
		{"public-file-private-parent", file(0644, uid), []os.FileInfo{dir(0700, uid), root}, true},
		{"public-file-private-ancestor", file(0666, uid), []os.FileInfo{dir(0777, uid), dir(0700, uid), root}, true},
		{"new-private-file", nil, []os.FileInfo{dir(0755, uid), root}, true},
		{"new-shared-parent", nil, []os.FileInfo{dir(0777, uid), root}, false},
		{"new-shared-sticky-parent", nil, []os.FileInfo{dir(0777|os.ModeSticky, 0), root}, false},
		{"private-under-system-temp", file(0600, uid), []os.FileInfo{dir(0700, uid), dir(0777|os.ModeSticky, 0), root}, true},
		{"unsafe-ancestor-replacement", file(0600, uid), []os.FileInfo{dir(0700, uid), dir(0777, 0), root}, false},
		{"foreign-file-owner", file(0600, uid+1), []os.FileInfo{dir(0700, uid), root}, false},
		{"foreign-parent-owner", file(0600, uid), []os.FileInfo{dir(0700, uid+1), root}, false},
		{"no-parent-evidence", file(0600, uid), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkGatewayUnixPrivacy(tc.file, tc.parents, uid)
			if (err == nil) != tc.allowed {
				t.Fatalf("privacy result: allowed=%v error=%v", tc.allowed, err)
			}
		})
	}
}

func TestGatewayCopyPrivacyUsesFutureMode(t *testing.T) {
	const uid = uint32(1000)
	info := func(mode os.FileMode) os.FileInfo { return gatewayUnixTestInfo{mode: mode, uid: uid} }
	root := gatewayUnixTestInfo{mode: os.ModeDir | 0755, uid: 0}
	for _, test := range []struct {
		name           string
		file           os.FileInfo
		parent, copied os.FileMode
		allowed        bool
	}{
		{"new private copy public parent", nil, 0755, 0600, true},
		{"new public copy public parent", nil, 0755, 0644, false},
		{"new public copy private parent", nil, 0700, 0644, true},
		{"private target future public copy", info(0600), 0755, 0644, false},
		{"public target future private copy", info(0644), 0755, 0600, false},
		{"public target private parent", info(0644), 0700, 0644, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			parents := []os.FileInfo{info(os.ModeDir | test.parent), root}
			err := checkGatewayUnixCopyPrivacy(test.file, parents, uid, test.copied)
			if (err == nil) != test.allowed {
				t.Fatalf("copy privacy result: allowed=%v error=%v", test.allowed, err)
			}
		})
	}
}

func TestGatewayConfigPrivacyUnixDoesNotModifyFiles(t *testing.T) {
	// macOS commonly exposes its temporary directory through /var -> /private/var.
	// Normalize the fixture root, as config loading does, while retaining the
	// redirected target/parent rejection checks below.
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "mcp.json")
	data := []byte("temporary fixture")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := CheckGatewayConfigPrivacy(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(data) {
		t.Fatalf("fixture content changed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("fixture mode changed: %v", err)
	}
	future := filepath.Join(parent, "missing", "mcp.json")
	if err := CheckGatewayConfigPrivacy(future); err != nil {
		t.Fatal(err)
	}
	if err := CheckGatewayConfigCopyPrivacy(future, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(future)); !os.IsNotExist(err) {
		t.Fatalf("check created a directory: %v", err)
	}
	link := filepath.Join(parent, "linked.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckGatewayConfigPrivacy(link); err == nil {
		t.Fatal("redirected file accepted")
	}
	linkedParent := filepath.Join(parent, "linked")
	if err := os.Symlink(parent, linkedParent); err != nil {
		t.Fatal(err)
	}
	if err := CheckGatewayConfigPrivacy(filepath.Join(linkedParent, "mcp.json")); err == nil {
		t.Fatal("redirected parent accepted")
	}
}
