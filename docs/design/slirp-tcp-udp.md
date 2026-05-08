# Slirp TCP/UDP NAT — Design

Status: **Deferred**. The MVP shipped on `feat/slirp-net-and-atomic-disk-meta`
covers ARP, DHCPv4, and ICMP echo to the gateway. TCP and UDP outbound NAT
is the next chunk and is described here so it can be picked up cleanly.

## Goal

Make `--net slirp` boot real workloads — outbound HTTP, DNS, package
managers, etc. — without `CAP_NET_ADMIN`, `/dev/net/tun`, or any iptables
rules. The host-side identity is the gocracker process's own UID; outbound
sockets dial through the host's normal kernel stack.

## Surface

The slirp engine is the in-process userspace network stack at
[`internal/slirp/`](../../internal/slirp/). Today it implements
`virtio.NetBackend`; TCP/UDP NAT is added inside the same package.

External APIs do not change. `NetworkMode = "slirp"` already exists at the
container, API, and CLI layers, and the kernel cmdline already stamps the
fixed plan (`10.0.2.15/24`, gw `10.0.2.2`, dns `10.0.2.3`).

## UDP

Simpler of the two. Per outbound flow keyed by
`(guestIP, guestPort, dstIP, dstPort)`:

1. On first egress UDP datagram, dial a host
   `*net.UDPConn` to `dstIP:dstPort`.
2. Spawn a goroutine reading replies; wrap each reply in IPv4+UDP+Ethernet
   and enqueue onto `s.rxQueue` with `dstMAC = guestMAC` and
   `srcIP = original dstIP`.
3. Idle expiry: 30 seconds. Map keyed flow → conn + lastActive timestamp;
   single janitor goroutine sweeps every 5 seconds.

Special case: `dstIP == 10.0.2.3 && dstPort == 53`. Forward to the host's
resolver. Easiest path: replace `dstIP` with `127.0.0.53` (or whatever the
host's first nameserver is) before the dial. Read the host's resolver
config from `/etc/resolv.conf` once on engine startup, not per-flow.

## TCP

Trickier — we are presenting a full TCP endpoint to the guest but proxy at
the application byte stream on the host side.

The clean path is to use **gVisor's `gvisor.dev/gvisor/pkg/tcpip`** netstack
in tap mode: it speaks Ethernet at one end (linked to our virtio frame
queue) and exposes a Go `net.Listener` / dialer at the other. We then
forward bytes between gVisor's listener-accepted conn and a host
`net.Dial("tcp", ...)`. This skips writing a TCP state machine.

**Cost:** ~50 MB of code in `go.sum`, ~5 MB in the linked binary. That
breaks gocracker's "single small static binary" claim in a meaningful way.

**Alternative — write a TCP state machine ourselves:** ~1500–2000 LoC for
correct retransmits, ordering, MSS clamping, RST handling. We get to skip
congestion control because we control both endpoints in-process: the
guest's TCP stack does the heavy lifting; our half just acks promptly and
shovels bytes to the host socket. RTOs still need to be handled, plus
sequence space wraparound, FIN handshake, and PAWS.

**Recommendation:** start with the gVisor path behind a build tag. Keep
the cgo-free pure-Go default that includes only the MVP. If size becomes
a real complaint, swap to a hand-rolled TCP state machine — by then we'll
have a working baseline to compare against.

```
// Build tag layout:
//   //go:build slirp_gvisor
//   internal/slirp/tcp_gvisor.go   — uses gvisor netstack
//
//   //go:build !slirp_gvisor
//   internal/slirp/tcp_stub.go     — current behaviour (drops with metric)
```

## Port forwarding (host → guest)

Out of scope for the first TCP/UDP cut. Plan for a follow-up:

- `--publish HOST_IP:HOST_PORT:GUEST_PORT/{tcp,udp}` flag at the CLI/API.
- Slirp engine binds the host socket and proxies bytes / datagrams to a
  guest-bound TCP/UDP flow.
- Same plan as `auto` mode's iptables DNAT, but in userspace.

## Testing

The MVP test harness in `internal/slirp/slirp_test.go` already proves the
ARP and DHCP packet shapes. TCP/UDP tests should:

- Use a `net.Listener` on `127.0.0.1` as the "external server" (since the
  slirp engine dials through `net.Dial`, the loopback works fine).
- Drive the slirp engine with crafted Ethernet frames and assert the
  reply frame parses back to the expected payload.
- Add a leak test that confirms idle flows are torn down.

## Migration / snapshot

In-process slirp state (flow table, ARP cache) is *not* persisted across
snapshots. After a restore, the guest re-ARPs and re-establishes flows.
That is acceptable — node-vmm's libslirp has the same property.
