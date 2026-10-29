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
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// A pod network lets one pod run service VMs (a database, a cache) next to
// its primary VM, the container that does the pod's work, when the pod lists
// them in the com.urunc.unikernel.podServices annotation.
//
// Every guest gets the pod's address and MAC, as the one guest of a dynamic
// network does, and tc filters on the pod's interface and the taps sort the
// frames between them:
//   - the pod's interface sends a service's TCP ports to that service and
//     everything else to the primary, and copies every ARP frame to each
//     service, so they all resolve the gateway;
//   - a service sends out of the pod only its ports' replies and ARP;
//   - the primary reaches service i at ServiceAddress(i): those frames go
//     straight to the service's tap with the addresses rewritten, so the
//     service sees a peer at ServiceAddress(i) and never its own address.
//
// Each container sets up and removes only its own tap; whichever of a primary
// and a service comes up second links the two.

const (
	// MaxPodServices bounds the services of a pod; ServiceAddress's last
	// byte and the filter priorities below depend on it.
	MaxPodServices  = 64
	maxServicePorts = 16

	// Filter priorities (a lower one runs first), one filter per priority.
	prioServiceARP    = 10   // + i, on the pod interface: copy ARP to service i
	prioServicePort   = 100  // + (i-1)*maxServicePorts + k, on the pod interface
	prioToService     = 10   // + i, on the primary's tap
	prioFromService   = 1    // on a service's tap: its replies to the primary
	prioServiceOut    = 2    // + k, on a service's tap: its port k's replies
	prioServiceOwnARP = 100  // on a service's tap: its ARP
	prioCatchAll      = 2000 // primary: both ways; service: drop the rest
)

var podTapRe = regexp.MustCompile(`^tap(\d+)_urunc$`)

// PodNetwork is one container's part in a pod network: Service is 0 for the
// primary and i (1..MaxPodServices) for the pod's i-th service, which accepts
// connections from outside the guest on Ports.
type PodNetwork struct {
	Service int
	Ports   []uint16
}

// ServiceAddress is the address the primary reaches service i at. The range
// (198.18.0.0/15, for benchmarking) is not routed anywhere, and the frames
// never leave the pod.
func ServiceAddress(i int) net.IP {
	return net.IPv4(198, 18, 0, byte(i)).To4()
}

func podTapName(i int) string {
	return strings.ReplaceAll(DefaultTap, "X", strconv.Itoa(i))
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

// NetworkSetup creates the container's tap and its filters, and links it to
// the primary or the services already running.
func (n PodNetwork) NetworkSetup(uid uint32, gid uint32) (*UnikernelNetworkInfo, error) {
	if err := n.validate(); err != nil {
		return nil, err
	}
	eth, err := discoverContainerIface()
	if err != nil {
		return nil, fmt.Errorf("failed to find container interface: %w", err)
	}
	ifInfo, err := getInterfaceInfo(eth.Attrs().Name)
	if err != nil {
		return nil, fmt.Errorf("getInterfaceInfo(%s) failed: %w", eth.Attrs().Name, err)
	}

	name := podTapName(n.Service)
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
	tap, err := networkSetup(name, "", eth, false, uid, gid)
	if err != nil {
		return nil, fmt.Errorf("networkSetup(%s) failed: %w", name, err)
	}
	err = n.addFilters(eth, tap, net.ParseIP(ifInfo.IP).To4(), eth.Attrs().HardwareAddr)
	if err != nil {
		if delErr := netlink.LinkDel(tap); delErr != nil {
			netlog.Warnf("failed to remove %s after a failed setup: %v", name, delErr)
		}
		return nil, err
	}
	return &UnikernelNetworkInfo{TapDevice: name, EthDevice: ifInfo}, nil
}

// Release removes the container's tap, and the filters on it with it. The
// other containers' filters that point at it drop those frames until the
// container's next VM replaces them.
func (n PodNetwork) Release() error {
	link, err := netlink.LinkByName(podTapName(n.Service))
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return err
	}
	return netlink.LinkDel(link)
}

