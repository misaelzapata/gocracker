package slirp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
)

// PortForward describes a single host→guest port-forward rule. It is
// the public shape callers use to register forwardings; the slirp
// engine handles SYN redirection (TCP) and datagram forwarding (UDP)
// based on the table.
//
// HostIP is the address the slirp host listener binds to (typically
// 127.0.0.1 for local-only). HostPort is the listening port. GuestIP /
// GuestPort identify the in-guest endpoint that should receive the
// forwarded connection (usually 10.0.2.15 + service port).
//
// Proto is the lowercase string "tcp" or "udp". Anything else returns
// an error from Add.
type PortForward struct {
	HostIP    net.IP
	GuestIP   net.IP
	HostPort  uint16
	GuestPort uint16
	Proto     string
}

// PortFwdRegistry is the thread-safe table of registered forwards. It
// is owned by the parent Slirp instance and accessed from both the TCP
// dispatcher (lookup-only) and operator code (Add / Listen).
type PortFwdRegistry struct {
	mu   sync.RWMutex
	fwds []PortForward

	// listeners tracks net.Listener instances created by Listen so
	// shutdown can close them deterministically. Map key is the
	// (proto, hostIP, hostPort) tuple stringified.
	listeners map[string]net.Listener
}

// NewPortFwdRegistry returns an empty registry. The zero value of
// PortFwdRegistry is also usable; New is provided for symmetry with the
// rest of the slirp package.
func NewPortFwdRegistry() *PortFwdRegistry {
	return &PortFwdRegistry{
		listeners: make(map[string]net.Listener),
	}
}

// ErrInvalidProto is returned by Add when Proto is not "tcp" or "udp".
var ErrInvalidProto = errors.New("slirp: portfwd proto must be tcp or udp")

// ErrDuplicateForward is returned when a (proto, hostIP, hostPort)
// triple is already registered.
var ErrDuplicateForward = errors.New("slirp: portfwd already registered")

// Add inserts a forward rule. Returns ErrInvalidProto if the protocol
// isn't recognised and ErrDuplicateForward if the host endpoint is
// already taken.
func (r *PortFwdRegistry) Add(pf PortForward) error {
	proto := strings.ToLower(pf.Proto)
	if proto != "tcp" && proto != "udp" {
		return ErrInvalidProto
	}
	pf.Proto = proto
	if pf.HostIP == nil {
		pf.HostIP = net.IPv4(127, 0, 0, 1).To4()
	} else {
		pf.HostIP = pf.HostIP.To4()
	}
	if pf.GuestIP == nil {
		pf.GuestIP = guestIP
	} else {
		pf.GuestIP = pf.GuestIP.To4()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.fwds {
		if existing.Proto == pf.Proto &&
			existing.HostIP.Equal(pf.HostIP) &&
			existing.HostPort == pf.HostPort {
			return ErrDuplicateForward
		}
	}
	if r.listeners == nil {
		r.listeners = make(map[string]net.Listener)
	}
	r.fwds = append(r.fwds, pf)
	return nil
}

// Lookup resolves a forward rule by (proto, ip, port). The ip can be
// either the host-side bind address or any address (some callers don't
// have the bind IP handy and just want to know if port is forwarded).
// Returns the matched rule and true on hit, zero-value + false on miss.
func (r *PortFwdRegistry) Lookup(proto string, ip net.IP, port uint16) (PortForward, bool) {
	proto = strings.ToLower(proto)
	ip4 := ip.To4()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, pf := range r.fwds {
		if pf.Proto != proto || pf.HostPort != port {
			continue
		}
		if ip4 == nil || pf.HostIP.Equal(ip4) {
			return pf, true
		}
	}
	return PortForward{}, false
}

// All returns a snapshot of the registered forwards. Safe to call from
// any goroutine; the returned slice is a copy.
func (r *PortFwdRegistry) All() []PortForward {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]PortForward, len(r.fwds))
	copy(out, r.fwds)
	return out
}

// Listen starts a goroutine per TCP forward rule that accepts inbound
// host connections and invokes onAccept with the matched rule and the
// accepted conn. The caller is responsible for proxying onto the guest
// (typically by dialing the gVisor stack's TCP endpoint).
//
// UDP rules are ignored here — UDP forwarding is per-datagram and lives
// in udp_nat.go.
//
// Listen returns when ctx is cancelled. All listeners are closed before
// it returns.
func (r *PortFwdRegistry) Listen(ctx context.Context, onAccept func(PortForward, net.Conn)) error {
	r.mu.Lock()
	snapshot := make([]PortForward, len(r.fwds))
	copy(snapshot, r.fwds)
	r.mu.Unlock()

	var wg sync.WaitGroup
	for _, pf := range snapshot {
		if pf.Proto != "tcp" {
			continue
		}
		addr := net.JoinHostPort(pf.HostIP.String(), fmt.Sprintf("%d", pf.HostPort))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			// Capturing the error here would short-circuit the
			// loop and leak earlier listeners. Soft-fail: log via
			// the caller's onAccept(nil, nil) — but since onAccept
			// doesn't carry an error channel we return early after
			// closing whatever was opened.
			r.closeAll()
			return fmt.Errorf("slirp: listen %s/%s: %w", pf.Proto, addr, err)
		}
		key := pf.Proto + "|" + addr
		r.mu.Lock()
		r.listeners[key] = ln
		r.mu.Unlock()

		wg.Add(1)
		go func(pf PortForward, ln net.Listener) {
			defer wg.Done()
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				// Run onAccept on its own goroutine so a slow
				// proxy doesn't block subsequent accepts.
				go onAccept(pf, conn)
			}
		}(pf, ln)
	}

	// Close listeners on cancellation; the Accept goroutines will then
	// observe net.ErrClosed and return.
	<-ctx.Done()
	r.closeAll()
	wg.Wait()
	return nil
}

// closeAll shuts every active listener and clears the listener map.
func (r *PortFwdRegistry) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, ln := range r.listeners {
		_ = ln.Close()
		delete(r.listeners, k)
	}
}
