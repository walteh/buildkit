package oci

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver/llbsolver/cdidevices"
	"github.com/moby/buildkit/util/network"
	rootlessmountopts "github.com/moby/buildkit/util/rootless/mountopts"
	"github.com/moby/buildkit/util/system"
	traceexec "github.com/moby/buildkit/util/tracing/exec"
	"github.com/moby/sys/user"
	"github.com/moby/sys/userns"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/selinux/go-selinux"
	"github.com/pkg/errors"
)

// ProcessMode configures PID namespaces
type ProcessMode int

const (
	// ProcessSandbox unshares pidns and mount procfs.
	ProcessSandbox ProcessMode = iota
	// NoProcessSandbox uses host pidns and bind-mount procfs.
	// Note that NoProcessSandbox allows build containers to kill (and potentially ptrace) an arbitrary process in the BuildKit host namespace.
	// NoProcessSandbox should be enabled only when the BuildKit is running in a container as an unprivileged user.
	NoProcessSandbox
)

var tracingEnvVars = []string{
	"OTEL_TRACES_EXPORTER=otlp",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + getTracingSocket(),
	"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=grpc",
}

func (pm ProcessMode) String() string {
	switch pm {
	case ProcessSandbox:
		return "sandbox"
	case NoProcessSandbox:
		return "no-sandbox"
	default:
		return ""
	}
}

// Ideally we don't have to import whole containerd just for the default spec

// GenerateSpec generates spec using containerd functionality.
// opts are ignored for s.Process, s.Hostname, and s.Mounts .
func GenerateSpec(ctx context.Context, meta executor.Meta, mounts []executor.Mount, id, resolvConf, hostsFile string, namespace network.Namespace, cgroupParent string, processMode ProcessMode, idmap *user.IdentityMapping, apparmorProfile string, selinuxB bool, tracingSocket string, cdiManager *cdidevices.Manager, opts ...oci.SpecOpts) (*specs.Spec, func(), error) {
	c := &containers.Container{
		ID: id,
	}

	if len(meta.CgroupParent) > 0 {
		cgroupParent = meta.CgroupParent
	}
	if cgroupParent != "" {
		var cgroupsPath string
		lastSeparator := cgroupParent[len(cgroupParent)-1:]
		if strings.Contains(cgroupParent, ".slice") && lastSeparator == ":" {
			cgroupsPath = cgroupParent + id
		} else {
			cgroupsPath = filepath.Join("/", cgroupParent, "buildkit", id)
		}
		opts = append(opts, oci.WithCgroup(cgroupsPath))
	}

	// containerd/oci.GenerateSpec requires a namespace, which
	// will be used to namespace specs.Linux.CgroupsPath if generated
	if _, ok := namespaces.Namespace(ctx); !ok {
		ctx = namespaces.WithNamespace(ctx, "buildkit")
	}

	opts = append(opts, generateMountOpts(resolvConf, hostsFile)...)

	if securityOpts, err := generateSecurityOpts(meta.SecurityMode, apparmorProfile, selinuxB); err == nil {
		opts = append(opts, securityOpts...)
	} else {
		return nil, nil, err
	}

	if processModeOpts, err := generateProcessModeOpts(processMode); err == nil {
		opts = append(opts, processModeOpts...)
	} else {
		return nil, nil, err
	}

	if idmapOpts, err := generateIDmapOpts(idmap); err == nil {
		opts = append(opts, idmapOpts...)
	} else {
		return nil, nil, err
	}

	if rlimitsOpts, err := generateRlimitOpts(meta.Ulimit); err == nil {
		opts = append(opts, rlimitsOpts...)
	} else {
		return nil, nil, err
	}

	hostname := defaultHostname
	if meta.Hostname != "" {
		hostname = meta.Hostname
	}

	if tracingSocket != "" {
		// https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/protocol/exporter.md
		meta.Env = append(meta.Env, tracingEnvVars...)
		meta.Env = append(meta.Env, traceexec.Environ(ctx)...)
	}

	opts = append(opts,
		withProcessArgs(meta.Args...),
		oci.WithEnv(meta.Env),
		oci.WithProcessCwd(meta.Cwd),
		oci.WithNewPrivileges,
		oci.WithHostname(hostname),
	)

	if cdiManager != nil {
		if cdiOpts, err := generateCDIOpts(cdiManager, meta.CDIDevices); err == nil {
			opts = append(opts, cdiOpts...)
		} else {
			return nil, nil, err
		}
	}

	var s *oci.Spec
	var err error

	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		// note: if we use the WithPlatform option instead of calling this other funciton,
		// we end up with no .Process.Args in our vm for some reason
		s, err = oci.GenerateSpecWithPlatform(ctx, nil, "linux/arm64", c, opts...)
		if err != nil {
			return nil, nil, errors.WithStack(err)
		}
	} else {
		s, err = oci.GenerateSpec(ctx, nil, c, opts...)
		if err != nil {
			return nil, nil, errors.WithStack(err)
		}
	}

	if cgroupV2NamespaceSupported() {
		s.Linux.Namespaces = append(s.Linux.Namespaces, specs.LinuxNamespace{
			Type: specs.CgroupNamespace,
		})
	}

	if len(meta.Ulimit) == 0 {
		// reset open files limit
		s.Process.Rlimits = nil
	}

	// set the networking information on the spec
	if err := namespace.Set(s); err != nil {
		return nil, nil, errors.WithStack(err)
	}

	sm := &submounts{}

	var releasers []func() error
	releaseAll := func() {
		sm.cleanup()
		for _, f := range releasers {
			f()
		}
		if s.Process.SelinuxLabel != "" {
			selinux.ReleaseLabel(s.Process.SelinuxLabel)
		}
	}

	for _, m := range mounts {
		if m.Src == nil {
			return nil, nil, errors.Errorf("mount %s has no source", m.Dest)
		}
		mountable, err := m.Src.Mount(ctx, m.Readonly)
		if err != nil {
			releaseAll()
			return nil, nil, errors.Wrapf(err, "failed to mount %s", m.Dest)
		}
		mounts, release, err := mountable.Mount()
		if err != nil {
			releaseAll()
			return nil, nil, errors.WithStack(err)
		}
		releasers = append(releasers, release)
		for _, mount := range mounts {
			mount, release, err := compactLongOverlayMount(mount, m.Readonly)
			if err != nil {
				releaseAll()
				return nil, nil, err
			}

			if release != nil {
				releasers = append(releasers, release)
			}

			mount, err = sm.subMount(mount, m.Selector)
			if err != nil {
				releaseAll()
				var os *os.PathError
				if errors.As(err, &os) {
					if strings.HasSuffix(os.Path, m.Selector) {
						os.Path = m.Selector
					}
				}
				return nil, nil, err
			}
			s.Mounts = append(s.Mounts, specs.Mount{
				Destination: system.GetAbsolutePath(m.Dest),
				Type:        normalizeMountType(mount.Type),
				Source:      mount.Source,
				Options:     mount.Options,
			})
		}
	}

	if tracingSocket != "" {
		// moby/buildkit#4764
		if _, err := os.Stat(tracingSocket); err == nil {
			if mount := getTracingSocketMount(tracingSocket); mount != nil {
				s.Mounts = append(s.Mounts, *mount)
			}
		}
	}

	s.Mounts = dedupMounts(s.Mounts)

	if userns.RunningInUserNS() {
		s.Mounts, err = rootlessmountopts.FixUpOCI(s.Mounts)
		if err != nil {
			releaseAll()
			return nil, nil, err
		}
	}

	return s, releaseAll, nil
}

