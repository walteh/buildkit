package buildctl_main

import (
	"testing"

	"github.com/moby/buildkit/util/testutil/integration"
	"github.com/stretchr/testify/require"
)

func testPrune(t *testing.T, sb integration.Sandbox) {
	integration.SkipOnPlatform(t, "windows")
	cmd := sb.Cmd("prune")
	err := cmd.Run()
	require.NoError(t, err)
}
