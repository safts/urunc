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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// containerd's CRI with the systemd cgroup driver hands every container a
// "slice:prefix:name" cgroupsPath, but a shim of a runtime_type other than
// runc's cannot be told SystemdCgroup on containerd 2.x, so urunc never gets
// --systemd-cgroup. It recognises the path's form instead.
func TestIsSystemdCgroupsPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{"kubepods-besteffort-pod1234.slice:cri-containerd:abcd", true},
		{"system.slice:urunc:test-container", true},
		{":cri-containerd:abcd", true}, // empty slice: runc's default, system.slice
		{"/kubepods/besteffort/pod1234/abcd", false},
		{"/pods/test-container", false},
		{"", false},
		{"kubepods.slice:cri-containerd", false},              // two parts
		{"a.slice:b:c:d", false},                              // four parts
		{"notaslice:cri-containerd:abcd", false},              // no .slice
		{"/kubepods.slice/pod.slice:cri-containerd:x", false}, // a path, not a unit
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isSystemdCgroupsPath(tt.path))
		})
	}
}

func TestUseSystemdCgroup(t *testing.T) {
	t.Parallel()
	assert.True(t, useSystemdCgroup(true, "/pods/x"), "the flag always wins")
	assert.True(t, useSystemdCgroup(false, "kubepods.slice:cri-containerd:x"), "detected from the path")
	assert.False(t, useSystemdCgroup(false, "/pods/x"))
}

func TestRuncArgs(t *testing.T) {
	t.Parallel()
	in := []string{"urunc", "--root", "/run/urunc", "create", "--bundle", "/b", "id"}

	got := runcArgs(in, "/usr/bin/runc", true)
	assert.Equal(t, []string{"/usr/bin/runc", "--systemd-cgroup", "--root", "/run/urunc", "create", "--bundle", "/b", "id"}, got)
	assert.Equal(t, "urunc", in[0], "the caller's args are not modified")

	got = runcArgs(in, "/usr/bin/runc", false)
	assert.Equal(t, append([]string{"/usr/bin/runc"}, in[1:]...), got)

	already := []string{"urunc", "--systemd-cgroup", "create", "id"}
	got = runcArgs(already, "/usr/bin/runc", true)
	assert.Equal(t, []string{"/usr/bin/runc", "--systemd-cgroup", "create", "id"}, got, "not added twice")
}

func TestBundleCgroupsPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"ociVersion":"1.2.0","linux":{"cgroupsPath":"kubepods-pod1.slice:cri-containerd:abc"}}`), 0o600))
	assert.Equal(t, "kubepods-pod1.slice:cri-containerd:abc", bundleCgroupsPath(dir))

	assert.Empty(t, bundleCgroupsPath(t.TempDir()), "no config.json")

	noLinux := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(noLinux, "config.json"), []byte(`{"ociVersion":"1.2.0"}`), 0o600))
	assert.Empty(t, bundleCgroupsPath(noLinux))

	bad := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bad, "config.json"), []byte(`{`), 0o600))
	assert.Empty(t, bundleCgroupsPath(bad))
}
