//go:build !windows

package bootstrap

import "syscall"

// Avoid blocking if a file is replaced with a FIFO between stat and open.
const nonblockRead = syscall.O_NONBLOCK
