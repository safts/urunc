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
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
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

// podOpts shape the pod interface: its MTU (default 1450) and, as Cilium
// gives a pod, a /32 address with a link-scope route to the gateway.
type podOpts struct {
	MTU     int
	Slash32 bool
}

func newTestPod(t *testing.T, opts ...podOpts) *testPod {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to create network namespaces and tap devices")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("needs /dev/net/tun")
	}
	o := podOpts{MTU: 1450}
	if len(opts) > 0 {
		o = opts[0]
	}
	plen := "/24"
	if o.Slash32 {
		plen = "/32"
	}
	p := &testPod{host: newNs(t), pod: newNs(t)}
	inNs(t, p.pod, func() {
		attrs := netlink.NewLinkAttrs()
		attrs.Name = "eth0"
		attrs.MTU = o.MTU
		require.NoError(t, netlink.LinkAdd(&netlink.Veth{LinkAttrs: attrs, PeerName: "host0"}))
		peer, err := netlink.LinkByName("host0")
		require.NoError(t, err)
		require.NoError(t, netlink.LinkSetNsFd(peer, int(p.host)))
		eth, err := netlink.LinkByName("eth0")
		require.NoError(t, err)
		require.NoError(t, netlink.LinkSetUp(eth))
		addr, err := netlink.ParseAddr("10.88.0.2" + plen)
		require.NoError(t, err)
		require.NoError(t, netlink.AddrAdd(eth, addr))
		if o.Slash32 {
			require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: eth.Attrs().Index, Scope: netlink.SCOPE_LINK,
				Dst: &net.IPNet{IP: net.ParseIP("10.88.0.1"), Mask: net.CIDRMask(32, 32)}}))
		}
		require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: eth.Attrs().Index, Gw: net.ParseIP("10.88.0.1")}))
	})
	inNs(t, p.host, func() {
		host0, err := netlink.LinkByName("host0")
		require.NoError(t, err)
		require.NoError(t, netlink.LinkSetMTU(host0, o.MTU))
		addr, err := netlink.ParseAddr("10.88.0.1" + plen)
		require.NoError(t, err)
		require.NoError(t, netlink.AddrAdd(host0, addr))
		require.NoError(t, netlink.LinkSetUp(host0))
		if o.Slash32 {
			require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: host0.Attrs().Index, Scope: netlink.SCOPE_LINK,
				Dst: &net.IPNet{IP: net.ParseIP("10.88.0.2"), Mask: net.CIDRMask(32, 32)}}))
		}
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

// sysctl writes a sysctl of ns (/proc/sys/net is the opener's namespace).
func sysctl(t *testing.T, ns netns.NsHandle, path, value string) {
	t.Helper()
	inNs(t, ns, func() {
		require.NoError(t, os.WriteFile("/proc/sys/"+path, []byte(value), 0o600))
	})
}

// serveFunc runs h on every TCP connection to port in ns.
func serveFunc(t *testing.T, ns netns.NsHandle, port int, h func(net.Conn)) {
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
			go func() {
				defer c.Close()
				h(c)
			}()
		}
	}()
}

// servePeer answers every connection with the address it came from.
func servePeer(t *testing.T, ns netns.NsHandle, port int) {
	serveFunc(t, ns, port, func(c net.Conn) {
		_, _ = c.Write([]byte(c.RemoteAddr().(*net.TCPAddr).IP.String()))
	})
}

// serveUDP answers every datagram to port in ns with tag.
func serveUDP(t *testing.T, ns netns.NsHandle, port int, tag string) {
	t.Helper()
	var pc net.PacketConn
	inNs(t, ns, func() {
		var err error
		pc, err = net.ListenPacket("udp4", fmt.Sprintf(":%d", port))
		require.NoError(t, err)
	})
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			_, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo([]byte(tag), from)
		}
	}()
}

func dialUDP(t *testing.T, ns netns.NsHandle, addr string) (string, error) {
	t.Helper()
	var got string
	var err error
	inNs(t, ns, func() {
		var c net.Conn
		if c, err = net.Dial("udp4", addr); err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err = c.Write([]byte("?")); err != nil {
			return
		}
		b := make([]byte, 1500)
		var n int
		n, err = c.Read(b)
		got = string(b[:n])
	})
	return got, err
}

