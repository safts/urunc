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

	t.Run("9pfs has no pre-start command", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, sharedfsRootfs{sfsType: "9pfs"}.preStartCmd())
	})
}
