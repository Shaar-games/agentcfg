package fsstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFileGuardRejectsBeforeWritingSourceBytes(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	destination := filepath.Join(root, "copy.txt")
	data := []byte("sensitive fixture content")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("destination privacy check rejected the copy")
	checked := 0
	options := Options{BeforeCopyFile: func(actualSource, actualDestination string, actualData []byte, mode os.FileMode) error {
		checked++
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil || actualSource != resolved || actualDestination != destination {
			t.Fatalf("wrong copy boundary: %q → %q (%v)", actualSource, actualDestination, err)
		}
		if _, err := os.Lstat(destination); !os.IsNotExist(err) {
			t.Fatal("destination existed before its guard")
		}
		info, err := os.Stat(source)
		if err != nil || string(actualData) != string(data) || mode != info.Mode().Perm() {
			t.Fatalf("guard received different copy contents or mode: %v", err)
		}
		return denied
	}}
	if _, err := Copy(source, destination, options); !errors.Is(err, denied) {
		t.Fatalf("copy bypassed rejection: %v", err)
	}
	if checked != 1 {
		t.Fatalf("copy guard called %d times", checked)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("rejected copy wrote a destination")
	}
	if _, err := Capture(source, options); err != nil {
		t.Fatal(err)
	}
	if checked != 1 {
		t.Fatal("read-only capture invoked a write guard")
	}
	options.BeforeCopyFile = func(_, actualDestination string, _ []byte, _ os.FileMode) error {
		if actualDestination != destination {
			t.Fatal("authorized destination changed")
		}
		return nil
	}
	if _, err := Copy(source, destination, options); err != nil {
		t.Fatal(err)
	}
	if copied, err := os.ReadFile(destination); err != nil || string(copied) != string(data) {
		t.Fatalf("authorized copy lost bytes: %q %v", copied, err)
	}
}

func TestCopyFileGuardCopiesOnlyValidatedBytes(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.txt")
	destination := filepath.Join(root, "copy.txt")
	checked := []byte("checked fixture content")
	changed := []byte("changed fixture content")
	if err := os.WriteFile(source, checked, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	options := Options{BeforeCopyFile: func(actualSource, _ string, data []byte, mode os.FileMode) error {
		if string(data) != string(checked) || mode != before.Mode().Perm() {
			t.Fatalf("guard did not inspect the original bytes and copy mode: %q %v", data, mode)
		}
		// Preserve identity, size and timestamp so a later stat cannot establish
		// that the bytes written are the same bytes the guard inspected.
		if err := os.WriteFile(actualSource, changed, before.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(actualSource, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal(err)
		}
		return nil
	}}
	if _, err := Copy(source, destination, options); err != nil {
		t.Fatal(err)
	}
	if copied, err := os.ReadFile(destination); err != nil || string(copied) != string(checked) {
		t.Fatalf("copy wrote bytes its guard never inspected: %q %v", copied, err)
	}
}
