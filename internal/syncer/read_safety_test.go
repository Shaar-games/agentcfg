package syncer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadOptionalRejectsOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate((16 << 20) + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readOptional(path); err == nil {
		t.Fatal("unbounded native configuration or ownership manifest read was accepted")
	}
}
