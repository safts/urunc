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
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// A pod network lets one pod run service VMs (a database, a stub API) next
// to its primary VM, the container that does the pod's work, when the pod
// lists them in the com.urunc.unikernel.podServices annotation.
//
// Every VM gets an address of its own on a bridge in the pod's network
// namespace, and the namespace routes between the bridge and the pod
// interface:
//   - the primary's traffic out of the pod is SNATed to the pod's address,
//     and connections to the pod's address that no service claims are
//     DNATed to the primary;
//   - connections to the pod's address on a service's ports (TCP and UDP)
//     are DNATed to that service, so probes reach it;
//   - the VMs reach each other on the bridge, and a service cannot open a
//     connection out of the pod.
//
// The monitor sets this up after the pivot into the container's root, so
// everything goes through netlink: nftables for the NAT and the filter, and
// RTM_SETLINK for forwarding (/proc/sys is read-only there). Each container
// owns its tap and one nftables table and removes only those; the bridge
// and forwarding are shared and set up idempotently by whoever comes first.

const (
	// MaxPodServices bounds the services of a pod: service i is
	// 198.18.0.<i>.
	MaxPodServices  = 64
	maxServicePorts = 16

	podBridge = "br_urunc"
	// The services' DNAT chains run before the primary's catch-all: the
	// first NAT chain that maps a connection wins.
	prioServiceDNAT = -100
	prioPrimaryDNAT = -90
)

var (
	podSubnet = net.IPv4Mask(255, 255, 255, 0)
	// PrimaryAddress is the primary VM's address on the pod network.
	PrimaryAddress = net.IPv4(198, 18, 0, 250).To4()
	podGateway     = net.IPv4(198, 18, 0, 254).To4()
)

// PodNetwork is one container's part in a pod network: Service is 0 for the
// primary and i (1..MaxPodServices) for the pod's i-th service, which the
// pod's address forwards Ports to. Owner is the container's ID, which its tap
// carries as its alias: a container's instances share their tap's name, and
// an old instance's release must not remove its successor's.
type PodNetwork struct {
	Service int
	Ports   []uint16
	Owner   string
}

// ServiceAddress is service i's address on the pod network (198.18.0.0/15
// is for benchmarking; it is routed nowhere, and only the pod's namespace
// knows this /24).
func ServiceAddress(i int) net.IP {
	return net.IPv4(198, 18, 0, byte(i)).To4()
}

func (n PodNetwork) address() net.IP {
	if n.Service == 0 {
		return PrimaryAddress
	}
	return ServiceAddress(n.Service)
}

// mac is the guest's: locally administered, from its address.
func (n PodNetwork) mac() net.HardwareAddr {
	return net.HardwareAddr{0x02, 0x75, 0x72, 0x75, 0x00, n.address()[3]}
}

func (n PodNetwork) tapName() string {
	return strings.ReplaceAll(DefaultTap, "X", strconv.Itoa(n.Service))
}

func (n PodNetwork) table() *nftables.Table {
	return &nftables.Table{Family: nftables.TableFamilyIPv4, Name: "urunc_pod_" + strconv.Itoa(n.Service)}
}

// portTable pins the container's bridge port to its guest's MAC and
// address, so that no guest sends as another.
func (n PodNetwork) portTable() *nftables.Table {
	return &nftables.Table{Family: nftables.TableFamilyBridge, Name: "urunc_pod_" + strconv.Itoa(n.Service)}
}

func (n PodNetwork) validate() error {
	switch {
	case n.Service < 0 || n.Service > MaxPodServices:
		return fmt.Errorf("pod network: service index %d is out of 0..%d", n.Service, MaxPodServices)
	case n.Service == 0 && len(n.Ports) > 0:
		return errors.New("pod network: the primary has no service ports")
	case len(n.Ports) > maxServicePorts:
		return fmt.Errorf("pod network: %d ports, at most %d", len(n.Ports), maxServicePorts)
	}
	for _, p := range n.Ports {
		if p == 0 {
			return errors.New("pod network: port 0")
		}
	}
	return nil
}

