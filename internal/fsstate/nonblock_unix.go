//go:build !windows

package fsstate

import "syscall"

const nonblockRead = syscall.O_NONBLOCK
