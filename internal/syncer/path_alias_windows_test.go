//go:build windows

package syncer

import "testing"

func TestPathAliasParentJunctionAdoption(t *testing.T) {
	testPathAliasParentAdoption(t, junction)
}

func TestPathAliasInteriorJunctionsRefused(t *testing.T) {
	testPathAliasInteriorLinks(t, junction)
}

func TestPathAliasJunctionRootRefused(t *testing.T) {
	testPathAliasLinkedRoot(t, junction)
}

func TestPathAliasRootReentryJunctionRefused(t *testing.T) {
	testPathAliasRootReentry(t, junction)
}
