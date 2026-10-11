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

package unikernels

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

func TestLinuxContainerBootCmdLine(t *testing.T) {
	t.Parallel()

	l := &Linux{}
	argv := []string{"/bin/sh", "-c", "echo 'a  b'\nx", "  padded  ", ""}
	require.NoError(t, l.parseContainerBootCmdLine(argv))
	assert.Equal(t, containerBootInit, l.App, "the boot initrd's /init is the guest init")
	assert.Equal(t, argv, l.Argv, "the container command is kept verbatim for the urunit configuration")
	assert.Empty(t, l.Command, "the container command is not put on the kernel command line")

	err := l.parseContainerBootCmdLine(nil)
	assert.Error(t, err, "a container boot needs a command")
}

func TestLinuxContainerBootCommandString(t *testing.T) {
	t.Parallel()

	l := &Linux{
		App:           containerBootInit,
		Argv:          []string{"/bin/sh", "-c", "printf container-boot-ok"},
		Monitor:       "qemu",
		Env:           []string{"TOKEN=not-on-the-kernel-command-line"},
		RootFsType:    "virtiofs",
		InitrdConf:    true,
		ContainerBoot: true,
	}

	cmdline, err := l.CommandString()
	require.NoError(t, err)

	// The root parameters stay: /init mounts the share they describe itself.
	assert.Contains(t, cmdline, "root=fs0 rw rootfstype=virtiofs")
	assert.Contains(t, cmdline, "rdinit=/init")
	// The command travels in the urunit configuration, not on the kernel
	// command line, which would cap its length and number of arguments.
	assert.NotContains(t, cmdline, "printf")
	assert.NotContains(t, cmdline, "/bin/sh")
	assert.Contains(t, cmdline, "console=hvc0")
	// The environment travels in the urunit configuration inside the initrd,
	// which /init points urunit at, so neither it nor a config path is here.
	assert.NotContains(t, cmdline, "TOKEN=")
	assert.NotContains(t, cmdline, "URUNIT_CONFIG")
	assert.NotContains(t, cmdline, "retain_initrd")

	// The urunit configuration is appended to the boot initrd, not handed to
	// the monitor as an initrd of its own.
	assert.Empty(t, l.MonitorCli().ExtraInitrd)
	assert.Contains(t, l.MonitorCli().OtherArgs, "-nodefaults")

	l.RootFsType = "9pfs"
	cmdline, err = l.CommandString()
	require.NoError(t, err)
	assert.Contains(t, cmdline, "root=fs0 rw rootfstype=9p rootflags=trans=virtio")
}

func TestLinuxCommandStringVerbosity(t *testing.T) {
	t.Parallel()

	base := Linux{
		App:        containerBootInit,
		Command:    "/bin/sh",
		Monitor:    "qemu",
		RootFsType: "virtiofs",
	}

	quiet := base
	quiet.Verbose = false
	cmdline, err := quiet.CommandString()
	require.NoError(t, err)
	assert.Contains(t, cmdline, "quiet", "a non-debug log level boots the guest quietly")

	verbose := base
	verbose.Verbose = true
	cmdline, err = verbose.CommandString()
	require.NoError(t, err)
	assert.NotContains(t, cmdline, "quiet", "a debug log level leaves the guest verbose")
}

func TestLinuxContainerBootUrunitConfig(t *testing.T) {
	t.Parallel()

	// A container boot uses the very same urunit configuration as a urunit
	// image: environment, user and working directory, and no block volumes.
	l := &Linux{
		Env:           []string{"PATH=/usr/bin", "FOO=bar"},
		ProcConfig:    types.ProcessConfig{UID: 1000, GID: 1000, WorkDir: "/work"},
		ContainerBoot: true,
	}
	assert.Equal(t, "URUNIT1\x00UES\x00PATH=/usr/bin\x00FOO=bar\x00UEE\x00UCS\x00UID:1000\x00GID:1000\x00WD:/work\x00UCE\x00UBS\x00UBE\x00", l.buildUrunitConfig())
}

