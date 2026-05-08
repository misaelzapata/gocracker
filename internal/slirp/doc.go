// Package slirp is an in-process userspace network stack for gocracker
// microVMs. It is the rootless alternative to the kernel TAP backend: instead
// of opening /dev/net/tun (which needs CAP_NET_ADMIN), it implements ARP,
// DHCPv4, ICMP echo, and IPv4 TCP/UDP NAT entirely in Go and routes traffic
// out through the host's normal sockets.
//
// The MVP scope shipped on the slirp+atomic-disk branch:
//
//   - ARP: respond to who-has(gateway) with the synthetic gateway MAC.
//   - DHCPv4: hand out 10.0.2.15/24 with gw 10.0.2.2 and dns 10.0.2.3 in
//     response to DISCOVER and REQUEST. No lease tracking — the lease is
//     effectively infinite for the lifetime of the VM.
//   - ICMP echo: gateway responds to pings (boot diagnostics).
//   - Outbound UDP NAT (incl. 10.0.2.3:53 DNS forwarding to the host
//     resolver, matching the libslirp/QEMU layout).
//   - Outbound TCP NAT: stubbed. Frames are accepted off the guest TX path
//     but currently return RST. See docs/design/slirp-tcp.md for the plan.
//
// The slirp.Slirp type implements virtio.NetBackend so it drops in wherever
// the TAP backend goes. The RX path is a buffered channel; the TX path is a
// per-frame callback dispatched from the virtio-net transmit goroutine. All
// state is owned by the parent VM lifetime — Close tears down outbound
// sockets, the DHCP table, and ARP cache.
//
// Network plan (matches QEMU -netdev user defaults):
//
//	Subnet:  10.0.2.0/24
//	Guest:   10.0.2.15
//	Gateway: 10.0.2.2 (host)
//	DNS:     10.0.2.3 (host; forwards to host resolver)
//	GW MAC:  52:55:0A:00:02:02 (deterministic; matches QEMU)
package slirp
