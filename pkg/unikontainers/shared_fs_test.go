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

	"github.com/stretchr/testify/assert"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

func TestVirtiofsdPreStartCmd(t *testing.T) {
	t.Parallel()

	count := func(argv []string, s string) int {
		n := 0
		for _, a := range argv {
			if a == s {
				n++
			}
		}
		return n
	}

	t.Run("default options get --inode-file-handles=never once", func(t *testing.T) {
		t.Parallel()
		s := sharedfsRootfs{sfsType: "virtiofs", vfsdConfig: types.ExtraBinConfig{
			Path: "/usr/libexec/virtiofsd", Options: "--cache always --sandbox chroot"}}
		assert.Equal(t, 1, count(s.preStartCmd(), "--inode-file-handles=never"))
	})

	t.Run("an explicit --inode-file-handles is kept", func(t *testing.T) {
		t.Parallel()
		s := sharedfsRootfs{sfsType: "virtiofs", vfsdConfig: types.ExtraBinConfig{
			Path: "/usr/libexec/virtiofsd", Options: "--inode-file-handles=prefer"}}
		argv := s.preStartCmd()
		assert.Equal(t, 1, count(argv, "--inode-file-handles=prefer"))
		assert.Zero(t, count(argv, "--inode-file-handles=never"))
	})

	t.Run("default options get --log-level=error once", func(t *testing.T) {
		t.Parallel()
		s := sharedfsRootfs{sfsType: "virtiofs", vfsdConfig: types.ExtraBinConfig{
			Path: "/usr/libexec/virtiofsd", Options: "--cache always --sandbox chroot"}}
		assert.Equal(t, 1, count(s.preStartCmd(), "--log-level=error"))
	})

	t.Run("an explicit --log-level is kept", func(t *testing.T) {
		t.Parallel()
		s := sharedfsRootfs{sfsType: "virtiofs", vfsdConfig: types.ExtraBinConfig{
			Path: "/usr/libexec/virtiofsd", Options: "--log-level debug"}}
		argv := s.preStartCmd()
		assert.Equal(t, 1, count(argv, "--log-level"))
		assert.Zero(t, count(argv, "--log-level=error"))
	})

	t.Run("a container without CAP_MKNOD and CAP_SETFCAP takes them from virtiofsd", func(t *testing.T) {
		t.Parallel()
		s := sharedfsRootfs{sfsType: "virtiofs", containerCaps: []string{}, vfsdConfig: types.ExtraBinConfig{
			Path: "/usr/libexec/virtiofsd", Options: "--cache always --sandbox chroot"}}
		assert.Equal(t, 1, count(s.preStartCmd(), "--modcaps=-mknod:-setfcap"))
	})

	t.Run("a container with CAP_MKNOD keeps it for virtiofsd", func(t *testing.T) {
		t.Parallel()
		s := sharedfsRootfs{sfsType: "virtiofs", containerCaps: []string{"CAP_MKNOD"}, vfsdConfig: types.ExtraBinConfig{
			Path: "/usr/libexec/virtiofsd", Options: "--cache always --sandbox chroot"}}
		assert.Equal(t, 1, count(s.preStartCmd(), "--modcaps=-setfcap"))
	})

	t.Run("a container with both, or without a capability list, adds no --modcaps", func(t *testing.T) {
		t.Parallel()
		for _, caps := range [][]string{{"CAP_MKNOD", "CAP_SETFCAP"}, nil} {
			s := sharedfsRootfs{sfsType: "virtiofs", containerCaps: caps, vfsdConfig: types.ExtraBinConfig{
				Path: "/usr/libexec/virtiofsd", Options: "--cache always --sandbox chroot"}}
			for _, a := range s.preStartCmd() {
				assert.NotContains(t, a, "--modcaps", "caps %v", caps)
			}
		}
	})

	t.Run("an explicit --modcaps is kept", func(t *testing.T) {
		t.Parallel()
		s := sharedfsRootfs{sfsType: "virtiofs", containerCaps: []string{}, vfsdConfig: types.ExtraBinConfig{
			Path: "/usr/libexec/virtiofsd", Options: "--modcaps=-mknod"}}
		argv := s.preStartCmd()
		assert.Equal(t, 1, count(argv, "--modcaps=-mknod"))
		assert.Zero(t, count(argv, "--modcaps=-mknod:-setfcap"))
	})

	t.Run("9pfs has no pre-start command", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, sharedfsRootfs{sfsType: "9pfs"}.preStartCmd())
	})
}
