// Package gocracker is the Go SDK for gocracker sandboxd + toolbox agent.
//
// Mirrors the Python + JS SDKs: a Client for the HTTP control plane
// and a ToolboxClient for in-guest operations over UDS + CONNECT.
// Stdlib-only (net/http, net, encoding/json) — no third-party
// dependencies beyond what the main module already uses. Drop-in
// usable from any Go project pointing at the gocracker module.
//
// Usage:
//
//	import gocracker "github.com/gocracker/gocracker/sandboxes/sdk/go"
//
//	c := gocracker.NewClient("http://127.0.0.1:9091")
//	sb, err := c.CreateSandbox(ctx, gocracker.CreateSandboxRequest{
//	    Image: "alpine:3.20", KernelPath: "/abs/path",
//	})
//	if err != nil { ... }
//	defer sb.Delete(ctx)
//
//	result, err := sb.Toolbox().Exec(ctx, []string{"echo", "hello"})
//	fmt.Println(string(result.Stdout))
package gocracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---- Typed errors ------------------------------------------------

// Error is the base error type for sandboxd client calls. Status and
// Body carry the raw HTTP response for diagnostic / retry logic.
type Error struct {
	Message string
	Status  int
	Body    string
}

func (e *Error) Error() string { return e.Message }

// ErrSandboxNotFound, ErrPoolNotFound, ErrInvalidRequest, ErrConflict
// are sentinels for errors.Is checks. The concrete return value is
// still *Error so Status / Body are available on unwrap.
var (
	ErrSandboxNotFound  = errors.New("sandboxd: sandbox not found")
	ErrInvalidRequest   = errors.New("sandboxd: invalid request")
	ErrConflict         = errors.New("sandboxd: conflict")
	ErrTemplateNotFound = errors.New("sandboxd: template not found")
	ErrPoolNotFound     = errors.New("sandboxd: pool not found")
)

// ---- Wire types --------------------------------------------------

// CreateSandboxRequest mirrors sandboxd.CreateSandboxRequest with
// JSON tags matching the wire exactly — the SDK is a thin
// typed passthrough, not a reshape.
type CreateSandboxRequest struct {
	Image       string   `json:"image,omitempty"`
	Dockerfile  string   `json:"dockerfile,omitempty"`
	Context     string   `json:"context,omitempty"`
	KernelPath  string   `json:"kernel_path"`
	MemMB       uint64   `json:"mem_mb,omitempty"`
	CPUs        int      `json:"cpus,omitempty"`
	Entrypoint  []string `json:"entrypoint,omitempty"`
	Cmd         []string `json:"cmd,omitempty"`
	Env         []string `json:"env,omitempty"`
	WorkDir     string   `json:"workdir,omitempty"`
	NetworkMode string   `json:"network_mode,omitempty"`
	JailerMode  string   `json:"jailer_mode,omitempty"`

	// Template, when set, names a registered template (e.g. "base-python",
	// "base-node"). The SDK resolves the template's spec client-side and
	// fills in Image/KernelPath/MemMB/CPUs if the caller left them zero.
	// This is the field Daytona-style callers use:
	//
	//	c.CreateSandbox(ctx, gocracker.CreateSandboxRequest{Template: "base-python"})
	//
	// Not sent on the wire — the wire shape is the one sandboxd's
	// CreateSandboxRequest expects (no template field). Resolution
	// happens in Client.CreateSandbox before POSTing.
	Template string `json:"-"`
}

