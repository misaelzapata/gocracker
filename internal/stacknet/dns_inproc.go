// dns_inproc.go — minimal in-process DNS resolver used by Compose mode on
// hosts that don't get a netns + dnsmasq stack.  Resolves names of the form
// <svc>.<project>.compose.local against a small in-memory map and returns
// NXDOMAIN for anything else.  No upstream forwarding (out of scope).
package stacknet

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// dnsTLD is the suffix appended to <svc>.<project> names served by the
// in-process resolver.  Compose-mode clients (containers/VMs) are configured
// with this domain in their /etc/resolv.conf.
const dnsTLD = "compose.local"

// InProcDNS is a tiny UDP DNS resolver that answers A-record queries for
// <svc>.<project>.compose.local from an in-memory map.  Anything else gets
// NXDOMAIN (rcode 3).  No external dependencies, no goroutine fan-out — a
// single Run goroutine handles all queries.
type InProcDNS struct {
	mu      sync.RWMutex
	project string
	records map[string]net.IP // canonical lowercase FQDN -> IPv4

	conn net.PacketConn
	stop chan struct{}
	done chan struct{}
}

// NewInProcDNS opens a UDP listener on listenAddr:0 (OS-assigned port).  The
// chosen address is available via Addr().  The caller is responsible for
// invoking Run to start serving and Close to tear down.
func NewInProcDNS(project string, listenAddr net.IP) (*InProcDNS, error) {
	if project == "" {
		return nil, errors.New("stacknet: dns project is empty")
	}
	if listenAddr == nil {
		listenAddr = net.IPv4(127, 0, 0, 1)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: listenAddr, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("stacknet: dns listen on %s: %w", listenAddr, err)
	}
	return &InProcDNS{
		project: strings.ToLower(project),
		records: map[string]net.IP{},
		conn:    conn,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}, nil
}

// Add registers `svc` -> `ip` for this project.  Subsequent lookups of
// <svc>.<project>.compose.local will resolve to ip.  ip must be IPv4.
func (d *InProcDNS) Add(svc string, ip net.IP) {
	if ip = ip.To4(); ip == nil {
		return
	}
	fqdn := fmt.Sprintf("%s.%s.%s", strings.ToLower(svc), d.project, dnsTLD)
	d.mu.Lock()
	d.records[fqdn] = append(net.IP(nil), ip...)
	d.mu.Unlock()
}

// Remove drops the record for `svc`.  No-op if absent.
func (d *InProcDNS) Remove(svc string) {
	fqdn := fmt.Sprintf("%s.%s.%s", strings.ToLower(svc), d.project, dnsTLD)
	d.mu.Lock()
	delete(d.records, fqdn)
	d.mu.Unlock()
}

// Addr returns the actual listen address (host:port) — useful for tests that
// asked for port 0.
func (d *InProcDNS) Addr() net.Addr {
	return d.conn.LocalAddr()
}

// Run blocks serving queries until ctx is cancelled or Close is called.
// Returns nil on clean shutdown, or the underlying read error otherwise.
func (d *InProcDNS) Run(ctx context.Context) error {
	defer close(d.done)

	// Cancel the read on stop/ctx.
	go func() {
		select {
		case <-ctx.Done():
		case <-d.stop:
		}
		_ = d.conn.SetReadDeadline(timeInPast())
	}()

	buf := make([]byte, 512)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.stop:
			return nil
		default:
		}
		n, src, err := d.conn.ReadFrom(buf)
		if err != nil {
			if isClosedConnErr(err) {
				return nil
			}
			if isTimeoutErr(err) {
				// Reset deadline only if not stopping.
				select {
				case <-ctx.Done():
					return nil
				case <-d.stop:
					return nil
				default:
				}
				_ = d.conn.SetReadDeadline(time.Time{})
				continue
			}
			return err
		}
		resp, ok := d.handle(buf[:n])
		if !ok {
			continue
		}
		_, _ = d.conn.WriteTo(resp, src)
	}
}