// dialFrom connects from ns, from local port lport, to addr.
func dialFrom(t *testing.T, ns netns.NsHandle, lport int, addr string) (string, error) {
	t.Helper()
	var got string
	var err error
	inNs(t, ns, func() {
		d := net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{Port: lport}}
		var c net.Conn
		if c, err = d.Dial("tcp4", addr); err != nil {
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

// pod is a booted pod network: a primary and services 1..n, each serving
// its tag on 8080 (the primary) or on its ports, UDP too.
type pod struct {
	*testPod
	guests map[int]netns.NsHandle
	stops  map[int]func()
}

var testServices = map[int][]uint16{1: {5432}, 2: {6379, 6380}}

func part(i int) PodNetwork {
	return PodNetwork{Service: i, Ports: testServices[i]}
}

func bootPod(t *testing.T, tp *testPod, order ...int) *pod {
	t.Helper()
	p := &pod{testPod: tp, guests: map[int]netns.NsHandle{}, stops: map[int]func(){}}
	infos := map[int]*UnikernelNetworkInfo{}
	for _, i := range order {
		info, err := tp.setup(t, part(i))
		require.NoError(t, err, "setup %d", i)
		infos[i] = info
	}
	for _, i := range order {
		p.start(t, i, infos[i])
	}
	return p
}

func (p *pod) start(t *testing.T, i int, info *UnikernelNetworkInfo) {
	t.Helper()
	g, stop := p.boot(t, info)
	p.guests[i], p.stops[i] = g, stop
	tag := fmt.Sprintf("guest%d", i)
	if i == 0 {
		serve(t, g, 8080, tag)
		serveUDP(t, g, 8080, tag)
	}
	for _, port := range testServices[i] {
		serve(t, g, int(port), tag)
		serveUDP(t, g, int(port), tag)
	}
	servePeer(t, g, 7)
}

func addr(i int, port int) string {
	ip := PrimaryAddress
	if i > 0 {
		ip = ServiceAddress(i)
	}
	return fmt.Sprintf("%s:%d", ip, port)
}

// The checks every pod passes, whatever the order its containers started
// in: probes, the primary and each service reach each other on any port
// and over UDP, the primary's egress shows the pod's address, and a
// service has none.
func checkPod(t *testing.T, p *pod) {
	t.Helper()
	serve(t, p.host, 9000, "host")
	servePeer(t, p.host, 9001)
	// A host that routes the pod network to the pod still reaches no VM
	// directly, and no service reaches it: the filters, not the missing
	// route, stop them.
	inNs(t, p.host, func() {
		host0, err := netlink.LinkByName("host0")
		require.NoError(t, err)
		err = netlink.RouteAdd(&netlink.Route{LinkIndex: host0.Attrs().Index, Gw: net.ParseIP("10.88.0.2"),
			Dst: &net.IPNet{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(24, 32)}})
		if !errors.Is(err, unix.EEXIST) {
			require.NoError(t, err)
		}
	})
	for i := range p.guests {
		_, err := dial(t, p.host, addr(i, 7))
		require.Error(t, err, "the host reached guest %d at its own address", i)
	}
	for i, ports := range testServices {
		for _, port := range ports {
			requireReply(t, p.host, fmt.Sprintf("10.88.0.2:%d", port), fmt.Sprintf("guest%d", i))
			got, err := dialUDP(t, p.host, fmt.Sprintf("10.88.0.2:%d", port))
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("guest%d", i), got, "udp probe %d", port)
			requireReply(t, p.guests[0], addr(i, int(port)), fmt.Sprintf("guest%d", i))
		}
		// Any port, not only the declared ones; the service sees the
		// primary's own address.
		requireReply(t, p.guests[0], addr(i, 7), PrimaryAddress.String())
		requireReply(t, p.guests[i], addr(0, 7), ServiceAddress(i).String()) // callback
		for j := range testServices {
			if j != i {
				requireReply(t, p.guests[i], addr(j, 7), ServiceAddress(i).String())
			}
		}
		_, err := dial(t, p.guests[i], "10.88.0.1:9000")
		require.Error(t, err, "service %d reached out of the pod", i)
	}
	requireReply(t, p.host, "10.88.0.2:8080", "guest0")
	requireReply(t, p.guests[0], "10.88.0.1:9000", "host")
	requireReply(t, p.guests[0], "10.88.0.1:9001", "10.88.0.2") // egress as the pod
}

func TestPodNetworkEveryStartOrder(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			p := bootPod(t, newTestPod(t), order...)
			checkPod(t, p)
		})
	}
}

// A Cilium-style /32 pod address, and an MTU of 1500.
func TestPodNetworkPodShapes(t *testing.T) {
	for name, o := range map[string]podOpts{"slash32": {MTU: 1450, Slash32: true}, "mtu1500": {MTU: 1500}} {
		t.Run(name, func(t *testing.T) {
			p := bootPod(t, newTestPod(t, o), 0, 1, 2)
			checkPod(t, p)
		})
	}
}

