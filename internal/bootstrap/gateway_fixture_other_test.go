//go:build !windows

package bootstrap

import (
	"os"
	"testing"
)

func privateGatewayFixtureHome(t *testing.T, home string) {
	t.Helper()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
}
