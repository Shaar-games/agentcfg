package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
)

func TestInitNativeMultiSelectKeepsPreselectedHarnesses(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var output bytes.Buffer
	selected, err := terminalHarnessSelection(context.Background(), strings.NewReader("0\n"), &output, []string{"codex", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(selected, []string{"codex", "cursor"}) {
		t.Fatalf("default Huh choices not preserved: %v; output: %s", selected, output.String())
	}
}
