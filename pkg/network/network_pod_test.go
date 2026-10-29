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

package network

import (
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// testPod is what CNI gives a pod (an eth0 veth to the host, which holds the
// gateway), with a network namespace per guest standing in for each VM.
type testPod struct {
	host, pod netns.NsHandle
}

func newNs(t *testing.T) netns.NsHandle {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	defer orig.Close()
	ns, err := netns.New()
	require.NoError(t, err)
	require.NoError(t, netns.Set(orig))
	t.Cleanup(func() { ns.Close() })
	return ns
}

// inNs runs fn on a thread in ns.
func inNs(t *testing.T, ns netns.NsHandle, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		// The thread stays locked, so the runtime discards it with its netns.
		require.NoError(t, netns.Set(ns))
		fn()
	}()
	<-done
}

func newTestPod(t *testing.T) *testPod {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to create network namespaces and tap devices")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("needs /dev/net/tun")
	}
	p := &testPod{host: newNs(t), pod: newNs(t)}
	inNs(t, p.pod, func() {
		attrs := netlink.NewLinkAttrs()
		attrs.Name = "eth0"
		attrs.MTU = 1450
		require.NoError(t, netlink.LinkAdd(&netlink.Veth{LinkAttrs: attrs, PeerName: "host0"}))
		peer, err := netlink.LinkByName("host0")
		require.NoError(t, err)
		require.NoError(t, netlink.LinkSetNsFd(peer, int(p.host)))
		eth, err := netlink.LinkByName("eth0")
		require.NoError(t, err)
		require.NoError(t, netlink.LinkSetUp(eth))
		addr, err := netlink.ParseAddr("10.88.0.2/24")
		require.NoError(t, err)
		require.NoError(t, netlink.AddrAdd(eth, addr))
		require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: eth.Attrs().Index, Gw: net.ParseIP("10.88.0.1")}))
	})
	inNs(t, p.host, func() {
		host0, err := netlink.LinkByName("host0")
		require.NoError(t, err)
		addr, err := netlink.ParseAddr("10.88.0.1/24")
		require.NoError(t, err)
		require.NoError(t, netlink.AddrAdd(host0, addr))
		require.NoError(t, netlink.LinkSetUp(host0))
	})
	return p
}

func (p *testPod) setup(t *testing.T, n PodNetwork) (*UnikernelNetworkInfo, error) {
	t.Helper()
	var info *UnikernelNetworkInfo
	var err error
	inNs(t, p.pod, func() { info, err = n.NetworkSetup(0, 0) })
	return info, err
}

// openTap attaches a queue to the tap name, creating it if need be. The fd
// joins Go's poller only once attached, so its reads wake up and Close ends
// them.
func openTap(t *testing.T, name string, flags uint16) *os.File {
	t.Helper()
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	ifr, err := unix.NewIfreq(name)
	require.NoError(t, err)
	ifr.SetUint16(flags)
	require.NoError(t, unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr))
	return os.NewFile(uintptr(fd), name)
}

// boot stands a guest up on info's tap, as its VM would: a network namespace
// with the address, MAC and route the monitor hands the guest, whose frames
// are relayed to and from the tap. It returns the guest's namespace and a
// stop func that detaches it, as the monitor's exit does.
func (p *testPod) boot(t *testing.T, info *UnikernelNetworkInfo) (netns.NsHandle, func()) {
	t.Helper()
	guest := newNs(t)
	var podFd, guestFd *os.File
	inNs(t, p.pod, func() {
		podFd = openTap(t, info.TapDevice, unix.IFF_TAP|unix.IFF_NO_PI|unix.IFF_VNET_HDR|unix.IFF_ONE_QUEUE)
	})
	inNs(t, guest, func() {
		guestFd = openTap(t, "g0", unix.IFF_TAP|unix.IFF_NO_PI)
		g0, err := netlink.LinkByName("g0")
		require.NoError(t, err)
		mac, err := net.ParseMAC(info.EthDevice.MAC)
		require.NoError(t, err)
		require.NoError(t, netlink.LinkSetHardwareAddr(g0, mac))
		require.NoError(t, netlink.LinkSetMTU(g0, info.EthDevice.MTU))
		ones, _ := net.IPMask(net.ParseIP(info.EthDevice.Mask).To4()).Size()
		addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", info.EthDevice.IP, ones))
		require.NoError(t, err)
		require.NoError(t, netlink.AddrAdd(g0, addr))
		require.NoError(t, netlink.LinkSetUp(g0))
		require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: g0.Attrs().Index, Gw: net.ParseIP(info.EthDevice.DefaultGateway)}))
	})
	// The pod side carries a virtio-net header (10 bytes, no offloads).
	const vnetHdr = 10
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := podFd.Read(buf)
			if err != nil {
				return
			}
			if n > vnetHdr {
				_, _ = guestFd.Write(buf[vnetHdr:n])
			}
		}
	}()
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := guestFd.Read(buf[vnetHdr:])
			if err != nil {
				return
			}
			clear(buf[:vnetHdr])
			_, _ = podFd.Write(buf[:vnetHdr+n])
		}
	}()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = podFd.Close()
			_ = guestFd.Close()
		}
	}
	t.Cleanup(stop)
	return guest, stop
}

// serve answers every connection to port in ns with tag.
func serve(t *testing.T, ns netns.NsHandle, port int, tag string) {
	t.Helper()
	var l net.Listener
	inNs(t, ns, func() {
		var err error
		l, err = net.Listen("tcp4", fmt.Sprintf(":%d", port))
		require.NoError(t, err)
	})
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(tag))
			_ = c.Close()
		}
	}()
}

