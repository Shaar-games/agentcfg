//go:build windows

package main

// Windows does not expose a Unix-style O_NONBLOCK flag. The anchored-open
// identity and regular-file checks still reject substituted special files.
const initNonblockRead = 0
