# Service VMs next to a pod's main container

Every container of a pod that `urunc` runs boots its own VM, and by default
only one of them can have the pod's network at a time: the guest takes the
pod's address, and a second VM in the same pod is refused. A pod that needs a
long-running helper next to its main container, such as a database or a cache
for the tests the main container runs, can list those helpers as services
with the `com.urunc.unikernel.podServices` pod annotation:

```yaml
metadata:
  annotations:
    com.urunc.unikernel.podServices: "postgres:5432,redis:6379"
```

Each entry is a container name followed by the TCP ports the service accepts
connections on, separated by colons (`name:port[:port...]`, up to 16 ports);
entries are comma separated, up to 64. A container the annotation does not
name is the pod's primary VM: the init containers and the main container. As
before, only one primary runs at a time.

containerd passes the annotation to `urunc` only if the runtime's
`pod_annotations` allows it (`com.urunc.unikernel.*`, see
[installation](../installation.md)).

## What each VM can reach

- **The primary** keeps the network a single VM gets: the pod's address, its
  egress and every connection to the pod, except the services' ports.
- **Service *i*** (the *i*-th entry, counting from 1) is reached by the
  primary at `198.18.0.<i>`, on any port: `postgres` above is
  `198.18.0.1:5432`. The service sees the connection come from that address.
- **Connections to the pod's address on a service's ports** go to that
  service, so kubelet probes on those ports reach it.
- **A service cannot reach out of the pod.** Only its replies on its own
  ports and its ARP leave it.

The primary does not reach a service at `localhost`, unlike containers that
share a kernel: each VM has its own loopback.

## How it works

Every guest gets the pod's address and MAC. tc filters in the pod's network
namespace sort the frames:

- on the pod interface, a service's ports go to its tap, every ARP frame is
  copied to each service's tap, and everything else goes to the primary;
- frames from the primary to `198.18.0.<i>` and the service's replies are
  redirected between the two taps, with `pedit` rewriting the source to
  `198.18.0.<i>` and the destination to the pod's address and MAC;
- a service's tap passes its ports' replies and ARP to the pod interface and
  drops everything else.

Each container sets up and removes only its own tap; whichever of a primary
and a service starts second links the two. The kernel needs the `u32` and
`matchall` classifiers and the `mirred`, `pedit`, `csum` and `gact` actions.

## Limits

- IPv4 and TCP ports only.
- A connection the primary opens from a source port that is also a service's
  port gets its replies sent to that service. Linux picks source ports from
  32768 up by default, so keep service ports below that.
