//go:build !windows

package bootstrap

import (
	"os"
	"testing"
)

func dirLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
