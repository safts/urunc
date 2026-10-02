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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/urunc-dev/urunc/pkg/network"
)

// Delete runs after the monitor has exited, from outside the sandbox's
// network namespace. It has to leave that namespace the way the container
// found it, or a container restarted in the same pod is refused the network.
func TestReleaseSandboxNetAfterMonitorExit(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create network namespaces and tap devices")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("needs /dev/net/tun")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	defer orig.Close()

	name := fmt.Sprintf("urunc-test-%d", os.Getpid())
	ns, err := netns.NewNamed(name)
	require.NoError(t, err)
	defer func() { _ = netns.DeleteNamed(name) }()
	defer ns.Close()

	// What CNI gives the pod, and what the container's setup added to it. No
	// monitor attaches to the tap, as after one has exited.
	attrs := netlink.NewLinkAttrs()
	attrs.Name = "eth0"
	require.NoError(t, netlink.LinkAdd(&netlink.Dummy{LinkAttrs: attrs}))
	eth, err := netlink.LinkByName("eth0")
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(eth))
	addr, err := netlink.ParseAddr("10.88.0.2/24")
	require.NoError(t, err)
	require.NoError(t, netlink.AddrAdd(eth, addr))
	require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: eth.Attrs().Index, Gw: net.ParseIP("10.88.0.1")}))
	_, err = network.DynamicNetwork{}.NetworkSetup(0, 0)
	require.NoError(t, err)
	require.NoError(t, netns.Set(orig))

	u := &Unikontainer{Spec: &specs.Spec{Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
		{Type: specs.NetworkNamespace, Path: filepath.Join("/var/run/netns", name)},
	}}}}
	require.NoError(t, u.releaseSandboxNet())

	require.NoError(t, netns.Set(ns))
	defer func() { require.NoError(t, netns.Set(orig)) }()
	_, err = netlink.LinkByName("tap0_urunc")
	require.Error(t, err, "the tap of the exited monitor is still in the namespace")
	qdiscs, err := netlink.QdiscList(eth)
	require.NoError(t, err)
	for _, q := range qdiscs {
		require.NotEqual(t, uint32(netlink.HANDLE_INGRESS), q.Attrs().Parent, "eth0 still redirects to the removed tap")
	}
}

// A sandbox whose namespace is gone, with its pod, has nothing to release.
func TestReleaseSandboxNetWithoutNamespace(t *testing.T) {
	u := &Unikontainer{Spec: &specs.Spec{Linux: &specs.Linux{Namespaces: []specs.LinuxNamespace{
		{Type: specs.NetworkNamespace, Path: "/var/run/netns/urunc-test-does-not-exist"},
	}}}}
	require.NoError(t, u.releaseSandboxNet())
}
