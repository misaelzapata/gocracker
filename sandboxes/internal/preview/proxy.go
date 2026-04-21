package preview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Resolver maps a sandbox id to its host-side toolbox UDS path.
// Implemented by sandboxd.Manager (via the Store) — kept as an
// interface here so the preview package doesn't reach back into
// sandboxd (which would create an import cycle with the eventual
// HTTP integration).
type Resolver interface {
	UDSPathForSandbox(id string) (string, bool)
}

// Proxy bridges an HTTP request from the host into a guest port via
// the toolbox UDS's CONNECT handshake. Used by:
//
//   - GET /previews/{token}        — token-authenticated relay
//   - <id>--<port>.sbx.localhost   — subdomain relay (cookie-authed)
//
// The proxy is HTTP-only: it shuttles request bytes verbatim to the
// guest and copies the response back. WebSocket upgrades work
// because we don't parse the response body — the bidirectional copy
// keeps both directions alive until either side closes.
type Proxy struct {
	Resolver Resolver
	// DialTimeout caps the UDS connect + CONNECT handshake.
	// Defaults to 5 s.
	DialTimeout time.Duration
	// IdleTimeout caps a single proxied request's lifetime.
	// Default 30 s. Long-poll / SSE / websocket use cases that
	// outlast this should set a higher value or 0 (unlimited).
	IdleTimeout time.Duration
}

// ServeRequest dials the sandbox UDS, sends "CONNECT <port>\n",
// validates the OK response, then writes the inbound HTTP request
// to the guest and copies the response back to w. Returns the first
// error encountered. The caller is responsible for token / cookie
// authentication BEFORE invoking ServeRequest.
//
// Conn is hijacked via http.Hijacker for upgraded protocols
// (websockets, server-sent events). Falls back to plain
// io.Copy when the response handler doesn't support hijacking
// (e.g. some test recorders).
func (p *Proxy) ServeRequest(w http.ResponseWriter, r *http.Request, sandboxID string, port uint16) error {
	udsPath, ok := p.Resolver.UDSPathForSandbox(sandboxID)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return ErrSandboxNotFound
	}
	dialTO := p.DialTimeout
	if dialTO == 0 {
		dialTO = 5 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(r.Context(), dialTO)
	defer cancel()
	guestConn, err := dialUDSConnect(dialCtx, udsPath, uint32(port))
	if err != nil {
		http.Error(w, "guest unreachable: "+err.Error(), http.StatusBadGateway)
		return err
	}
	defer guestConn.Close()

	if p.IdleTimeout > 0 {
		_ = guestConn.SetDeadline(time.Now().Add(p.IdleTimeout))
	}

	// Rewrite the request URI to the path the guest sees: strip
	// any leading /previews/{token} prefix the caller already
	// consumed. The caller (slice 3 handler) sets r.URL.Path to the
	// sub-path; we forward that as-is.
	if err := writeRequestToGuest(guestConn, r); err != nil {
		http.Error(w, "guest write failed: "+err.Error(), http.StatusBadGateway)
		return err
	}

	// Hijack the inbound conn so we can stream the response in
	// both directions (websocket / SSE / chunked). Fall through to
	// plain copy when hijack isn't supported (httptest.ResponseRecorder
	// in unit tests).
	hj, ok := w.(http.Hijacker)
	if !ok {
		return copyResponseToResponseWriter(w, guestConn)
	}
	clientConn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
		return err
	}
	defer clientConn.Close()
	// Drain anything bufio buffered on the client side into the
	// guest (handles request bodies that arrived after the hijack).
	if buffered := brw.Reader.Buffered(); buffered > 0 {
		b, _ := brw.Reader.Peek(buffered)
		_, _ = guestConn.Write(b)
	}
	return bidiCopy(clientConn, guestConn)
}

// dialUDSConnect dials udsPath and performs the Firecracker-style
// CONNECT handshake to reach guest port. Returns the bridged
// net.Conn (suitable for raw HTTP framing) on success.
func dialUDSConnect(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("dial uds: %w", err)
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("CONNECT %d\n", port))); err != nil {
		conn.Close()
		return nil, fmt.Errorf("CONNECT write: %w", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("CONNECT read: %w", err)
	}
	if !strings.HasPrefix(line, "OK") {
		conn.Close()
		return nil, fmt.Errorf("CONNECT rejected: %s", strings.TrimSpace(line))
	}
	// Wrap to flush any buffered bytes back into reads. Most CONNECT
	// responses are exactly "OK\n" so the buffer is empty here, but
	// be defensive.
	if br.Buffered() > 0 {
		bufBytes, _ := br.Peek(br.Buffered())
		conn = &prefixedConn{Conn: conn, prefix: bufBytes}
	}
	return conn, nil
}

// writeRequestToGuest re-emits the HTTP request on the guest conn.
// Uses Go's stdlib http.Request.Write which is HTTP/1.1 compliant
// and handles Host, Content-Length, transfer encodings, etc.
//
// Strips Hop-by-hop headers per RFC 7230 §6.1. Connection: close is
// added so the guest closes after the response — simpler than
// keep-alive accounting at this layer.
func writeRequestToGuest(w net.Conn, r *http.Request) error {
	r2 := r.Clone(r.Context())
	for _, h := range hopByHopHeaders {
		r2.Header.Del(h)
	}
	r2.Header.Set("Connection", "close")
	r2.Header.Set("X-Forwarded-For", clientIP(r))
	r2.Header.Set("X-Forwarded-Proto", "http")
	if origHost := r.Host; origHost != "" {
		r2.Header.Set("X-Forwarded-Host", origHost)
	}
	// http.Request.Write requires URL.Host to be empty (it would
	// otherwise emit a proxy-style absolute URL).
	r2.URL.Scheme = ""
	r2.URL.Host = ""
	return r2.Write(w)
}

// copyResponseToResponseWriter is the non-hijack fallback. Reads
// the HTTP response from the guest, copies headers + body into the
// inbound ResponseWriter. No bidirectional streaming — fine for
// test recorders, not for websockets.
func copyResponseToResponseWriter(w http.ResponseWriter, guestConn net.Conn) error {
	br := bufio.NewReader(guestConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		http.Error(w, "guest response: "+err.Error(), http.StatusBadGateway)
		return err
	}
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	return err
}

// bidiCopy keeps two streams in sync until either errors / closes.
// Used post-hijack so the client can speak HTTP back to the guest
// (websocket frames, request bodies that race the response, etc.).
func bidiCopy(a, b net.Conn) error {
	errc := make(chan error, 2)
	go func() { _, err := io.Copy(a, b); errc <- err }()
	go func() { _, err := io.Copy(b, a); errc <- err }()
	<-errc
	a.Close()
	b.Close()
	<-errc
	return nil
}

// hopByHopHeaders are the RFC 7230 §6.1 hop-by-hop headers. Stripped
// before forwarding to the guest (and from the guest's response).
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func isHopByHop(k string) bool {
	k = http.CanonicalHeaderKey(k)
	for _, h := range hopByHopHeaders {
		if k == http.CanonicalHeaderKey(h) {
			return true
		}
	}
	return false
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Append, don't replace — preserves the chain.
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host != "" {
			return xff + ", " + host
		}
		return xff
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// prefixedConn glues already-buffered bytes back onto a net.Conn so
// downstream readers see them on the first Read. Only kicks in when
// the CONNECT response had pipelined bytes after the OK\n.
type prefixedConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixedConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// ErrSandboxNotFound is returned by ServeRequest when the resolver
// has no UDS for the requested sandbox id.
var ErrSandboxNotFound = errors.New("preview: sandbox not found")
