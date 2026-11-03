//go:build !windows

package syncer

import "syscall"

// A concurrently substituted FIFO must fail validation without blocking.
const adoptionNonblockRead = syscall.O_NONBLOCK
