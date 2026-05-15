//go:build !slirp_gvisor

package slirp

import "sync/atomic"

// tcpStack is a placeholder in the stub build so the Slirp.tcp field
// type is consistent across build tags. The gVisor build replaces this
// with a struct that owns the stack and channel endpoint.
type tcpStack struct{}

// tcpDropped is the package-level counter for the default (non-gVisor)
// build. It mirrors s.stats.TCPDropped so the metric remains observable
// via TCPMetrics() regardless of which TCP backend is compiled in.
var tcpDropped atomic.Uint64

// handleTCP is the default TCP frame handler. Without the slirp_gvisor
// build tag we have no usermode TCP stack to inject into, so every TCP
// frame is counted and dropped. Guests see RST-equivalent silence and
// either fall back to UDP or retry.
//
// Returns (replyFrame, ok). ok is false because there's never a reply.
func (s *Slirp) handleTCP(_ []byte) ([]byte, bool) {
	tcpDropped.Add(1)
	s.stats.TCPDropped.Add(1)
	return nil, false
}

// tcpInit is called from Slirp.New() to set up any per-instance TCP
// state. The stub has none.
func (s *Slirp) tcpInit() {}

// tcpClose tears down any per-instance TCP state. The stub has none.
func (s *Slirp) tcpClose() {}

// TCPMetrics returns the lifetime count of TCP frames the stub has
// dropped. With the slirp_gvisor build tag this is replaced by a real
// counter of frames injected into the gVisor stack.
func TCPMetrics() uint64 { return tcpDropped.Load() }