// NetworkSetup puts the container's tap on the pod's bridge and installs
// its NAT and filter rules.
func (n PodNetwork) NetworkSetup(uid uint32, gid uint32) (*UnikernelNetworkInfo, error) {
	if err := n.validate(); err != nil {
		return nil, err
	}
	eth, err := discoverContainerIface()
	if err != nil {
		return nil, fmt.Errorf("failed to find container interface: %w", err)
	}
	podIP, err := ipv4Of(eth)
	if err != nil {
		return nil, err
	}
	br, err := ensurePodBridge(eth)
	if err != nil {
		return nil, err
	}

	name := n.tapName()
	// A tap left by this container's previous VM, whose monitor exited on
	// its own, is removed; one a monitor holds belongs to a running VM.
	if old, err := netlink.LinkByName(name); err == nil {
		if tapInUse(old) {
			return nil, fmt.Errorf("pod network: %s is in use: this container's VM is already running", name)
		}
		if err := netlink.LinkDel(old); err != nil {
			return nil, fmt.Errorf("failed to remove the stale %s: %w", name, err)
		}
	}
	tap, err := createTapDevice(name, eth.Attrs().MTU, uid, gid)
	if err != nil {
		return nil, fmt.Errorf("createTapDevice(%s) failed: %w", name, err)
	}
	err = errors.Join(netlink.LinkSetAlias(tap, n.Owner), netlink.LinkSetMaster(tap, br), netlink.LinkSetUp(tap),
		n.setRules(eth.Attrs().Name, podIP))
	if err == nil && n.Service > 0 {
		err = forgetFlows(podIP, n.Ports)
	}
	if err != nil {
		if delErr := netlink.LinkDel(tap); delErr != nil {
			netlog.Warnf("failed to remove %s after a failed setup: %v", name, delErr)
		}
		return nil, fmt.Errorf("pod network: %w", err)
	}
	return &UnikernelNetworkInfo{
		TapDevice: name,
		EthDevice: Interface{
			IP:             n.address().String(),
			DefaultGateway: podGateway.String(),
			Mask:           net.IP(podSubnet).String(),
			Interface:      eth.Attrs().Name,
			MAC:            n.mac().String(),
			MTU:            eth.Attrs().MTU,
		},
	}, nil
}

// Release removes the container's tap and its tables, once its VM is gone:
// it leaves a tap a monitor holds, and one another instance of the
// container (another Owner) set up since. The bridge stays for the pod's
// other VMs, until the namespace goes. An empty Owner releases whatever has
// the container's tap name.
func (n PodNetwork) Release() error {
	link, err := netlink.LinkByName(n.tapName())
	var notFound netlink.LinkNotFoundError
	switch {
	case err == nil:
		if tapInUse(link) || (n.Owner != "" && link.Attrs().Alias != n.Owner) {
			return nil
		}
		if err := netlink.LinkDel(link); err != nil {
			return err
		}
	case !errors.As(err, &notFound):
		return err
	}
	// Adding first makes the delete succeed when there is no table.
	return nftFlush(func(c *nftables.Conn) {
		for _, t := range []*nftables.Table{n.table(), n.portTable()} {
			c.AddTable(t)
			c.DelTable(t)
		}
	})
}

// forgetFlows drops the conntrack entries of UDP flows to the pod's address
// on ports: one that started before the service's rules went in stays mapped
// to the primary, or to the namespace itself, for as long as its client
// keeps sending.
func forgetFlows(podIP net.IP, ports []uint16) error {
	var filters []netlink.CustomConntrackFilter
	for _, p := range ports {
		f := &netlink.ConntrackFilter{}
		if err := errors.Join(f.AddProtocol(unix.IPPROTO_UDP), f.AddIP(netlink.ConntrackOrigDstIP, podIP),
			f.AddPort(netlink.ConntrackOrigDstPort, p)); err != nil {
			return err
		}
		filters = append(filters, f)
	}
	if len(filters) == 0 {
		return nil
	}
	_, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, unix.AF_INET, filters...)
	return err
}

