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

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const systemdCgroupFlag = "--systemd-cgroup"

// isSystemdCgroupsPath reports whether cgroupsPath has the systemd driver's
// "slice:prefix:name" form (the slice may be empty) rather than being a
// cgroupfs path.
//
// containerd's CRI picks that form when the node uses the systemd cgroup
// driver, and its runc shim then passes --systemd-cgroup to the runtime. The
// urunc shim never does: on containerd 2.x a runtime_type other than runc's
// gets the generic runtime options, which carry no SystemdCgroup. Without the
// flag urunc (and the runc it hands the pause container to) would take the
// path as a cgroupfs directory relative to its own cgroup, and the VM would
// land outside the pod's slice, invisible to the kubelet.
func isSystemdCgroupsPath(cgroupsPath string) bool {
	if strings.Contains(cgroupsPath, "/") {
		return false
	}
	parts := strings.Split(cgroupsPath, ":")
	if len(parts) != 3 {
		return false
	}
	return parts[0] == "" || strings.HasSuffix(parts[0], ".slice")
}

// useSystemdCgroup is the cgroup driver for a container: systemd when the
// --systemd-cgroup flag is set or the spec's cgroupsPath has the systemd form.
func useSystemdCgroup(flag bool, cgroupsPath string) bool {
	return flag || isSystemdCgroupsPath(cgroupsPath)
}

// specCgroupsPath returns spec.linux.cgroupsPath, or "" without one.
func specCgroupsPath(spec *specs.Spec) string {
	if spec == nil || spec.Linux == nil {
		return ""
	}
	return spec.Linux.CgroupsPath
}

// bundleCgroupsPath returns linux.cgroupsPath of the bundle's config.json, or
// "" when the file is missing, unreadable or has none. It is only used to pick
// the cgroup driver for a container urunc hands over to runc, which reads and
// validates the bundle itself.
func bundleCgroupsPath(bundlePath string) string {
	data, err := os.ReadFile(filepath.Join(bundlePath, "config.json"))
	if err != nil {
		return ""
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return ""
	}
	return specCgroupsPath(&spec)
}

// runcArgs is urunc's own argv retargeted at runc: binPath in place of
// argv[0] and, for the systemd driver, --systemd-cgroup as the first global
// flag unless it is already there. args itself is not modified.
func runcArgs(args []string, binPath string, systemdCgroup bool) []string {
	out := make([]string, 0, len(args)+1)
	out = append(out, binPath)
	if systemdCgroup && !slices.Contains(args[1:], systemdCgroupFlag) {
		out = append(out, systemdCgroupFlag)
	}
	return append(out, args[1:]...)
}
