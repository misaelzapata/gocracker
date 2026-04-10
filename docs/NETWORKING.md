# Networking

gocracker provides three networking modes for guest VMs: no network, automatic
NAT, and manual TAP. Compose stacks use a fourth mode with bridged namespaces.

## Overview

| Mode | Flag | Use case |
|------|------|----------|
| None (default) | `--net none` | Isolated workloads, no network needed |
| Automatic NAT | `--net auto` | Single VMs that need internet access |
| Manual TAP | `--tap tap0` | Full control, custom topologies |
| Compose bridge | (automatic) | Multi-VM stacks with service DNS |

## Automatic Networking (`--net auto`)

When you pass `--net auto`, gocracker handles all host-side networking:

```bash
sudo ./gocracker run \
  --image alpine:3.20 \
  --kernel ./kernel \
  --net auto \
  --wait
```

### What happens

1. **Subnet selection** -- A /30 subnet is picked from the `198.18.0.0/15`
   pool. The choice is deterministic (FNV-1a hash of the project name) and
   avoids subnets already in use on the host.

2. **TAP creation** -- A TAP device is created with a name derived from the
   project (e.g. `gct-default`), capped at 15 characters (Linux IFNAMSIZ).

3. **IP assignment** -- The first host IP in the subnet becomes the gateway
   on the TAP device. The next IP is assigned to the guest.

4. **IP forwarding** -- `/proc/sys/net/ipv4/ip_forward` is set to `1`. The
   previous value is restored on cleanup.

5. **Upstream detection** -- The host's default IPv4 route interface is
   discovered by probing routes to `1.1.1.1` and `8.8.8.8`, falling back to
   the lowest-priority default route.

6. **iptables rules** -- Three rules are added:
   - `POSTROUTING -t nat ... -j MASQUERADE` (guest source NAT)
   - `FORWARD -i <tap> -o <upstream> -j ACCEPT` (outbound)
   - `FORWARD -i <upstream> -o <tap> -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT` (return traffic)

7. **Cleanup** -- On VM stop, all iptables rules are removed and ip_forward
   is restored to its original value.

gocracker prefers `iptables-nft` and falls back to legacy `iptables`.

### Diagram: Auto Mode

```
+-----------+       +--------+       +----------+       +----------+
|  Guest    |       | TAP    |       | iptables |       | Upstream |
|  (virtio- | ----> | device | ----> |   NAT    | ----> | (eth0 /  |
|   net)    |       |        |       | MASQ     |       |  wlan0)  |
+-----------+       +--------+       +----------+       +----------+
  198.18.x.2/30    198.18.x.1/30                         internet
```

## Compose Networking

When running `gocracker compose`, each stack gets an isolated network namespace
with a bridge. Services communicate by name.

### How it works

1. **Network namespace** -- A new netns named `gcns-<project>` is created.

2. **Bridge** -- A bridge (`gcbr-<project>`) is created inside the namespace.

3. **Veth pair** -- A veth pair connects the host to the namespace:
   - Host side: `gch-<project>` with the gateway IP
   - Namespace side: `gcb-<project>` attached to the bridge

4. **Subnet** -- A /24 is selected from the `198.18.0.0/15` pool (same
   FNV-1a hashing, but with /24 prefix for more addresses).

5. **TAP attachment** -- Each service's TAP device is moved into the namespace
   and attached to the bridge.

6. **Service DNS** -- The guest init resolves service names via `/etc/hosts`
   entries injected at boot time (host aliases mapping service names to IPs).

7. **Port publishing** -- Published ports (`ports: "8080:80"`) are forwarded
   from the host via a userspace TCP/UDP proxy that listens on the host and
   dials the service IP inside the namespace.

### Diagram: Compose Mode

