package fsstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func put(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func link(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestSnapshotsExpandSharedLinksWithoutChangingTargets(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	put(t, filepath.Join(external, "folder", "file.txt"), "Keep this source.")
	link(t, filepath.Join(external, "folder"), filepath.Join(root, "one"))
	link(t, "one", filepath.Join(root, "two"))
	before, err := Capture(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	copy := filepath.Join(t.TempDir(), "copy")
	expanded, err := Copy(root, copy, Options{FollowLinks: true})
	if err != nil {
		t.Fatal(err)
	}
	physical, err := Capture(copy, Options{})
	if err != nil || physical.Layout != hexEmpty || physical.Content != expanded.Content {
		t.Fatalf("invalid expanded copy: %v", err)
	}
	for _, path := range []string{"one/file.txt", "two/file.txt"} {
		data, err := os.ReadFile(filepath.Join(copy, path))
		if err != nil || string(data) != "Keep this source." {
			t.Fatal("shared target was not copied")
		}
	}
	after, err := Capture(root, Options{})
	if err != nil || after.State != before.State {
		t.Fatal("read changed original links")
	}
	if _, err := Capture(root, Options{FollowLinks: true, Boundary: root}); err == nil {
		t.Fatal("an explicit boundary was ignored")
	}
}

const hexEmpty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestSnapshotRejectsBrokenCyclesAndLimitsWithoutWrites(t *testing.T) {
	for _, kind := range []string{"cycle", "broken", "entries", "bytes", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			put(t, filepath.Join(root, "file"), "data")
			options := Options{FollowLinks: true}
			switch kind {
			case "cycle":
				link(t, root, filepath.Join(root, "loop"))
			case "broken":
				link(t, filepath.Join(root, "missing"), filepath.Join(root, "bad"))
			case "entries":
				options.MaxEntries = 1
			case "bytes":
				options.MaxBytes = 1
			case "cancel":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				options.Context = ctx
			}
			dest := filepath.Join(t.TempDir(), "copy")
			_, err := Copy(root, dest, options)
			if err == nil {
				t.Fatal("invalid snapshot accepted")
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, "file"))
			if err != nil || string(data) != "data" {
				t.Fatal("snapshot failure modified its source")
			}
		})
	}
}

func TestIdenticalContentsDoNotHideRetargeting(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	put(t, first, "same")
	put(t, second, "same")
	path := filepath.Join(root, "alias")
	link(t, first, path)
	before, err := Capture(path, Options{FollowLinks: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	link(t, second, path)
	after, err := Capture(path, Options{FollowLinks: true})
	if err != nil || before.Content != after.Content || before.State == after.State {
		t.Fatal("link binding was not distinguished from contents")
	}
}

func TestCopyDoesNotSilentlyOmitLinks(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "source"), "keep")
	link(t, "source", filepath.Join(root, "alias"))
	if _, err := Copy(filepath.Join(root, "alias"), filepath.Join(root, "copy"), Options{}); err == nil {
		t.Fatal("copy silently omitted a link")
	}
}
