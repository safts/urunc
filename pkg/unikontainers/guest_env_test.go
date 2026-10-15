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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPasswd = `root:x:0:0:root:/root:/bin/sh
app:x:61100:1001::/home/app:/bin/bash
nohome:x:2000:2000:::/bin/sh
`

// writePasswd creates a container rootfs whose /etc/passwd holds content.
func writePasswd(t *testing.T, content string) string {
	t.Helper()
	rootfs := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootfs, "etc", "passwd"), []byte(content), 0o644))
	return rootfs
}

func TestGuestEnvWithHome(t *testing.T) {
	t.Parallel()
	rootfs := writePasswd(t, testPasswd)

	tests := []struct {
		name string
		env  []string
		uid  uint32
		want []string
	}{
		{name: "home from passwd", env: []string{"PATH=/bin"}, uid: 61100, want: []string{"PATH=/bin", "HOME=/home/app"}},
		{name: "root", env: nil, uid: 0, want: []string{"HOME=/root"}},
		{name: "spec value wins", env: []string{"HOME=/custom"}, uid: 61100, want: []string{"HOME=/custom"}},
		{name: "empty value is replaced", env: []string{"HOME=", "A=b"}, uid: 61100, want: []string{"A=b", "HOME=/home/app"}},
		{name: "uid not in passwd", env: []string{"A=b"}, uid: 4242, want: []string{"A=b", "HOME=/"}},
		{name: "user without a home", env: nil, uid: 2000, want: []string{"HOME=/"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, guestEnvWithHome(tt.env, rootfs, tt.uid))
		})
	}
}

func TestGuestEnvWithHomeNoPasswd(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"HOME=/"}, guestEnvWithHome(nil, t.TempDir(), 61100))
}

// A passwd symlink pointing out of the rootfs must resolve inside it, never on
// the host.
func TestGuestEnvWithHomeSymlinkStaysInRootfs(t *testing.T) {
	t.Parallel()
	outside := writePasswd(t, "app:x:61100:1001::/host/home:/bin/sh\n")
	rootfs := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(outside, "etc", "passwd"), filepath.Join(rootfs, "etc", "passwd")))

	assert.Equal(t, []string{"HOME=/"}, guestEnvWithHome(nil, rootfs, 61100))
}

// The spec's environment is not changed in place: the guest gets the copy.
func TestGuestEnvWithHomeLeavesInputAlone(t *testing.T) {
	t.Parallel()
	rootfs := writePasswd(t, testPasswd)
	env := make([]string, 1, 4)
	env[0] = "HOME="

	got := guestEnvWithHome(env, rootfs, 61100)
	assert.Equal(t, []string{"HOME=/home/app"}, got)
	assert.Equal(t, []string{"HOME="}, env)
}

// The guest's main process gets HOME the way runc gives it to a container:
// from the image's /etc/passwd when the spec does not set it.
func TestBuildMonitorSpecSetsGuestHome(t *testing.T) {
	t.Parallel()
	monRootfs := t.TempDir()
	u, rootfsParams := newSpecUnikontainer(t, monRootfs)
	bundle := t.TempDir()
	u.State.Bundle = bundle
	rootfs := filepath.Join(bundle, "rootfs")
	require.NoError(t, os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootfs, "etc", "passwd"), []byte(testPasswd), 0o644))
	u.Spec.Process.Env = []string{"PATH=/bin"}
	u.Spec.Process.User.UID = 61100

	ms := u.buildMonitorSpec(rootfsParams, monitorResources{Rootfs: rootfsParams})

	assert.Equal(t, []string{"PATH=/bin", "HOME=/home/app"}, ms.GuestParams.EnvVars)
	assert.Equal(t, []string{"PATH=/bin"}, u.Spec.Process.Env)
}

// The guest's hostname is the container's, as runc sets it.
func TestBuildMonitorSpecSetsGuestHostname(t *testing.T) {
	t.Parallel()
	u, rootfsParams := newSpecUnikontainer(t, t.TempDir())
	u.State.Bundle = t.TempDir()
	u.Spec.Hostname = "issue-repo-42-0"

	ms := u.buildMonitorSpec(rootfsParams, monitorResources{Rootfs: rootfsParams})

	assert.Equal(t, "issue-repo-42-0", ms.GuestParams.Hostname)
}