func (n PodNetwork) addFilters(eth, tap netlink.Link, podIP net.IP, podMAC net.HardwareAddr) error {
	for _, l := range []netlink.Link{eth, tap} {
		if err := ensureIngressQdisc(l); err != nil {
			return fmt.Errorf("failed to add an ingress qdisc to %s: %w", l.Attrs().Name, err)
		}
	}
	if n.Service == 0 {
		err := errors.Join(
			setFilter(&netlink.MatchAll{FilterAttrs: ingressAttrs(eth, prioCatchAll, unix.ETH_P_ALL), Actions: redirectTo(tap)}),
			setFilter(&netlink.MatchAll{FilterAttrs: ingressAttrs(tap, prioCatchAll, unix.ETH_P_ALL), Actions: redirectTo(eth)}),
		)
		if err != nil {
			return err
		}
		links, err := netlink.LinkList()
		if err != nil {
			return err
		}
		for _, l := range links {
			m := podTapRe.FindStringSubmatch(l.Attrs().Name)
			if m == nil {
				continue
			}
			if i, _ := strconv.Atoi(m[1]); i >= 1 && i <= MaxPodServices {
				if err := linkService(tap, l, i, podIP, podMAC); err != nil {
					return err
				}
			}
		}
		return nil
	}

	i := n.Service
	errs := []error{
		setFilter(&netlink.MatchAll{FilterAttrs: ingressAttrs(tap, prioServiceOwnARP, unix.ETH_P_ARP), Actions: redirectTo(eth)}),
		setFilter(&netlink.MatchAll{FilterAttrs: ingressAttrs(tap, prioCatchAll, unix.ETH_P_ALL), Actions: []netlink.Action{
			&netlink.GenericAction{ActionAttrs: netlink.ActionAttrs{Action: netlink.TC_ACT_SHOT}},
		}}),
		// The gateway's ARP replies reach the primary; the service needs them too.
		setFilter(&netlink.MatchAll{FilterAttrs: ingressAttrs(eth, prioServiceARP+i, unix.ETH_P_ARP), Actions: []netlink.Action{
			&netlink.MirredAction{
				ActionAttrs:  netlink.ActionAttrs{Action: netlink.TC_ACT_UNSPEC}, // go on to the next filter
				MirredAction: netlink.TCA_EGRESS_MIRROR,
				Ifindex:      tap.Attrs().Index,
			},
		}}),
	}
	for k, p := range n.Ports {
		errs = append(errs,
			setFilter(&netlink.U32{FilterAttrs: ingressAttrs(tap, prioServiceOut+k, unix.ETH_P_IP),
				Sel: u32Sel(keyTCP, nl.TcU32Key{Mask: 0xffff0000, Val: uint32(p) << 16, Off: 20}), Actions: redirectTo(eth)}),
			setFilter(&netlink.U32{FilterAttrs: ingressAttrs(eth, prioServicePort+(i-1)*maxServicePorts+k, unix.ETH_P_IP),
				Sel: u32Sel(keyTCP, nl.TcU32Key{Mask: 0x0000ffff, Val: uint32(p), Off: 20}), Actions: redirectTo(tap)}),
		)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	primary, err := netlink.LinkByName(podTapName(0))
	if err != nil {
		return nil // the primary links the service when it comes up
	}
	return linkService(primary, tap, i, podIP, podMAC)
}

// linkService lets the primary reach service i at ServiceAddress(i). Both
// guests have the pod's address, so the frames each way get the service
// address as their source and the pod's as their destination.
func linkService(primary, service netlink.Link, i int, podIP net.IP, podMAC net.HardwareAddr) error {
	addr := ServiceAddress(i)
	sel := u32Sel(nl.TcU32Key{Mask: 0xffffffff, Val: binary.BigEndian.Uint32(addr), Off: 16})
	rewrite := func(to netlink.Link) []netlink.Action {
		return append([]netlink.Action{
			&netlink.PeditAction{ActionAttrs: netlink.ActionAttrs{Action: netlink.TC_ACT_PIPE}, SrcIP: addr, DstIP: podIP, DstMacAddr: podMAC},
			&netlink.CsumAction{ActionAttrs: netlink.ActionAttrs{Action: netlink.TC_ACT_PIPE},
				UpdateFlags: netlink.TCA_CSUM_UPDATE_FLAG_IPV4HDR | netlink.TCA_CSUM_UPDATE_FLAG_ICMP |
					netlink.TCA_CSUM_UPDATE_FLAG_TCP | netlink.TCA_CSUM_UPDATE_FLAG_UDP},
		}, redirectTo(to)...)
	}
	return errors.Join(
		setFilter(&netlink.U32{FilterAttrs: ingressAttrs(primary, prioToService+i, unix.ETH_P_IP), Sel: sel, Actions: rewrite(service)}),
		setFilter(&netlink.U32{FilterAttrs: ingressAttrs(service, prioFromService, unix.ETH_P_IP), Sel: sel, Actions: rewrite(primary)}),
	)
}

// keyTCP matches an IPv4 packet carrying TCP. The port keys after it assume
// a header without options, as Linux sends.
var keyTCP = nl.TcU32Key{Mask: 0x00ff0000, Val: unix.IPPROTO_TCP << 16, Off: 8}

func u32Sel(keys ...nl.TcU32Key) *nl.TcU32Sel {
	return &nl.TcU32Sel{Flags: nl.TC_U32_TERMINAL, Keys: keys}
}

func ingressAttrs(link netlink.Link, prio int, proto uint16) netlink.FilterAttrs {
	return netlink.FilterAttrs{
		LinkIndex: link.Attrs().Index,
		Parent:    netlink.MakeHandle(0xffff, 0),
		Priority:  uint16(prio), //nolint:gosec // at most prioCatchAll
		Protocol:  proto,
	}
}

func redirectTo(link netlink.Link) []netlink.Action {
	return []netlink.Action{&netlink.MirredAction{
		ActionAttrs:  netlink.ActionAttrs{Action: netlink.TC_ACT_STOLEN},
		MirredAction: netlink.TCA_EGRESS_REDIR,
		Ifindex:      link.Attrs().Index,
	}}
}

// setFilter makes f the only filter at its priority, so that a container's
// next VM replaces what its previous one left (a tap that is gone, or one
// that came back with a new index).
func setFilter(f netlink.Filter) error {
	if err := netlink.FilterDel(f); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("failed to remove the filter at priority %d: %w", f.Attrs().Priority, err)
	}
	if err := netlink.FilterAdd(f); err != nil {
		return fmt.Errorf("failed to add a %s filter at priority %d: %w", f.Type(), f.Attrs().Priority, err)
	}
	return nil
}

func ensureIngressQdisc(link netlink.Link) error {
	if err := addIngressQdisc(link); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	return nil
}
