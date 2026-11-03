//go:build windows

package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentcfg/internal/pathutil"
	"golang.org/x/sys/windows"
)

func TestDiscoveryCapturesActualNTFSShortAndLongSpellings(t *testing.T) {
	long := filepath.Join(t.TempDir(), "native home with spaces")
	if err := os.Mkdir(long, 0700); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	size, err := windows.GetShortPathName(name, &buffer[0], uint32(len(buffer)))
	if err != nil || size == 0 || size >= uint32(len(buffer)) {
		t.Skipf("NTFS 8.3 aliases unavailable: %v", err)
	}
	short := windows.UTF16ToString(buffer[:size])
	if strings.EqualFold(short, long) {
		t.Skip("filesystem did not allocate a distinct 8.3 alias")
	}
	native := put(t, short, ".cursor/mcp.json", `{"mcpServers":{"fixture":{"command":"node"}}}`)
	plan, err := Discover(short)
	if err != nil {
		t.Fatal(err)
	}
	physical, err := pathutil.Resolve(native)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Home != short || len(plan.Servers) != 1 || plan.Servers[0].Origins[0].Source != native {
		t.Fatalf("native short origin changed: %+v", plan)
	}
	a, okA := plan.SourceSHA256(native)
	b, okB := plan.SourceSHA256(physical)
	if !okA || !okB || a != b {
		t.Fatalf("short/long capture mismatch: %s %s", native, physical)
	}
	if err := plan.Revalidate(); err != nil {
		t.Fatal(err)
	}
}
