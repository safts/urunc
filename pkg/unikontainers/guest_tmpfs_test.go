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
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

func TestGuestTmpfsMounts(t *testing.T) {
	t.Parallel()

	tmpfs := func(dst string, opts ...string) specs.Mount {
		return specs.Mount{Destination: dst, Type: "tmpfs", Source: "tmpfs", Options: opts}
	}
	// containerd's default mounts (oci/mounts.go)
	containerdDefaults := []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		tmpfs("/dev", "nosuid", "strictatime", "mode=755", "size=65536k"),
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		tmpfs("/run", "nosuid", "strictatime", "mode=755", "size=65536k"),
	}

	tests := []struct {
		name   string
		mounts []specs.Mount
		want   []types.TmpfsParams
	}{
		{
			name:   "flags and data",
			mounts: []specs.Mount{tmpfs("/scratch", "noexec", "nosuid", "nodev", "size=64m", "mode=1770")},
			want:   []types.TmpfsParams{{Destination: "/scratch", Flags: unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NODEV, Data: "size=64m,mode=1770"}},
		},
		{
			name:   "run",
			mounts: []specs.Mount{tmpfs("/run", "nosuid", "strictatime", "mode=755", "size=65536k")},
			want:   []types.TmpfsParams{{Destination: "/run", Flags: unix.MS_NOSUID | unix.MS_STRICTATIME, Data: "mode=755,size=65536k"}},
		},
		{
			name:   "dev is dropped",
			mounts: []specs.Mount{tmpfs("/dev"), tmpfs("/dev/")},
		},
		{
			name: "non-tmpfs is dropped",
			mounts: []specs.Mount{
				{Destination: "/data", Type: "bind", Source: "/src", Options: []string{"rbind"}},
				{Destination: "/proc", Type: "proc", Source: "proc"},
			},
		},
		{
			name:   "cleared flags and propagation",
			mounts: []specs.Mount{tmpfs("/t", "ro", "rw", "rprivate", "size=1m")},
			want:   []types.TmpfsParams{{Destination: "/t", Flags: 0, Data: "size=1m"}},
		},
		{
			name:   "containerd defaults",
			mounts: containerdDefaults,
			want: []types.TmpfsParams{
				{Destination: "/dev/shm", Flags: unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV, Data: "mode=1777,size=65536k"},
				{Destination: "/run", Flags: unix.MS_NOSUID | unix.MS_STRICTATIME, Data: "mode=755,size=65536k"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, guestTmpfsMounts(tc.mounts))
		})
	}
}
