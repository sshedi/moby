package oci

import (
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/continuity/fs"
	"github.com/moby/buildkit/solver/llbsolver/cdidevices"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/sys/user"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

func withProcessArgs(args ...string) oci.SpecOpts {
	return oci.WithProcessArgs(args...)
}

func generateMountOpts(_, _ string) []oci.SpecOpts {
	return nil
}

// generateSecurityOpts may affect mounts, so must be called after generateMountOpts
func generateSecurityOpts(mode pb.SecurityMode, _ string, _ bool) ([]oci.SpecOpts, error) {
	if mode == pb.SecurityMode_INSECURE {
		return nil, errors.New("no support for running in insecure mode on FreeBSD")
	}
	return nil, nil
}

// generateProcessModeOpts may affect mounts, so must be called after generateMountOpts
func generateProcessModeOpts(mode ProcessMode) ([]oci.SpecOpts, error) {
	if mode == NoProcessSandbox {
		return nil, errors.New("no support for NoProcessSandbox on FreeBSD")
	}
	return nil, nil
}

func generateIDmapOpts(idmap *user.IdentityMapping) ([]oci.SpecOpts, error) {
	if idmap == nil {
		return nil, nil
	}
	return nil, errors.New("no support for IdentityMapping on FreeBSD")
}

func generateRlimitOpts(ulimits []*pb.Ulimit) ([]oci.SpecOpts, error) {
	if len(ulimits) == 0 {
		return nil, nil
	}
	return nil, errors.New("no support for POSIXRlimit on FreeBSD")
}

// tracing is not implemented on FreeBSD
func getTracingSocketMount(_ string) *specs.Mount {
	return nil
}

// tracing is not implemented on FreeBSD
func getTracingSocket() string {
	return ""
}

func cgroupV2NamespaceSupported() bool {
	return false
}

func sub(m mount.Mount, subPath string) (mount.Mount, func() error, error) {
	src, err := fs.RootPath(m.Source, subPath)
	if err != nil {
		return mount.Mount{}, nil, err
	}
	m.Source = src
	return m, func() error { return nil }, nil
}

func generateCDIOpts(_ *cdidevices.Manager, devices []*pb.CDIDevice) ([]oci.SpecOpts, error) {
	if len(devices) == 0 {
		return nil, nil
	}
	return nil, errors.New("no support for CDI on FreeBSD")
}