func ipv4Of(link netlink.Link) (net.IP, error) {
	addrs, err := retryDump(func() ([]netlink.Addr, error) { return netlink.AddrList(link, netlink.FAMILY_V4) })
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if ip := a.IP.To4(); ip != nil && !ip.IsLoopback() {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("pod network: %s has no IPv4 address", link.Attrs().Name)
}

// ensurePodBridge creates the pod's bridge, or takes the one another
// container created, and turns forwarding on for it and the pod interface.
// Its MAC is fixed: a bridge that takes its lowest port's would change the
// guests' gateway under them as taps come and go.
func ensurePodBridge(eth netlink.Link) (netlink.Link, error) {
	attrs := netlink.NewLinkAttrs()
	attrs.Name = podBridge
	attrs.MTU = eth.Attrs().MTU
	attrs.HardwareAddr = net.HardwareAddr{0x02, 0x75, 0x72, 0x75, 0x00, podGateway[3]}
	if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: attrs}); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("failed to create %s: %w", podBridge, err)
	}
	br, err := netlink.LinkByName(podBridge)
	if err != nil {
		return nil, err
	}
	err = errors.Join(
		netlink.AddrReplace(br, &netlink.Addr{IPNet: &net.IPNet{IP: podGateway, Mask: podSubnet}}),
		netlink.LinkSetUp(br),
		setForwarding(br),
		setForwarding(eth),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to set up %s: %w", podBridge, err)
	}
	return br, nil
}

// setForwarding turns IPv4 forwarding on for link, as
// net.ipv4.conf.<link>.forwarding=1 does, through RTM_SETLINK: the kernel
// forwards what arrives on an interface that has it on.
func setForwarding(link netlink.Link) error {
	const ipv4DevconfForwarding = 1 // IPV4_DEVCONF_FORWARDING (the index is 1-based)
	req := nl.NewNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(link.Attrs().Index) //nolint:gosec // an ifindex
	req.AddData(msg)
	af := nl.NewRtAttr(unix.IFLA_AF_SPEC, nil)
	conf := af.AddRtAttr(unix.AF_INET, nil).AddRtAttr(unix.IFLA_INET_CONF, nil)
	conf.AddRtAttr(ipv4DevconfForwarding, nl.Uint32Attr(1))
	req.AddData(af)
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("failed to turn forwarding on for %s: %w", link.Attrs().Name, err)
	}
	return nil
}

// nftFlush sends the batch fill builds to nftables in the calling thread's
// network namespace. The connection enters the namespace by its fd when it
// flushes, so the fd stays open until then.
func nftFlush(fill func(c *nftables.Conn)) error {
	ns, err := netns.Get()
	if err != nil {
		return err
	}
	defer ns.Close()
	c, err := nftables.New(nftables.WithNetNSFd(int(ns)))
	if err != nil {
		return err
	}
	fill(c)
	return c.Flush()
}

// setRules replaces the container's table with its rules, in one batch.
func (n PodNetwork) setRules(eth string, podIP net.IP) error {
	return nftFlush(func(c *nftables.Conn) { n.addRules(c, eth, podIP) })
}

func (n PodNetwork) addRules(c *nftables.Conn, eth string, podIP net.IP) {
	n.addPortRules(c)
	t := n.table()
	c.AddTable(t)
	c.DelTable(t)
	c.AddTable(t)
	addr := n.address()
	toPod := slices.Concat(ifIs(expr.MetaKeyIIFNAME, eth), ipIs(ipDst, podIP))

	if n.Service == 0 {
		pre := c.AddChain(&nftables.Chain{Name: "prerouting", Table: t, Type: nftables.ChainTypeNAT,
			Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityRef(prioPrimaryDNAT)})
		c.AddRule(&nftables.Rule{Table: t, Chain: pre, Exprs: slices.Concat(toPod, natTo(expr.NATTypeDestNAT, addr))})
		post := c.AddChain(&nftables.Chain{Name: "postrouting", Table: t, Type: nftables.ChainTypeNAT,
			Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource})
		c.AddRule(&nftables.Rule{Table: t, Chain: post, Exprs: slices.Concat(
			ifIs(expr.MetaKeyOIFNAME, eth), ipIs(ipSrc, addr), natTo(expr.NATTypeSourceNAT, podIP))})
		addForward(c, t, slices.Concat(ifIs(expr.MetaKeyIIFNAME, eth), ipIs(ipDst, addr), unmapped()))
		return
	}

	pre := c.AddChain(&nftables.Chain{Name: "prerouting", Table: t, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityRef(prioServiceDNAT)})
	for _, proto := range []byte{unix.IPPROTO_TCP, unix.IPPROTO_UDP} {
		for _, p := range n.Ports {
			c.AddRule(&nftables.Rule{Table: t, Chain: pre, Exprs: slices.Concat(toPod, []expr.Any{
				&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(p)},
			}, natTo(expr.NATTypeDestNAT, addr))})
		}
	}
	// Out of the pod a service may only answer: its new connections would
	// leave with an address nothing routes back, and are dropped.
	addForward(c, t,
		slices.Concat(ifIs(expr.MetaKeyOIFNAME, eth), ipIs(ipSrc, addr), ctBits(expr.CtKeySTATE, ctNew, false)),
		slices.Concat(ifIs(expr.MetaKeyIIFNAME, eth), ipIs(ipDst, addr), unmapped()))
}