```
                    Host
    +-------------------------------------+
    |  gch-myapp (veth)   198.18.x.1/24   |
    +-----------|-------------------------+
                | (veth pair)
    +-----------v-------------------------+
    |  Network Namespace: gcns-myapp       |
    |                                      |
    |  gcb-myapp (veth) -- gcbr-myapp (br) |
    |                       |       |      |
    |                  gct-app  gct-db     |
    |                    TAP      TAP      |
    +--------------------------------------+
                       |          |
                  +----v---+ +---v----+
                  |  App   | | Postgres|
                  |  VM    | |   VM   |
                  +--------+ +--------+
                198.18.x.3   198.18.x.2
```

Services resolve each other by name. The app VM can reach `postgres:5432`
because `postgres` maps to `198.18.x.2` in `/etc/hosts`.

## Manual TAP

For full control, create a TAP device yourself and pass it with `--tap`:

```bash
# Create and configure the TAP
sudo ip tuntap add dev tap0 mode tap
sudo ip addr add 192.168.100.1/30 dev tap0
sudo ip link set dev tap0 up

# Enable forwarding and NAT (if internet access is needed)
sudo sysctl -w net.ipv4.ip_forward=1
sudo iptables -t nat -A POSTROUTING -o eth0 -s 192.168.100.0/30 -j MASQUERADE

# Run the VM
sudo ./gocracker run \
  --image alpine:3.20 \
  --kernel ./kernel \
  --tap tap0 \
  --wait
```

The guest will receive its IP via the init process (DHCP client or static
configuration passed through the kernel command line).

## Guest-Side Networking

The guest init binary (`internal/guest/init.go`) configures networking at
boot:

1. Mounts `/proc`, `/sys`, and device filesystems.
2. Reads the network configuration from the kernel command line or
   runtime config embedded in the initrd.
3. Brings up `eth0` with the assigned IP and default gateway.
4. Writes `/etc/resolv.conf` with the gateway as DNS server (or host DNS).
5. Injects `/etc/hosts` entries for compose service names.

## Troubleshooting

### "ip_forward: permission denied"

gocracker needs root to write `/proc/sys/net/ipv4/ip_forward`. Run with
`sudo` or set it manually:

```bash
sudo sysctl -w net.ipv4.ip_forward=1
```

### "iptables-nft or iptables not found on host"

Install iptables:

```bash
# Debian/Ubuntu
sudo apt install iptables

# Fedora/RHEL
sudo dnf install iptables-nft
```

### "no IPv4 default route found"

Auto networking needs a default route to detect the upstream interface. Check:

```bash
ip route show default
```

If you are on a machine with no internet (e.g. air-gapped), use manual TAP
mode instead.

### "Cannot find device" errors

The TAP device may have been removed or is owned by another process. For auto
mode, gocracker creates and cleans up TAPs automatically. For manual mode,
verify the device exists:

```bash
ip link show tap0
```

### Compose port forwarding not working

Published ports use a userspace proxy (not iptables DNAT). Verify the host
side is listening:

```bash
ss -tlnp | grep <host-port>
```

If nothing is listening, the service VM may not have started yet. Check the
compose output for errors.

### VM has no connectivity

1. Verify the TAP is up: `ip link show <tap-name>`
2. Check iptables rules: `sudo iptables -L FORWARD -v -n`
3. Check NAT: `sudo iptables -t nat -L POSTROUTING -v -n`
4. Ping the gateway from inside the VM (if you have an interactive shell).

---

## More Documentation

- [Getting Started](GETTING_STARTED.md) | [Networking](NETWORKING.md) | [Architecture](ARCHITECTURE.md) | [Compose](COMPOSE.md)
- [API Reference](API.md) | [CLI Reference](CLI_REFERENCE.md) | [Snapshots](SNAPSHOTS.md)
- [Examples](EXAMPLES.md) | [Validated Projects](VALIDATED_PROJECTS.md) | [Troubleshooting](TROUBLESHOOTING.md)
- [How gocracker Fits In](COMPETITIVE_ANALYSIS.md) | [Security Policy](../SECURITY.md)

