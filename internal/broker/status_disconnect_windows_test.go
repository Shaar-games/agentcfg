//go:build windows

package broker

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStatusRecognizesWindowsPipeClosing(t *testing.T) {
	for _, err := range []error{
		windows.ERROR_NO_DATA, // "The pipe is being closed."
		windows.ERROR_BROKEN_PIPE,
		windows.ERROR_PIPE_NOT_CONNECTED,
		fmt.Errorf("wrapped pipe close: %w", windows.ERROR_NO_DATA),
	} {
		if !statusConnectionClosed(err) {
			t.Fatalf("expected closed-connection classification for %v", err)
		}
	}
	if statusConnectionClosed(windows.ERROR_ACCESS_DENIED) {
		t.Fatal("access denied must not be treated as a normal shutdown")
	}
}