// addPortRules drops what arrives on the container's bridge port from
// another MAC, or as IPv4 or ARP from another address.
func (n PodNetwork) addPortRules(c *nftables.Conn) {
	t := n.portTable()
	c.AddTable(t)
	c.DelTable(t)
	c.AddTable(t)
	pre := c.AddChain(&nftables.Chain{Name: "prerouting", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityRef(-300)})
	port := ifIs(expr.MetaKeyIIFNAME, n.tapName())
	isEther := func(etherType uint16) []expr.Any {
		return []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseLLHeader, Offset: 12, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(etherType)},
		}
	}
	not := func(base expr.PayloadBase, offset, length uint32, want []byte) []expr.Any {
		return []expr.Any{
			&expr.Payload{DestRegister: 1, Base: base, Offset: offset, Len: length},
			&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: want},
		}
	}
	drop := &expr.Verdict{Kind: expr.VerdictDrop}
	for _, m := range [][]expr.Any{
		slices.Concat(port, not(expr.PayloadBaseLLHeader, 6, 6, n.mac())),
		slices.Concat(port, isEther(unix.ETH_P_IP), not(expr.PayloadBaseNetworkHeader, ipSrc, 4, n.address())),
		// The ARP sender's protocol address, in an Ethernet/IPv4 request or reply.
		slices.Concat(port, isEther(unix.ETH_P_ARP), not(expr.PayloadBaseNetworkHeader, 14, 4, n.address())),
	} {
		c.AddRule(&nftables.Rule{Table: t, Chain: pre, Exprs: append(m, drop)})
	}
}

// addForward adds a forward chain that drops what each match matches.
func addForward(c *nftables.Conn, t *nftables.Table, matches ...[]expr.Any) {
	fwd := c.AddChain(&nftables.Chain{Name: "forward", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter})
	for _, m := range matches {
		c.AddRule(&nftables.Rule{Table: t, Chain: fwd, Exprs: append(m, &expr.Verdict{Kind: expr.VerdictDrop})})
	}
}

// ctNew is every state but an answer to a connection seen before.
const ctNew = expr.CtStateBitNEW | expr.CtStateBitINVALID | expr.CtStateBitUNTRACKED

// ctDstNAT is IPS_DST_NAT: the connection was DNATed.
const ctDstNAT = 1 << 5

// unmapped matches a new connection that reached a VM's own address from
// outside the pod, not through a DNAT of the pod's address: nothing routes
// the pod network to the pod, and nothing should.
func unmapped() []expr.Any {
	return slices.Concat(ctBits(expr.CtKeySTATE, ctNew, false), ctBits(expr.CtKeySTATUS, ctDstNAT, true))
}

// ctBits matches a conntrack key having one of bits set, or, with none,
// none of them.
func ctBits(key expr.CtKey, bits uint32, none bool) []expr.Any {
	op := expr.CmpOpNeq
	if none {
		op = expr.CmpOpEq
	}
	return []expr.Any{
		&expr.Ct{Key: key, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: binaryutil.NativeEndian.PutUint32(bits), Xor: make([]byte, 4)},
		&expr.Cmp{Op: op, Register: 1, Data: make([]byte, 4)},
	}
}

// Offsets of the IPv4 source and destination addresses.
const (
	ipSrc = 12
	ipDst = 16
)

// ifIs matches the input or output interface's name.
func ifIs(key expr.MetaKey, name string) []expr.Any {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return []expr.Any{
		&expr.Meta{Key: key, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: b},
	}
}

func ipIs(offset uint32, ip net.IP) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ip.To4()},
	}
}

func natTo(kind expr.NATType, to net.IP) []expr.Any {
	return []expr.Any{
		&expr.Immediate{Register: 1, Data: to.To4()},
		&expr.NAT{Type: kind, Family: unix.NFPROTO_IPV4, RegAddrMin: 1},
	}
}
