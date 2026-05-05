# Design Docs

Living plans for in-flight or deferred work. Each doc states its current
status and links the relevant code. They are not user-facing
documentation — for that, see [`../`](../).

## Index

- [code-disk-attach.md](code-disk-attach.md) — Attach a per-launch code
  disk to a shared rootfs template. Phase 1 (cold-boot CLI/REST) shipped
  on `feat/slirp-net-and-atomic-disk-meta`; snapshot+attach is Phase 2.
- [sandbox-speed-backlog.md](sandbox-speed-backlog.md) — Captured
  follow-up wins from the multi-agent TTI analysis (mem.bin mmap,
  toolbox conn pooling, initrd cache key under burst, OCI parallel
  extract, etc.).
- [slirp-tcp-udp.md](slirp-tcp-udp.md) — Outbound TCP/UDP NAT for the
  userspace network stack. ARP+DHCP+ICMP-echo MVP shipped on
  `feat/slirp-net-and-atomic-disk-meta`; TCP/UDP is the next chunk.
- [whp-backend.md](whp-backend.md) — Windows Hypervisor Platform backend.
  **Deferred**, planned only.
- [hvf-backend.md](hvf-backend.md) — macOS Hypervisor.framework backend
  (Apple Silicon focus). **Deferred**, planned only.
