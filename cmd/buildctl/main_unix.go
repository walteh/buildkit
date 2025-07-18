//go:build !windows

package buildctl_main

import (
	"syscall"
)

func init() {
	syscall.Umask(0)
}
