// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package unikontainers

import (
	"slices"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// monCaps are the capabilities the monitor keeps on top of the container's own
// set, in all five sets, up to its execve:
//   - CAP_NET_ADMIN: required for the creation and setup of the tap device
//     inside the container's network namespace.
var monCaps = []string{"CAP_NET_ADMIN"}

// monSetupCaps are the capabilities "urunc init" needs to finish the monitor's
// environment, whatever the container's own set. The container's capabilities
// describe its workload, which runs in the guest; the setup is urunc's own
// and runs as root on the host side:
//   - CAP_NET_ADMIN: the tap device (monCaps).
//   - CAP_SYS_CHROOT: virtiofsd's chroot sandbox (--sandbox chroot).
//   - CAP_CHOWN, CAP_DAC_OVERRIDE, CAP_FOWNER, CAP_FSETID, CAP_SETUID,
//     CAP_SETGID: virtiofsd's own set (it serves the guest's files as each
//     caller's uid/gid), and urunc's chown of the monitor's devices and socket
//     directory and its setgroups/setgid/setuid to the container user.
//   - CAP_SETPCAP: dropping the bounding set, by virtiofsd for itself and by
//     urunc before the monitor's execve (dropToExecCaps).
//
// None of them reaches the monitor: dropToExecCaps cuts the bounding set back
// to the container's set plus monCaps before the drop to the container user,
// so the monitor runs with exactly what it ran with before. virtiofsd's
// CAP_MKNOD and CAP_SETFCAP are not here: sharedfsRootfs takes them away from
// it unless the container has them (virtiofsdModcaps).
var monSetupCaps = []string{
	"CAP_NET_ADMIN",
	"CAP_SYS_CHROOT",
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FOWNER",
	"CAP_FSETID",
	"CAP_SETUID",
	"CAP_SETGID",
	"CAP_SETPCAP",
}

// monitorOCISpec builds the OCI spec that describes the monitor's
// execution environment. It consists of the container's spec,
// but altered accordingly for the respective monitor and rootfs.
func monitorOCISpec(spec *specs.Spec, monRes monitorResources, rootfsParams types.RootfsParams) *specs.Spec {
	monSpec := *spec
	monSpec.Root = &specs.Root{Path: rootfsParams.MonRootfs, Readonly: false}
	monSpec.Mounts = monRes.Mounts

	process := *spec.Process
	process.Cwd = "/"
	monSpec.Process = &process

	linux := *spec.Linux
	linux.Devices = monRes.Devices
	linux.Namespaces = spec.Linux.Namespaces
	monSpec.Linux = &linux

	return &monSpec
}

// MonitorCapabilities returns the capability set libcontainer starts "urunc
// init" with: the container's own set plus monSetupCaps in the bounding,
// effective and permitted sets, and plus monCaps in the inheritable and
// ambient ones. A container that drops capabilities (drop: [ALL]) would
// otherwise leave the setup unable to chroot virtiofsd, chown or change user,
// and no VM would start. dropToExecCaps takes the extra ones away again
// before the monitor's execve.
func MonitorCapabilities(spec *specs.Spec) *configs.Capabilities {
	caps := MonitorExecCapabilities(spec)
	if caps == nil {
		uniklog.Warn("The container declares no capabilities. The monitor inherits urunc's own set.")
		return nil
	}

	for _, capab := range monSetupCaps {
		caps.Bounding = withCap(caps.Bounding, capab)
		caps.Effective = withCap(caps.Effective, capab)
		caps.Permitted = withCap(caps.Permitted, capab)
	}

	return caps
}

// MonitorExecCapabilities returns the capability set the monitor runs with
// after the setup: the container's own set plus monCaps in all five sets, or
// nil when the spec declares none (the monitor then keeps urunc's own set).
func MonitorExecCapabilities(spec *specs.Spec) *configs.Capabilities {
	caps := specCapabilities(spec)
	if caps == nil {
		return nil
	}

	for _, capab := range monCaps {
		caps.Bounding = withCap(caps.Bounding, capab)
		caps.Effective = withCap(caps.Effective, capab)
		caps.Permitted = withCap(caps.Permitted, capab)
		caps.Inheritable = withCap(caps.Inheritable, capab)
		caps.Ambient = withCap(caps.Ambient, capab)
	}

	return caps
}

// specCapabilities copies the container's capability set out of the OCI spec,
// returning nil when the spec declares none.
func specCapabilities(spec *specs.Spec) *configs.Capabilities {
	if spec.Process == nil || spec.Process.Capabilities == nil {
		return nil
	}

	caps := spec.Process.Capabilities

	return &configs.Capabilities{
		Bounding:    slices.Clone(caps.Bounding),
		Effective:   slices.Clone(caps.Effective),
		Permitted:   slices.Clone(caps.Permitted),
		Inheritable: slices.Clone(caps.Inheritable),
		Ambient:     slices.Clone(caps.Ambient),
	}
}

// withCap returns caps with add appended, unless it is already there.
func withCap(caps []string, add string) []string {
	if slices.Contains(caps, add) {
		return caps
	}

	return append(caps, add)
}