func TestLinuxUrunitConfigAdditionalGids(t *testing.T) {
	t.Parallel()

	// OCI process.user.additionalGids go to urunit verbatim and in order, one
	// AGID record each, right after GID.
	l := &Linux{
		ProcConfig: types.ProcessConfig{UID: 1000, GID: 1000, AdditionalGids: []uint32{1000, 2000}, WorkDir: "/work"},
	}
	assert.Equal(t, "URUNIT1\x00UES\x00UEE\x00UCS\x00UID:1000\x00GID:1000\x00AGID:1000\x00AGID:2000\x00WD:/work\x00UCE\x00UBS\x00UBE\x00", l.buildUrunitConfig())
}

func TestLinuxUrunitConfAsInitrd(t *testing.T) {
	t.Parallel()

	// A urunit image on a shared rootfs gets its config as the initrd.
	assert.True(t, (&Linux{InitrdConf: true, RootFsType: "virtiofs"}).urunitConfAsInitrd())
	// An initrd rootfs carries the config inside the initrd.
	assert.False(t, (&Linux{InitrdConf: true, RootFsType: "initrd"}).urunitConfAsInitrd())
	// A container boot appends the config to its boot initrd.
	assert.False(t, (&Linux{InitrdConf: true, RootFsType: "virtiofs", ContainerBoot: true}).urunitConfAsInitrd())
}

func TestLinuxContainerBootInitRequiresInitrd(t *testing.T) {
	t.Parallel()

	l := &Linux{}
	err := l.Init(types.UnikernelParams{
		CmdLine:       []string{"/bin/true"},
		Monitor:       "qemu",
		Rootfs:        types.RootfsParams{Type: "virtiofs"},
		ContainerBoot: true,
	})
	require.Error(t, err, "a container boot without the boot initrd path cannot build the guest initrd")
	assert.ErrorContains(t, err, "boot initrd")
}

func TestExitStatusFile(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "/run/urunc/exit-status", ExitStatusFile(true, "virtiofs"))
	assert.Equal(t, "/run/urunc/exit-status", ExitStatusFile(true, "9pfs"))
	assert.Equal(t, ExitStatusDisk, ExitStatusFile(true, "block"))
	for _, rootfs := range []string{"initrd", ""} {
		assert.Empty(t, ExitStatusFile(true, rootfs), rootfs)
	}
	assert.Empty(t, ExitStatusFile(false, "virtiofs"))
	assert.Empty(t, ExitStatusFile(false, "block"))
}

func TestLinuxCommandStringExitStatus(t *testing.T) {
	t.Parallel()

	rootfs := types.BlockDevParams{ID: "rootfs", Source: "/dev/dm-1", MountPoint: "/"}
	vol := func(id string) types.BlockDevParams {
		return types.BlockDevParams{ID: id, Source: "/dev/" + id, MountPoint: "/" + id}
	}
	for _, tc := range []struct {
		rootfs        string
		containerBoot bool
		blk           []types.BlockDevParams
		want          string
	}{
		{"virtiofs", true, nil, "/run/urunc/exit-status"},
		{"9pfs", true, nil, "/run/urunc/exit-status"},
		{"block", true, []types.BlockDevParams{rootfs}, "/dev/vdb"},
		{"block", true, []types.BlockDevParams{rootfs, vol("v1"), vol("v2")}, "/dev/vdd"},
		{"block", false, []types.BlockDevParams{rootfs}, ""},
		{"virtiofs", false, nil, ""},
	} {
		l := &Linux{
			App:           containerBootInit,
			Command:       "/bin/true",
			Monitor:       "qemu",
			RootFsType:    tc.rootfs,
			InitrdConf:    true,
			ContainerBoot: tc.containerBoot,
		}
		l.Blk = l.withExitStatusDisk(tc.blk)
		cmdline, err := l.CommandString()
		require.NoError(t, err)
		kernelPart, _, _ := strings.Cut(cmdline, " -- ")
		if tc.want != "" {
			assert.Contains(t, kernelPart, "URUNIT_EXIT_STATUS="+tc.want+" ", tc)
		} else {
			assert.NotContains(t, cmdline, "URUNIT_EXIT_STATUS", tc)
		}
	}
}

