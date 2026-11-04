package syncer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeParentRollbackPreservesForeignFilesAndReplacements(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-user-file", true: "replaced-directory"}[replace], func(t *testing.T) {
			home := t.TempDir()
			parent := filepath.Join(home, ".cursor", "skills")
			cleanup, err := prepareNativeParent(home, filepath.Join(parent, "added"))
			if err != nil || cleanup == nil {
				t.Fatalf("prepare: %v", err)
			}
			if replace {
				if err := os.Rename(parent, filepath.Join(home, "saved-parent")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
			}
			note := filepath.Join(parent, "user-notes.txt")
			writeTestFile(t, note, "Preserve my notes.")
			err = cleanup()
			if replace && err == nil {
				t.Fatal("replacement directory identity was ignored")
			}
			assertAdoptionBytes(t, note, []byte("Preserve my notes."))
		})
	}
}
