// Preview integration for sandboxd (Fase 7 slice 3). Wires the
// sandboxes/internal/preview Token + Proxy into the HTTP control
// plane:
//
//   POST /sandboxes/{id}/preview/{port}   — mint a signed URL
//   GET  /previews/{token}/*              — proxy the request into
//                                             the guest's port
//
// Subdomain routing (<id>--<port>.sbx.localhost) lands in slice 4.
package sandboxd

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gocracker/gocracker/sandboxes/internal/preview"
)

// MintPreviewResponse is what POST /sandboxes/{id}/preview/{port}
// returns. Clients can either:
//   - follow URL directly (token in path)
//   - set Cookie: sbx_t=<token> and use the subdomain form
//     (<id>--<port>.sbx.localhost) — slice 4 wires that
type MintPreviewResponse struct {
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	Subdomain string    `json:"subdomain"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ErrPreviewDisabled is returned when the HTTP handler is hit but
// no signer was configured on the Manager. Happens when the
// operator explicitly disabled preview (no key env / flag set).
var ErrPreviewDisabled = errors.New("sandboxd: preview disabled (no signer key configured)")

// previewManager lazily constructs the Signer + Proxy + binds them
// to the sandboxd.Store's UDS resolver. Lazily because most Managers
// don't use preview at all (cold-only deployments, pool-only, tests).
type previewManager struct {
	signer      *preview.Signer
	proxy       *preview.Proxy
	defaultTTL  time.Duration
	previewHost string // "sbx.localhost" by default; drives subdomain URL shape
}

// UDSPathForSandbox satisfies preview.Resolver by looking up the
// Store. Returns false when the sandbox is unknown OR has no UDS
// path (shouldn't happen for leased / cold-booted sandboxes but is
// defensive).
func (m *Manager) UDSPathForSandbox(id string) (string, bool) {
	sb, ok := m.Store.Get(id)
	if !ok {
		return "", false
	}
	if sb.UDSPath == "" {
		return "", false
	}
	return sb.UDSPath, true
}

// ensurePreviewManager initializes on first use. Returns
// ErrPreviewDisabled when no PreviewSigningKey is set — HTTP
// handlers downgrade that to 501.
func (m *Manager) ensurePreviewManager() (*previewManager, error) {
	var err error
	m.previewInit.Do(func() {
		key := m.PreviewSigningKey
		if len(key) == 0 {
			// Auto-generate a per-process random key. Tokens expire
			// at restart — fine for dev; production should set the
			// key explicitly via env / flag for token persistence.
			key = make([]byte, 32)
			if _, rerr := rand.Read(key); rerr != nil {
				err = fmt.Errorf("sandboxd: generate preview key: %w", rerr)
				return
			}
		}
		signer, serr := preview.NewSigner(key)
		if serr != nil {
			err = fmt.Errorf("sandboxd: new preview signer: %w", serr)
			return
		}
		ttl := m.PreviewTTL
		if ttl == 0 {
			ttl = 1 * time.Hour
		}
		host := m.PreviewHost
		if host == "" {
			host = "sbx.localhost"
		}
		m.previewMgr = &previewManager{
			signer:      signer,
			proxy:       &preview.Proxy{Resolver: m, DialTimeout: 5 * time.Second, IdleTimeout: 0},
			defaultTTL:  ttl,
			previewHost: host,
		}
	})
	return m.previewMgr, err
}

// MintPreview generates a signed token for (sandboxID, port) with
// the configured TTL. Returns ErrSandboxNotFound if the sandbox
// isn't in the store.
func (m *Manager) MintPreview(id string, port uint16) (MintPreviewResponse, error) {
	if _, ok := m.Store.Get(id); !ok {
		return MintPreviewResponse{}, ErrSandboxNotFound
	}
	pm, err := m.ensurePreviewManager()
	if err != nil {
		return MintPreviewResponse{}, err
	}
	expires := time.Now().Add(pm.defaultTTL)
	tok := pm.signer.Sign(preview.TokenPayload{SandboxID: id, Port: port, ExpiresAt: expires})
	return MintPreviewResponse{
		Token:     tok,
		URL:       "/previews/" + tok + "/",
		// Subdomain is just <sandbox-id>.<root>. The target port lives
		// in the signed token; a single subdomain can front multiple
		// ports if the caller mints separate tokens for each.
		Subdomain: fmt.Sprintf("%s.%s", id, pm.previewHost),
		ExpiresAt: expires,
	}, nil
}

// ServePreview is the GET /previews/{token}/* handler. Verifies
// the token, rewrites the request path (stripping the /previews/
// prefix), and hands off to the proxy.
func (m *Manager) ServePreview(w http.ResponseWriter, r *http.Request) {
	pm, err := m.ensurePreviewManager()
	if err != nil {
		http.Error(w, ErrPreviewDisabled.Error(), http.StatusNotImplemented)
		return
	}
	// r.URL.Path is "/previews/{token}/..." — strip the prefix.
	rest := strings.TrimPrefix(r.URL.Path, "/previews/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	token := parts[0]
	subPath := "/"
	if len(parts) == 2 {
		subPath = "/" + parts[1]
	}

	payload, err := pm.signer.Verify(token)
	if err != nil {
		// ErrInvalidToken is intentionally indistinguishable — don't
		// leak the reason in the response (it could help enumerate
		// valid sandbox IDs).
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	// Rewrite the URL so the proxy + guest see the sub-path only.
	r2 := r.Clone(r.Context())
	r2.URL.Path = subPath
	if idx := strings.IndexByte(subPath, '?'); idx >= 0 {
		r2.URL.RawQuery = subPath[idx+1:]
	}
	_ = pm.proxy.ServeRequest(w, r2, payload.SandboxID, payload.Port)
}

// ServePreviewHost is the subdomain entry point. Host shape is
// <sandbox-id>.<previewHost>; the port lives in the signed token,
// not the DNS label, so end-user URLs stay short (<id>.sbx.localhost
// vs <id>--<port>.sbx.localhost). One token = one (sandbox, port)
// pair.
//
// First hit carries ?token=<tok> → verify → Set-Cookie: sbx_t →
// 303 redirect to same URL sans ?token. Subsequent hits use the
// cookie.
//
// Rejections:
//   - host doesn't parse → 400
//   - no cookie and no ?token → 401
//   - invalid/expired token or cookie → 401 (indistinguishable)
//   - token's sandbox_id mismatches the subdomain → 403
func (m *Manager) ServePreviewHost(w http.ResponseWriter, r *http.Request) {
	pm, err := m.ensurePreviewManager()
	if err != nil {
		http.Error(w, ErrPreviewDisabled.Error(), http.StatusNotImplemented)
		return
	}
	id, ok := parsePreviewHost(r.Host, pm.previewHost)
	if !ok {
		http.Error(w, "invalid preview host", http.StatusBadRequest)
		return
	}

	// Try ?token=... first (first-hit flow).
	if qtok := r.URL.Query().Get("token"); qtok != "" {
		payload, verr := pm.signer.Verify(qtok)
		if verr != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		if payload.SandboxID != id {
			http.Error(w, "token mismatches subdomain", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     previewCookieName,
			Value:    qtok,
			Path:     "/",
			Expires:  payload.ExpiresAt,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		clean := *r.URL
		q := clean.Query()
		q.Del("token")
		clean.RawQuery = q.Encode()
		http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
		return
	}

	cookie, err := r.Cookie(previewCookieName)
	if err != nil || cookie.Value == "" {
		http.Error(w, "missing preview credentials", http.StatusUnauthorized)
		return
	}
	payload, verr := pm.signer.Verify(cookie.Value)
	if verr != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	if payload.SandboxID != id {
		http.Error(w, "cookie mismatches subdomain", http.StatusForbidden)
		return
	}
	_ = pm.proxy.ServeRequest(w, r, payload.SandboxID, payload.Port)
}

// IsPreviewHost reports whether r.Host matches the preview-host
// shape (<id>.<previewHost>). Used by the top-level mux to route
// to ServePreviewHost vs the normal control-plane.
func (m *Manager) IsPreviewHost(host string) bool {
	pm, err := m.ensurePreviewManager()
	if err != nil || pm == nil {
		return false
	}
	_, ok := parsePreviewHost(host, pm.previewHost)
	return ok
}

// parsePreviewHost validates the Host header shape and extracts the
// sandbox id. Expected shape: "<sandbox-id>.<root>" (e.g.
// "sb-abc123.sbx.localhost"). The target port lives in the signed
// token, not the hostname — keeps URLs short and lets one subdomain
// front any number of ports the user has minted tokens for.
//
// Ignores any ":port" suffix on the Host header (that's the
// client→sandboxd port, not the guest's port).
func parsePreviewHost(host, root string) (string, bool) {
	if idx := strings.LastIndexByte(host, ':'); idx > 0 {
		host = host[:idx]
	}
	if !strings.HasSuffix(host, "."+root) {
		return "", false
	}
	id := strings.TrimSuffix(host, "."+root)
	if id == "" {
		return "", false
	}
	// Reject nested subdomains (e.g. "foo.bar.sbx.localhost") — we
	// only accept one label in front of the root.
	if strings.Contains(id, ".") {
		return "", false
	}
	return id, true
}

const previewCookieName = "sbx_t"

// parsePreviewPort parses the {port} path value from
// /sandboxes/{id}/preview/{port}. Invalid → ErrInvalidRequest.
func parsePreviewPort(raw string) (uint16, error) {
	n, err := strconv.ParseUint(raw, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%w: port %q: %v", ErrInvalidRequest, raw, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("%w: port 0 reserved", ErrInvalidRequest)
	}
	return uint16(n), nil
}

// --- Manager plumbing (Manager struct extension) ---

// previewInit / previewMgr live on Manager via the same lazy pattern
// as poolInit / tmplInit. Adding them via a separate fieldExt struct
// lets slice 3 land without touching Manager's field list — kept as
// a field on Manager directly, defined in sandbox.go.

// The following are set by sandboxd's main(). Empty defaults apply.
//
//   PreviewSigningKey []byte
//   PreviewTTL        time.Duration
//   PreviewHost       string // subdomain root; default sbx.localhost
//
// Defined inline on Manager so zero-value init works without bumping
// NewManager (there is no constructor — ensurePreviewManager handles
// defaults).

var _ = sync.Once{}
