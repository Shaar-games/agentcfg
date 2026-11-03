//go:build !windows

package main

import "syscall"

// Avoid blocking if a native skill asset is swapped for a FIFO during import.
const initNonblockRead = syscall.O_NONBLOCK
