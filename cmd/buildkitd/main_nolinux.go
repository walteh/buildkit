//go:build !linux

package buildkitd_main

import (
	"os"

	"github.com/moby/sys/reexec"
)

func init() {
	if reexec.Init() {
		os.Exit(0)
	}
}
