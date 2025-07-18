package oci

import (
	"context"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/continuity/fs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// func sub(m mount.Mount, subPath string) (mount.Mount, func() error, error) {
// 	src, err := fs.RootPath(m.Source, subPath)
// 	if err != nil {
// 		return mount.Mount{}, nil, err
// 	}
// 	m.Source = src
// 	return m, func() error { return nil }, nil
// }

// func generateCDIOpts(_ *cdidevices.Manager, devices []*pb.CDIDevice) ([]oci.SpecOpts, error) {
// 	if len(devices) == 0 {
// 		return nil, nil
// 	}
// 	return nil, errors.New("no support for CDI on Darwin")
// }

// withDefaultProfile sets the default seccomp profile to the spec.
// Note: must follow the setting of process capabilities
func withDefaultProfile() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		var err error
		// s.Linux.Seccomp, err = seccomp.GetDefaultProfile(s)
		return err
	}
}

func sub(m mount.Mount, subPath string) (mount.Mount, func() error, error) {
	src, err := fs.RootPath(m.Source, subPath)
	if err != nil {
		return mount.Mount{}, nil, err
	}
	m.Source = src
	return m, func() error { return nil }, nil
}