// dial connects from ns to addr and returns what the server sent.
func dial(t *testing.T, ns netns.NsHandle, addr string) (string, error) {
	t.Helper()
	var got string
	var err error
	inNs(t, ns, func() {
		var c net.Conn
		c, err = net.DialTimeout("tcp4", addr, 2*time.Second)
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		var b []byte
		b, err = io.ReadAll(c)
		got = string(b)
	})
	return got, err
}

func requireReply(t *testing.T, ns netns.NsHandle, addr, want string) {
	t.Helper()
	got, err := dial(t, ns, addr)
	require.NoError(t, err, "dial %s", addr)
	require.Equal(t, want, got, "dial %s", addr)
}

// A primary VM (the pod's main container) and a service VM run at once. The
// host reaches the service on its ports and the primary on every other one,
// the primary reaches the service at the service's address, and only the
// primary reaches out of the pod. Either may start first.
func TestPodNetworkPrimaryAndService(t *testing.T) {
	for _, order := range []string{"service first", "primary first"} {
		t.Run(order, func(t *testing.T) {
			p := newTestPod(t)
			primary, service := PodNetwork{}, PodNetwork{Service: 1, Ports: []uint16{5432}}
			first, second := service, primary
			if order == "primary first" {
				first, second = primary, service
			}
			infos := map[int]*UnikernelNetworkInfo{}
			for _, n := range []PodNetwork{first, second} {
				info, err := p.setup(t, n)
				require.NoError(t, err)
				infos[n.Service] = info
			}
			require.Equal(t, "tap0_urunc", infos[0].TapDevice)
			require.Equal(t, "tap1_urunc", infos[1].TapDevice)
			for _, info := range infos {
				require.Equal(t, "10.88.0.2", info.EthDevice.IP, "every guest has the pod's address")
			}

			pg, _ := p.boot(t, infos[0])
			sg, _ := p.boot(t, infos[1])
			serve(t, pg, 8080, "primary")
			serve(t, sg, 5432, "service")
			serve(t, p.host, 9000, "host")

			requireReply(t, p.host, "10.88.0.2:5432", "service")
			requireReply(t, p.host, "10.88.0.2:8080", "primary")
			requireReply(t, pg, ServiceAddress(1).String()+":5432", "service")
			requireReply(t, pg, "10.88.0.1:9000", "host")
			_, err := dial(t, sg, "10.88.0.1:9000")
			require.Error(t, err, "a service must not reach out of the pod")
		})
	}
}

// The primary's container restarts while its service runs: the new primary
// VM gets the pod's network and reaches the service again.
func TestPodNetworkPrimaryRestart(t *testing.T) {
	p := newTestPod(t)
	service, err := p.setup(t, PodNetwork{Service: 1, Ports: []uint16{6379}})
	require.NoError(t, err)
	sg, _ := p.boot(t, service)
	serve(t, sg, 6379, "service")

	primary, err := p.setup(t, PodNetwork{})
	require.NoError(t, err)
	_, stop := p.boot(t, primary)
	stop()
	inNs(t, p.pod, func() { require.NoError(t, PodNetwork{}.Release()) })

	primary, err = p.setup(t, PodNetwork{})
	require.NoError(t, err)
	pg, _ := p.boot(t, primary)
	requireReply(t, pg, ServiceAddress(1).String()+":6379", "service")
	requireReply(t, p.host, "10.88.0.2:6379", "service")
}

// A service's container restarts while the primary runs.
func TestPodNetworkServiceRestart(t *testing.T) {
	p := newTestPod(t)
	primary, err := p.setup(t, PodNetwork{})
	require.NoError(t, err)
	pg, _ := p.boot(t, primary)

	svc := PodNetwork{Service: 2, Ports: []uint16{80, 443}}
	info, err := p.setup(t, svc)
	require.NoError(t, err)
	_, stop := p.boot(t, info)
	stop() // its monitor exited on its own: the tap is left, unused

	info, err = p.setup(t, svc)
	require.NoError(t, err)
	sg, _ := p.boot(t, info)
	serve(t, sg, 443, "service")
	requireReply(t, pg, ServiceAddress(2).String()+":443", "service")
	requireReply(t, p.host, "10.88.0.2:443", "service")
}

// A VM still holds the tap: its container runs, so a second one with the
// same part in the pod is refused.
func TestPodNetworkRefusesARunningTap(t *testing.T) {
	p := newTestPod(t)
	for _, n := range []PodNetwork{{}, {Service: 1, Ports: []uint16{5432}}} {
		info, err := p.setup(t, n)
		require.NoError(t, err)
		p.boot(t, info)
		_, err = p.setup(t, n)
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "in use"), err.Error())
	}
}

// Removing one container's tap leaves the others' alone.
func TestPodNetworkReleaseKeepsOtherTaps(t *testing.T) {
	p := newTestPod(t)
	_, err := p.setup(t, PodNetwork{})
	require.NoError(t, err)
	_, err = p.setup(t, PodNetwork{Service: 1})
	require.NoError(t, err)
	inNs(t, p.pod, func() {
		require.NoError(t, PodNetwork{Service: 1}.Release())
		_, err := netlink.LinkByName("tap1_urunc")
		require.Error(t, err)
		_, err = netlink.LinkByName("tap0_urunc")
		require.NoError(t, err)
	})
}

func TestPodNetworkRejectsBadParts(t *testing.T) {
	p := newTestPod(t)
	for _, n := range []PodNetwork{
		{Service: -1},
		{Service: MaxPodServices + 1},
		{Ports: []uint16{80}},
		{Service: 1, Ports: make([]uint16, maxServicePorts+1)},
		{Service: 1, Ports: []uint16{0}},
	} {
		_, err := p.setup(t, n)
		require.Error(t, err, "%+v", n)
	}
}