// handle parses a single DNS query and produces a response.  Returns
// (response, true) on success, (nil, false) if the packet was unparseable
// (RFC says drop, don't respond, to avoid amplification reflection).
func (d *InProcDNS) handle(pkt []byte) ([]byte, bool) {
	if len(pkt) < 12 {
		return nil, false
	}
	id := binary.BigEndian.Uint16(pkt[0:2])
	qdcount := binary.BigEndian.Uint16(pkt[4:6])
	if qdcount != 1 {
		// We only handle single-question queries.  Real resolvers
		// don't issue multi-question queries in practice.
		return nil, false
	}

	name, qtype, qclass, end, err := parseQuestion(pkt[12:])
	if err != nil {
		return nil, false
	}
	fqdn := strings.ToLower(strings.TrimSuffix(name, "."))

	d.mu.RLock()
	ip, found := d.records[fqdn]
	d.mu.RUnlock()

	// Build response.
	// Header (12 bytes), then echo the question, then optionally one answer
	// RR.  Flags: QR=1, RD=copied, RA=0, RCODE=0 or 3.
	resp := make([]byte, 0, 512)
	resp = appendUint16(resp, id)
	flagsHigh := byte(0x80) // QR=1
	flagsLow := byte(0)
	// preserve RD bit from request
	if pkt[2]&0x01 != 0 {
		flagsHigh |= 0x01
	}
	// Decide rcode + AA.  NXDOMAIN only when the name itself is unknown.
	// For known names with unsupported types we return NOERROR + 0 answers
	// (NODATA), which matches what real authoritative servers do.
	hasA := found && qtype == 1 && qclass == 1
	if found {
		flagsHigh |= 0x04 // AA=1 for our zone
	} else {
		flagsLow |= 0x03 // NXDOMAIN
	}
	resp = append(resp, flagsHigh, flagsLow)

	// QDCOUNT=1.
	resp = appendUint16(resp, 1)
	// ANCOUNT.
	if hasA {
		resp = appendUint16(resp, 1)
	} else {
		resp = appendUint16(resp, 0)
	}
	// NSCOUNT, ARCOUNT.
	resp = appendUint16(resp, 0)
	resp = appendUint16(resp, 0)

	// Echo question (raw bytes from request, up to end of question section).
	resp = append(resp, pkt[12:12+end]...)

	// Answer section.
	if hasA {
		// Use a pointer back to the question's name to save bytes.
		resp = append(resp, 0xc0, 0x0c) // pointer to offset 12 (start of question)
		resp = appendUint16(resp, 1)    // TYPE=A
		resp = appendUint16(resp, 1)    // CLASS=IN
		resp = appendUint32(resp, 30)   // TTL=30s
		resp = appendUint16(resp, 4)    // RDLENGTH
		resp = append(resp, ip[0], ip[1], ip[2], ip[3])
	}
	return resp, true
}

// Close stops the server and unblocks Run.  Safe to call multiple times.
func (d *InProcDNS) Close() error {
	select {
	case <-d.stop:
		return nil
	default:
		close(d.stop)
	}
	_ = d.conn.Close()
	<-d.done
	return nil
}

// --- minimal DNS parser ---------------------------------------------------

// parseQuestion decodes one Question section.  Returns the FQDN (no trailing
// dot in caller's normalisation), QTYPE, QCLASS, and the number of bytes
// consumed.
func parseQuestion(b []byte) (name string, qtype, qclass uint16, consumed int, err error) {
	var labels []string
	i := 0
	for {
		if i >= len(b) {
			return "", 0, 0, 0, errors.New("dns: truncated name")
		}
		l := int(b[i])
		i++
		if l == 0 {
			break
		}
		if l&0xc0 != 0 {
			// Compression pointers in a question are not legal per
			// RFC 1035 §4.1.4 but we tolerate them defensively.
			return "", 0, 0, 0, errors.New("dns: compression in question")
		}
		if l > 63 || i+l > len(b) {
			return "", 0, 0, 0, errors.New("dns: bad label length")
		}
		labels = append(labels, string(b[i:i+l]))
		i += l
	}
	if i+4 > len(b) {
		return "", 0, 0, 0, errors.New("dns: truncated qtype/qclass")
	}
	qtype = binary.BigEndian.Uint16(b[i : i+2])
	qclass = binary.BigEndian.Uint16(b[i+2 : i+4])
	i += 4
	return strings.Join(labels, "."), qtype, qclass, i, nil
}

// --- tiny helpers ---------------------------------------------------------

func appendUint16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendUint32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func isClosedConnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "use of closed") ||
		errors.Is(err, net.ErrClosed)
}

func isTimeoutErr(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

// timeInPast returns a fixed time in the past, used to unblock blocking
// reads on the listening UDP socket.
func timeInPast() time.Time {
	return time.Unix(1, 0)
}
