# Service VMs next to a pod's main container

Every container of a pod that `urunc` runs boots its own VM, and by default
only one of them can have the pod's network at a time: the guest takes the
pod's address, and a second VM in the same pod is refused. A pod that needs
long-running helpers next to its main container, such as a database or a
stub API for the tests the main container runs, can list them as services
with the `com.urunc.unikernel.podServices` pod annotation:

```yaml
metadata:
  annotations:
    com.urunc.unikernel.podServices: "postgres:5432,redis:6379"
```

Each entry is a container name followed by the ports the pod's address
forwards to the service, separated by colons (`name:port[:port...]`, up to
16 ports, TCP and UDP); entries are comma separated, up to 64. A container
the annotation does not name is the pod's primary VM: the init containers
and the main container. As before, only one primary runs at a time.

containerd passes the annotation to `urunc` only if the runtime's
`pod_annotations` allows it (`com.urunc.unikernel.*`, see
[installation](../installation.md)).

## Addresses

Every VM has an address of its own on a bridge in the pod's network
namespace, `198.18.0.0/24`:

| VM | Address |
|---|---|
| service *i* (the *i*-th entry, from 1) | `198.18.0.<i>`: `postgres` above is `198.18.0.1` |
| the primary | `198.18.0.250` |
| the gateway (the bridge) | `198.18.0.254` |

## What each VM can reach

- **The VMs reach each other** on any port and protocol: the primary
  reaches `198.18.0.1:5432`, and a service can call the primary back at
  `198.18.0.250` or reach another service.
- **The primary's traffic out of the pod** leaves with the pod's address
  (SNAT), so network policy sees the pod as before.
- **Connections to the pod's address** on a service's ports go to that
  service (DNAT), so kubelet probes on those ports reach it; every other
  connection to the pod's address goes to the primary.
- **A service cannot open a connection out of the pod.** It only answers.
- **Nothing outside the pod reaches a VM at its `198.18.0.x` address**,
  even with a route to it: only connections to the pod's address get in.

The primary does not reach a service at `localhost`, unlike containers that
share a kernel: each VM has its own loopback. The primary's guest sees
`198.18.0.250` as its own address, not the pod's.

## How it works

The first container of the pod creates the bridge `br_urunc`
(`198.18.0.254/24`, the pod interface's MTU, a fixed MAC) and turns IPv4
forwarding on for it and the pod interface. Each container then puts its tap
on the bridge and installs one nftables table, `urunc_pod_<i>` (0 for the
primary):

- a service's table DNATs its ports on the pod's address to it, and drops
  its new connections out of the pod;
- the primary's table DNATs every other connection to the pod's address to
  it (at a later priority, so the services' ports win) and SNATs its
  traffic out of the pod;
- both drop new connections from outside the pod to their own address
  that were not DNATed.

Each container replaces only its own table, in one atomic batch, and on
stop removes only its tap and its table. Everything goes through netlink
(nftables, and `RTM_SETLINK` for forwarding), because the monitor sets the
network up after it pivots into the container's root, where there is no
`iptables` and `/proc/sys` is read-only.

The kernel needs the bridge, `nf_tables`, `nft_nat`, `nft_chain_nat`,
`nf_conntrack` and `nft_ct`.

## Limits

- IPv4 only, and the range is fixed.
- The primary reaching itself or a service through the pod's address
  (hairpin) is not supported: use the `198.18.0.x` addresses.
- Services have no egress.