type mountRef struct {
	mount   mount.Mount
	unmount func() error
	subRefs map[string]mountRef
}

type submounts struct {
	m map[uint64]mountRef
}

func (s *submounts) subMount(m mount.Mount, subPath string) (mount.Mount, error) {
	// for Windows, always go through the sub-mounting process
	if path.Join("/", subPath) == "/" && runtime.GOOS != "windows" {
		return m, nil
	}
	if s.m == nil {
		s.m = map[uint64]mountRef{}
	}
	h, err := hashstructure.Hash(m, hashstructure.FormatV2, nil)
	if err != nil {
		return mount.Mount{}, errors.WithStack(err)
	}
	if mr, ok := s.m[h]; ok {
		if sm, ok := mr.subRefs[subPath]; ok {
			return sm.mount, nil
		}
		sm, unmount, err := sub(mr.mount, subPath)
		if err != nil {
			return mount.Mount{}, err
		}
		mr.subRefs[subPath] = mountRef{
			mount:   sm,
			unmount: unmount,
		}
		return sm, nil
	}

	lm := snapshot.LocalMounterWithMounts([]mount.Mount{m})

	mp, err := lm.Mount()
	if err != nil {
		return mount.Mount{}, err
	}

	s.m[h] = mountRef{
		mount:   bind(mp, m.ReadOnly()),
		unmount: lm.Unmount,
		subRefs: map[string]mountRef{},
	}

	sm, unmount, err := sub(s.m[h].mount, subPath)
	if err != nil {
		return mount.Mount{}, err
	}
	s.m[h].subRefs[subPath] = mountRef{
		mount:   sm,
		unmount: unmount,
	}
	return sm, nil
}

func (s *submounts) cleanup() {
	var wg sync.WaitGroup
	wg.Add(len(s.m))
	for _, m := range s.m {
		func(m mountRef) {
			go func() {
				for _, sm := range m.subRefs {
					sm.unmount()
				}
				m.unmount()
				wg.Done()
			}()
		}(m)
	}
	wg.Wait()
}

func bind(p string, ro bool) mount.Mount {
	m := mount.Mount{
		Source: p,
	}
	if runtime.GOOS != "windows" {
		// Windows uses a mechanism similar to bind mounts, but will err out if we request
		// a mount type it does not understand. Leaving the mount type empty on Windows will
		// yield the same result.
		m.Type = "bind"
		m.Options = []string{"rbind"}
	}
	if ro {
		m.Options = append(m.Options, "ro")
	}
	return m
}

func compactLongOverlayMount(m mount.Mount, ro bool) (mount.Mount, func() error, error) {
	if m.Type != "overlay" {
		return m, nil, nil
	}

	sz := 0
	for _, opt := range m.Options {
		sz += len(opt) + 1
	}

	// can fit to single page, no need to compact
	if sz < 4096-512 {
		return m, nil, nil
	}

	lm := snapshot.LocalMounterWithMounts([]mount.Mount{m})

	mp, err := lm.Mount()
	if err != nil {
		return mount.Mount{}, nil, err
	}

	return bind(mp, ro), lm.Unmount, nil
}