// Sandbox mirrors sandboxd.Sandbox.
type Sandbox struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Image     string    `json:"image"`
	UDSPath   string    `json:"uds_path,omitempty"`
	GuestIP   string    `json:"guest_ip,omitempty"`
	RuntimeID string    `json:"runtime_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ErrorMsg  string    `json:"error,omitempty"`

	client *Client `json:"-"`
}

// Delete tears down the sandbox via the owning Client.
func (s *Sandbox) Delete(ctx context.Context) error {
	if s.client == nil {
		return fmt.Errorf("sandbox has no client; call client.Delete(id) instead")
	}
	return s.client.Delete(ctx, s.ID)
}

// Recycle tears down this leased sandbox and returns a fresh one from
// the same pool. The old handle (`s`) is dead after this call — use
// the returned *Sandbox. Only works for lease-origin sandboxes.
func (s *Sandbox) Recycle(ctx context.Context) (*Sandbox, error) {
	if s.client == nil {
		return nil, fmt.Errorf("sandbox has no client; call client.Recycle(ctx, id) instead")
	}
	return s.client.Recycle(ctx, s.ID)
}

// Toolbox returns a ToolboxClient bound to this sandbox's UDS.
func (s *Sandbox) Toolbox() *ToolboxClient {
	if s.UDSPath == "" {
		return nil
	}
	return NewToolboxClient(s.UDSPath)
}

// CreateSandboxResponse wraps the sandbox pointer in the same shape
// sandboxd uses on the wire.
type CreateSandboxResponse struct {
	Sandbox Sandbox `json:"sandbox"`
}

// Pool / Template / Preview shapes mirror sandboxd. JSON names
// match the Go server's struct tags exactly.

type CreatePoolRequest struct {
	TemplateID           string `json:"template_id"`
	FromTemplate         string `json:"from_template,omitempty"`
	Image                string `json:"image,omitempty"`
	Dockerfile           string `json:"dockerfile,omitempty"`
	Context              string `json:"context,omitempty"`
	KernelPath           string `json:"kernel_path,omitempty"`
	MemMB                uint64 `json:"mem_mb,omitempty"`
	CPUs                 int    `json:"cpus,omitempty"`
	JailerMode           string `json:"jailer_mode,omitempty"`
	MinPaused            int    `json:"min_paused,omitempty"`
	MaxPaused            int    `json:"max_paused,omitempty"`
	ReplenishParallelism int    `json:"replenish_parallelism,omitempty"`
}

type Pool struct {
	TemplateID string         `json:"template_id"`
	Image      string         `json:"image"`
	KernelPath string         `json:"kernel_path"`
	MemMB      uint64         `json:"mem_mb,omitempty"`
	CPUs       int            `json:"cpus,omitempty"`
	JailerMode string         `json:"jailer_mode,omitempty"`
	MinPaused  int            `json:"min_paused,omitempty"`
	MaxPaused  int            `json:"max_paused,omitempty"`
	Counts     map[string]int `json:"counts,omitempty"`
}

type LeaseSandboxRequest struct {
	TemplateID string        `json:"template_id"`
	Timeout    time.Duration `json:"timeout,omitempty"`
}

type CreateTemplateRequest struct {
	ID         string   `json:"id,omitempty"`
	Image      string   `json:"image,omitempty"`
	Dockerfile string   `json:"dockerfile,omitempty"`
	Context    string   `json:"context,omitempty"`
	KernelPath string   `json:"kernel_path"`
	MemMB      uint64   `json:"mem_mb,omitempty"`
	CPUs       int      `json:"cpus,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	Env        []string `json:"env,omitempty"`
	WorkDir    string   `json:"workdir,omitempty"`
}

