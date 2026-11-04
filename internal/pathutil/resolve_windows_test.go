//go:build windows

package pathutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestResolveNormalizesNTFSShortAndLongPath(t *testing.T) {
	long := filepath.Join(t.TempDir(), "native-config")
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	ptr, err := windows.UTF16PtrFromString(WindowsAPIPath(long))
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetShortPathName(ptr, &buffer[0], uint32(len(buffer)))
	if err != nil || length == 0 || length >= uint32(len(buffer)) {
		t.Skipf("NTFS short aliases unavailable: %v", err)
	}
	short := normalWindowsPath(windows.UTF16ToString(buffer[:length]))
	if strings.EqualFold(short, long) {
		t.Skip("filesystem did not assign a distinct 8.3 path")
	}
	resolvedLong, err := Resolve(long)
	if err != nil {
		t.Fatal(err)
	}
	resolvedShort, err := Resolve(short)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(resolvedLong, resolvedShort) {
		t.Fatalf("short/long aliases did not resolve to the same path: %q != %q", resolvedLong, resolvedShort)
	}
}