// The namespace defaults a node may set do not matter: br_netfilter on
// or off, forwarding off everywhere, strict reverse-path filtering.
func TestPodNetworkHostileDefaults(t *testing.T) {
	brnf := "/proc/sys/net/bridge/bridge-nf-call-iptables"
	for _, v := range []string{"0", "1"} {
		t.Run("bridge-nf-call-iptables="+v, func(t *testing.T) {
			if _, err := os.Stat(brnf); err != nil {
				t.Skip("no br_netfilter")
			}
			tp := newTestPod(t)
			sysctl(t, tp.pod, "net/bridge/bridge-nf-call-iptables", v)
			sysctl(t, tp.pod, "net/ipv4/ip_forward", "0")
			sysctl(t, tp.pod, "net/ipv4/conf/all/rp_filter", "1")
			sysctl(t, tp.pod, "net/ipv4/conf/default/rp_filter", "1")
			checkPod(t, bootPod(t, tp, 0, 1, 2))
		})
	}
}

// The monitor sets the network up after the pivot, where /proc/sys is
// read-only. The test re-runs itself in a mount namespace of its own with
// /proc/sys read-only, which no process of a multi-threaded test can
// enter by itself.
func TestPodNetworkReadOnlyProcSys(t *testing.T) {
	if os.Getenv("URUNC_TEST_RO_PROCSYS") == "" {
		if os.Geteuid() != 0 {
			t.Skip("needs root")
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestPodNetworkReadOnlyProcSys$", "-test.count=1") //nolint:gosec // the test binary itself
		cmd.Env = append(os.Environ(), "URUNC_TEST_RO_PROCSYS=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		require.Contains(t, string(out), "PASS")
		return
	}
	require.NoError(t, unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""))
	require.NoError(t, unix.Mount("/proc/sys", "/proc/sys", "", unix.MS_BIND, ""))
	require.NoError(t, unix.Mount("", "/proc/sys", "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""))
	require.Error(t, os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o600), "/proc/sys is still writable")
	checkPod(t, bootPod(t, newTestPod(t), 0, 1, 2))
}

// Ten containers set up at once, as a pod's start can overlap them.
func TestPodNetworkConcurrentSetup(t *testing.T) {
	tp := newTestPod(t)
	const n = 9
	infos := make([]*UnikernelNetworkInfo, n+1)
	errs := make([]error, n+1)
	var wg sync.WaitGroup
	for i := 0; i <= n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inNs(t, tp.pod, func() { infos[i], errs[i] = PodNetwork{Service: i, Ports: portsOf(i)}.NetworkSetup(0, 0) })
		}()
	}
	wg.Wait()
	guests := map[int]netns.NsHandle{}
	for i := 0; i <= n; i++ {
		require.NoError(t, errs[i], "container %d", i)
		guests[i], _ = tp.boot(t, infos[i])
		servePeer(t, guests[i], 7)
	}
	for i := 1; i <= n; i++ {
		requireReply(t, guests[0], addr(i, 7), PrimaryAddress.String())
		requireReply(t, guests[i], addr(0, 7), ServiceAddress(i).String())
		serve(t, guests[i], int(portsOf(i)[0]), "svc")
		requireReply(t, tp.host, fmt.Sprintf("10.88.0.2:%d", portsOf(i)[0]), "svc")
	}
}

func portsOf(i int) []uint16 {
	if i == 0 {
		return nil
	}
	return []uint16{uint16(10000 + i)} //nolint:gosec // i < 65
}

// A connection the primary opens from a service's port gets its replies:
// connection tracking, not the port, decides.
func TestPodNetworkNoPortCollision(t *testing.T) {
	tp := newTestPod(t)
	p := bootPod(t, tp, 0, 1)
	serve(t, p.host, 9000, "host")
	got, err := dialFrom(t, p.guests[0], 5432, "10.88.0.1:9000")
	require.NoError(t, err)
	require.Equal(t, "host", got)
}

// Restarts: of the primary, of a service (also one whose monitor exited
// on its own, leaving its tap), with the other side running throughout.
func TestPodNetworkRestarts(t *testing.T) {
	for _, i := range []int{0, 1} {
		for _, released := range []bool{true, false} {
			t.Run(fmt.Sprintf("container %d released %v", i, released), func(t *testing.T) {
				p := bootPod(t, newTestPod(t), 0, 1, 2)
				p.stops[i]()
				if released {
					inNs(t, p.pod, func() { require.NoError(t, part(i).Release()) })
				}
				info, err := p.setup(t, part(i))
				require.NoError(t, err)
				p.start(t, i, info)
				checkPod(t, p)
			})
		}
	}
}