type Template struct {
	ID          string         `json:"id"`
	SpecHash    string         `json:"spec_hash"`
	State       string         `json:"state"`
	SnapshotDir string         `json:"snapshot_dir,omitempty"`
	LastError   string         `json:"last_error,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Spec        map[string]any `json:"spec,omitempty"`
}

type CreateTemplateResponse struct {
	Template Template `json:"template"`
	CacheHit bool     `json:"cache_hit"`
}

type Preview struct {
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	Subdomain string    `json:"subdomain"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ---- Client ------------------------------------------------------

// Client is the sandboxd HTTP client. Safe for concurrent use.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient builds a Client with a default 30-second HTTP timeout.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Sandbox lifecycle --------------------------------------------------

func (c *Client) CreateSandbox(ctx context.Context, req CreateSandboxRequest) (*Sandbox, error) {
	// Daytona-style template resolution: look up the named template
	// and fill in the spec fields the caller didn't set. The template's
	// snapshot is already in the warm cache so container.Run hits the
	// restore fast path instead of cold-booting from scratch.
	if req.Template != "" {
		t, err := c.GetTemplate(ctx, req.Template)
		if err != nil {
			return nil, fmt.Errorf("%w: template %q: %v", ErrTemplateNotFound, req.Template, err)
		}
		if req.Image == "" {
			req.Image = stringFromSpec(t.Spec, "image")
		}
		if req.KernelPath == "" {
			req.KernelPath = stringFromSpec(t.Spec, "kernel_path")
		}
		if req.MemMB == 0 {
			req.MemMB = uint64FromSpec(t.Spec, "mem_mb")
		}
		if req.CPUs == 0 {
			req.CPUs = intFromSpec(t.Spec, "cpus")
		}
		req.Template = "" // never sent on the wire
	}
	var resp CreateSandboxResponse
	if err := c.post(ctx, "/sandboxes", req, &resp); err != nil {
		return nil, err
	}
	resp.Sandbox.client = c
	return &resp.Sandbox, nil
}

// stringFromSpec / uint64FromSpec / intFromSpec are tiny helpers for
// reading a field out of a template's Spec map (the SDK models the
// template's Spec as map[string]interface{} for wire flexibility).
func stringFromSpec(spec map[string]interface{}, key string) string {
	if v, ok := spec[key].(string); ok {
		return v
	}
	return ""
}

func uint64FromSpec(spec map[string]interface{}, key string) uint64 {
	switch v := spec[key].(type) {
	case float64:
		return uint64(v)
	case uint64:
		return v
	case int:
		return uint64(v)
	}
	return 0
}

func intFromSpec(spec map[string]interface{}, key string) int {
	switch v := spec[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case uint64:
		return int(v)
	}
	return 0
}

func (c *Client) ListSandboxes(ctx context.Context) ([]Sandbox, error) {
	var resp struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	if err := c.get(ctx, "/sandboxes", &resp); err != nil {
		return nil, err
	}
	for i := range resp.Sandboxes {
		resp.Sandboxes[i].client = c
	}
	return resp.Sandboxes, nil
}

func (c *Client) GetSandbox(ctx context.Context, id string) (*Sandbox, error) {
	var sb Sandbox
	if err := c.get(ctx, "/sandboxes/"+id, &sb); err != nil {
		return nil, err
	}
	sb.client = c
	return &sb, nil
}

func (c *Client) Delete(ctx context.Context, id string) error {
	return c.request(ctx, http.MethodDelete, "/sandboxes/"+id, nil, nil, []int{http.StatusNoContent})
}

// Recycle tears down a leased sandbox and returns a fresh one from the
// same pool in a single HTTP round trip. The returned *Sandbox has a
// new id; the old id is gone. Errors:
//
//   - ErrSandboxNotFound: id unknown or already deleted
//   - ErrConflict: the sandbox was cold-booted, not leased (recycle
//     only works for pool-origin sandboxes)
//   - ErrPoolNotFound: the pool was unregistered between lease and
//     recycle
//   - ErrPoolExhausted: the pool's AcquireWait timed out (5 s)
func (c *Client) Recycle(ctx context.Context, id string) (*Sandbox, error) {
	var resp CreateSandboxResponse
	if err := c.request(ctx, http.MethodPost, "/sandboxes/"+id+"/recycle", nil, &resp, []int{http.StatusCreated, http.StatusOK}); err != nil {
		return nil, err
	}
	resp.Sandbox.client = c
	return &resp.Sandbox, nil
}

// Pool lifecycle ---------------------------------------------------

func (c *Client) RegisterPool(ctx context.Context, req CreatePoolRequest) (*Pool, error) {
	var p Pool
	if err := c.post(ctx, "/pools", req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Client) ListPools(ctx context.Context) ([]Pool, error) {
	var resp struct {
		Pools []Pool `json:"pools"`
	}
	if err := c.get(ctx, "/pools", &resp); err != nil {
		return nil, err
	}
	return resp.Pools, nil
}

func (c *Client) UnregisterPool(ctx context.Context, templateID string) error {
	return c.request(ctx, http.MethodDelete, "/pools/"+templateID, nil, nil, []int{http.StatusNoContent})
}

func (c *Client) LeaseSandbox(ctx context.Context, req LeaseSandboxRequest) (*Sandbox, error) {
	var resp CreateSandboxResponse
	if err := c.post(ctx, "/sandboxes/lease", req, &resp); err != nil {
		return nil, err
	}
	resp.Sandbox.client = c
	return &resp.Sandbox, nil
}

// Templates --------------------------------------------------------

func (c *Client) CreateTemplate(ctx context.Context, req CreateTemplateRequest) (*CreateTemplateResponse, error) {
	var resp CreateTemplateResponse
	if err := c.post(ctx, "/templates", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ListTemplates(ctx context.Context) ([]Template, error) {
	var resp struct {
		Templates []Template `json:"templates"`
	}
	if err := c.get(ctx, "/templates", &resp); err != nil {
		return nil, err
	}
	return resp.Templates, nil
}

func (c *Client) GetTemplate(ctx context.Context, id string) (*Template, error) {
	var t Template
	if err := c.get(ctx, "/templates/"+id, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *Client) DeleteTemplate(ctx context.Context, id string) error {
	return c.request(ctx, http.MethodDelete, "/templates/"+id, nil, nil, []int{http.StatusNoContent})
}

// Preview ----------------------------------------------------------

func (c *Client) MintPreview(ctx context.Context, sandboxID string, port uint16) (*Preview, error) {
	var p Preview
	path := fmt.Sprintf("/sandboxes/%s/preview/%d", sandboxID, port)
	if err := c.post(ctx, path, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Health -----------------------------------------------------------

func (c *Client) Healthz(ctx context.Context) (bool, error) {
	var resp struct {
		OK bool `json:"ok"`
	}
	if err := c.get(ctx, "/healthz", &resp); err != nil {
		return false, err
	}
	return resp.OK, nil
}

// ---- HTTP internals ---------------------------------------------

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.request(ctx, http.MethodGet, path, nil, out, []int{http.StatusOK})
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.request(ctx, http.MethodPost, path, body, out, []int{http.StatusOK, http.StatusCreated})
}

func (c *Client) request(ctx context.Context, method, path string, body, out any, okStatus []int) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return &Error{Message: fmt.Sprintf("%s %s: marshal: %v", method, path, err)}
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return &Error{Message: fmt.Sprintf("%s %s: %v", method, path, err)}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Message: fmt.Sprintf("%s %s: %v", method, path, err)}
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)

	ok := false
	for _, s := range okStatus {
		if resp.StatusCode == s {
			ok = true
			break
		}
	}
	if !ok {
		return wrapHTTPError(resp.StatusCode, string(rawBody), fmt.Sprintf("%s %s", method, path))
	}
	if out == nil || len(rawBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(rawBody, out); err != nil {
		return &Error{Message: fmt.Sprintf("%s %s: decode: %v", method, path, err)}
	}
	return nil
}

func wrapHTTPError(status int, body, context string) error {
	msg := body
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		if s, ok := parsed["error"].(string); ok && s != "" {
			msg = s
		}
	}
	full := fmt.Sprintf("%s: %s", context, msg)
	e := &Error{Message: full, Status: status, Body: body}
	// Sentinel wrapping: attach via errors.Is chain so callers can
	// match on the well-known errors. We stash in a wrappedErr type
	// to preserve Error{} fields.
	switch status {
	case http.StatusNotFound:
		return &sentinelErr{sentinel: guessNotFoundSentinel(context), wrapped: e}
	case http.StatusBadRequest:
		return &sentinelErr{sentinel: ErrInvalidRequest, wrapped: e}
	case http.StatusConflict:
		return &sentinelErr{sentinel: ErrConflict, wrapped: e}
	}
	return e
}

// guessNotFoundSentinel returns a more specific sentinel based on
// the request context (e.g. "DELETE /templates/..." → ErrTemplateNotFound).
// Keeps errors.Is(err, ErrTemplateNotFound) matching right without
// adding a distinct HTTP status per resource kind.
func guessNotFoundSentinel(context string) error {
	if strings.Contains(context, "/templates") {
		return ErrTemplateNotFound
	}
	if strings.Contains(context, "/pools") {
		return ErrPoolNotFound
	}
	return ErrSandboxNotFound
}

// sentinelErr glues a *Error with errors.Is support for the
// package-level sentinels. Keeps the Status / Body fields available
// via errors.As(err, &*Error).
type sentinelErr struct {
	sentinel error
	wrapped  *Error
}

func (s *sentinelErr) Error() string { return s.wrapped.Error() }
func (s *sentinelErr) Unwrap() error { return s.wrapped }
func (s *sentinelErr) Is(target error) bool {
	return target == s.sentinel
}
