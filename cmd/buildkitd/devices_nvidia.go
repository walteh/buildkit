//go:build nvidia
// +build nvidia

package buildkitd_main

import (
	_ "github.com/moby/buildkit/contrib/cdisetup/nvidia"
)