// A connection the primary holds to a service survives the other
// service's restart, and the restarted service is reachable again.
func TestPodNetworkConnectionSurvivesAnotherRestart(t *testing.T) {
	p := bootPod(t, newTestPod(t), 0, 1, 2)
	serveFunc(t, p.guests[1], 9999, func(c net.Conn) { _, _ = io.Copy(c, c) })
	var c net.Conn
	inNs(t, p.guests[0], func() {
		var err error
		c, err = net.DialTimeout("tcp4", addr(1, 9999), 2*time.Second)
		require.NoError(t, err)
	})
	defer c.Close()
	p.stops[2]()
	inNs(t, p.pod, func() { require.NoError(t, part(2).Release()) })
	info, err := p.setup(t, part(2))
	require.NoError(t, err)
	p.start(t, 2, info)
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	_, err = c.Write([]byte("still"))
	require.NoError(t, err)
	b := make([]byte, 5)
	_, err = io.ReadFull(c, b)
	require.NoError(t, err)
	require.Equal(t, "still", string(b))
	requireReply(t, p.guests[0], addr(2, 6379), "guest2")
}

// payload is a reproducible stream of test bytes.
func payload(seed int64) io.Reader {
	return rand.New(rand.NewSource(seed)) //nolint:gosec // test data, not secrets
}

// 64 MiB each way, primary to service and host to service, arrives intact.
func TestPodNetworkLargeTransfers(t *testing.T) {
	for _, mtu := range []int{1450, 1500} {
		t.Run(fmt.Sprint(mtu), func(t *testing.T) {
			tp := newTestPod(t, podOpts{MTU: mtu})
			guests := map[int]netns.NsHandle{}
			for _, n := range []PodNetwork{{}, {Service: 1, Ports: []uint16{5432}}} {
				info, err := tp.setup(t, n)
				require.NoError(t, err)
				guests[n.Service], _ = tp.boot(t, info)
			}
			p := &pod{testPod: tp, guests: guests}
			const size = 64 << 20
			// The service hashes what it gets and sends back as much.
			serveFunc(t, p.guests[1], 5432, func(c net.Conn) {
				h := sha256.New()
				_, _ = io.CopyN(h, c, size)
				_, _ = c.Write(h.Sum(nil))
				_, _ = io.CopyN(c, payload(2), size)
			})
			for _, from := range []netns.NsHandle{p.guests[0], p.host} {
				target := addr(1, 5432)
				if from == p.host {
					target = "10.88.0.2:5432"
				}
				inNs(t, from, func() {
					c, err := net.DialTimeout("tcp4", target, 2*time.Second)
					require.NoError(t, err)
					defer c.Close()
					_ = c.SetDeadline(time.Now().Add(60 * time.Second))
					sent := sha256.New()
					_, err = io.CopyN(io.MultiWriter(c, sent), payload(1), size)
					require.NoError(t, err)
					sum := make([]byte, sha256.Size)
					_, err = io.ReadFull(c, sum)
					require.NoError(t, err)
					require.Equal(t, sent.Sum(nil), sum, "upload to %s", target)
					want := sha256.New()
					_, _ = io.CopyN(want, payload(2), size)
					got := sha256.New()
					_, err = io.CopyN(got, c, size)
					require.NoError(t, err)
					require.Equal(t, want.Sum(nil), got.Sum(nil), "download from %s", target)
				})
			}
		})
	}
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

// Releasing one container removes its tap and table and nothing else, and
// releasing twice, or what never ran, is fine.
func TestPodNetworkReleaseKeepsTheOthers(t *testing.T) {
	p := bootPod(t, newTestPod(t), 0, 1, 2)
	p.stops[1]()
	inNs(t, p.pod, func() {
		require.NoError(t, part(1).Release())
		require.NoError(t, part(1).Release())
		require.NoError(t, PodNetwork{Service: 5}.Release())
		_, err := netlink.LinkByName("tap1_urunc")
		require.Error(t, err)
	})
	requireReply(t, p.host, "10.88.0.2:6379", "guest2")
	requireReply(t, p.host, "10.88.0.2:8080", "guest0")
	_, err := dial(t, p.host, "10.88.0.2:5432")
	require.Error(t, err, "the released service's port still answers")
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