func TestLinuxBlockRootExitStatusDisk(t *testing.T) {
	t.Parallel()

	blk := make([]types.BlockDevParams, 1, 4)
	blk[0] = types.BlockDevParams{ID: "rootfs", Source: "/dev/dm-1", MountPoint: "/"}
	vol := types.BlockDevParams{ID: "vol1", Source: "/dev/dm-2", MountPoint: "/data"}
	blk = append(blk, vol)
	orig := append([]types.BlockDevParams(nil), blk...)

	l := &Linux{Monitor: "qemu", RootFsType: "block", ContainerBoot: true}
	l.Blk = l.withExitStatusDisk(blk)
	assert.Equal(t, orig, blk, "the caller's block list is not modified")
	assert.Empty(t, blk[:3][2].ID, "the caller's backing array is not written")
	assert.Len(t, l.Blk, 3)

	var qemu []string
	for _, a := range l.MonitorBlockCli() {
		qemu = append(qemu, a.ExactArgs...)
	}
	joined := strings.Join(qemu, " ")
	assert.Contains(t, joined, "serial=exitstatus")
	assert.Contains(t, joined, "file="+ExitStatusDisk)

	l.Monitor = "firecracker"
	fc := l.MonitorBlockCli()
	assert.Equal(t, "FCexitstatus", fc[len(fc)-1].ID)
	assert.Equal(t, ExitStatusDisk, fc[len(fc)-1].Path)

	conf := l.buildUrunitConfig()
	assert.NotContains(t, conf, "exitstatus", "urunit does not mount the status disk")
	assert.Contains(t, conf, "ID:FCvol1\x00")

	// Not a container boot, a shared-fs root, or no letter left: no disk.
	for _, other := range []*Linux{
		{RootFsType: "block"},
		{RootFsType: "virtiofs", ContainerBoot: true},
	} {
		assert.Equal(t, blk, other.withExitStatusDisk(blk))
	}
	full := make([]types.BlockDevParams, 26)
	assert.Len(t, l.withExitStatusDisk(full), 26)
}

func TestLinuxContainerBootUrunitConfigTmpfs(t *testing.T) {
	t.Parallel()

	l := &Linux{
		Env:           []string{"PATH=/usr/bin", "FOO=bar"},
		ProcConfig:    types.ProcessConfig{UID: 1000, GID: 1000, WorkDir: "/work"},
		ContainerBoot: true,
		Tmpfs:         []types.TmpfsParams{{Destination: "/scratch", Flags: 14, Data: "size=64m,mode=1770"}},
	}
	assert.Equal(t, "URUNIT1\x00UES\x00PATH=/usr/bin\x00FOO=bar\x00UEE\x00UCS\x00UID:1000\x00GID:1000\x00WD:/work\x00UCE\x00UBS\x00UBE\x00UTS\x00MP:/scratch\x00FL:14\x00DAT:size=64m,mode=1770\x00UTE\x00", l.buildUrunitConfig())

	// A urunit inside the user's image may predate the tmpfs section.
	l.ContainerBoot = false
	assert.NotContains(t, l.buildUrunitConfig(), "UTS")
}

func TestLinuxContainerBootUrunitConfigArgv(t *testing.T) {
	t.Parallel()

	l := &Linux{
		ProcConfig:    types.ProcessConfig{WorkDir: "/work"},
		ContainerBoot: true,
		Argv:          []string{"/bin/sh", "-c", "echo 'a  b'\nx", ""},
	}
	assert.Contains(t, l.buildUrunitConfig(), "WD:/work\x00ARC:4\x00ARV:/bin/sh\x00ARV:-c\x00ARV:echo 'a  b'\nx\x00ARV:\x00UCE\x00")

	// No length or count limit: a 16 KiB argument and 500 arguments.
	long := strings.Repeat("x", 16*1024)
	l.Argv = []string{"/bin/sh", "-c", long}
	assert.Contains(t, l.buildUrunitConfig(), "ARC:3\x00ARV:/bin/sh\x00ARV:-c\x00ARV:"+long+"\x00UCE\x00")

	l.Argv = make([]string, 500)
	for i := range l.Argv {
		l.Argv[i] = "arg" + strconv.Itoa(i)
	}
	conf := l.buildUrunitConfig()
	assert.Contains(t, conf, "ARC:500\x00ARV:arg0\x00")
	assert.Contains(t, conf, "\x00ARV:"+strings.Join(l.Argv, "\x00ARV:")+"\x00UCE\x00")

	// A urunit inside the user's image may predate the command in the
	// configuration, so only a container boot writes it.
	l.ContainerBoot = false
	assert.NotContains(t, l.buildUrunitConfig(), "ARC:")
}
