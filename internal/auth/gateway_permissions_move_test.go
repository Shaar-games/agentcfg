package auth

import (
	"os"
	"path/filepath"
	"testing"

	"agentcfg/internal/pathutil"
)

func TestGatewayMovePathsUseFutureParents(t *testing.T) {
	root, err := pathutil.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "old-private", "staged")
	destination := filepath.Join(root, "native-parent", "native")
	for _, path := range []string{filepath.Join(source, "nested"), filepath.Dir(destination)} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	file, parents, moved, err := gatewayMovePaths(source, destination, "nested/mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	if file != filepath.Join(source, "nested", "mcp.json") || moved != 2 || len(parents) < 3 || parents[0] != filepath.Join(source, "nested") || parents[1] != source || parents[2] != filepath.Dir(destination) {
		t.Fatal("relocation did not combine moved-directory modes with the future destination parents")
	}
	for _, parent := range parents {
		if parent == filepath.Dir(source) || parent == destination {
			t.Fatal("old private ancestry or replaced native entry was used to certify the future location")
		}
	}
	for _, relative := range []string{"../outside", "nested/../../outside", filepath.Join(root, "absolute")} {
		if _, _, _, err := gatewayMovePaths(source, destination, relative); err == nil {
			t.Fatal("relocation accepted a file outside its moved root")
		}
	}
}
