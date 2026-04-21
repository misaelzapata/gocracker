package agent

// Wire types used by the toolbox HTTP-over-vsock handlers. These are a
// minimal cherry-pick from sandboxes/internal/model in feat/sandboxes-v2 —
// only what handlers in this package serialize on the wire. The full
// sandbox/template/pool model lands in Fase 4 with the control plane.

// SandboxSecret is a host→guest secret entry. The guest stores these in
// memory only; persistence is the host control plane's job. AllowedHosts
// is reserved for the egress-proxy header injection that lands in Fase 4
// — for this slice the field is accepted but not enforced anywhere.
type SandboxSecret struct {
	Name         string   `json:"name"`
	Value        string   `json:"value"`
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
}

type FileEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"` // "file" | "dir"
	Size int64  `json:"size"`
}

type FileListResponse struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

type GitCloneRequest struct {
	Repository string `json:"repository"`
	Directory  string `json:"directory"`
}

type GitStatusRequest struct {
	Directory string `json:"directory"`
}

// SetNetworkRequest is the host-only payload that re-IPs the guest's
// primary interface after a snapshot restore. Sent host → agent on
// POST /internal/setnetwork between vmm.Restore() and returning the
// VM to the user. The host control plane is trusted to provide a
// non-conflicting IP/MAC pair from its sandbox pool — the agent does
// no allocation, just applies what it's told.
type SetNetworkRequest struct {
	Interface string `json:"interface,omitempty"` // empty = "eth0"
	IP        string `json:"ip"`                  // CIDR notation, e.g. "10.100.7.2/30"
	Gateway   string `json:"gateway,omitempty"`   // bare IP, e.g. "10.100.7.1"
	MAC       string `json:"mac,omitempty"`       // colon-hex, e.g. "02:42:00:00:00:07"
}

// SetNetworkResponse mirrors the request inputs back so the host can
// log/audit. Errors come back as HTTP 4xx/5xx with the error JSON
// the rest of the agent uses.
type SetNetworkResponse struct {
	OK        bool   `json:"ok"`
	Interface string `json:"interface"`
	IP        string `json:"ip"`
	Gateway   string `json:"gateway,omitempty"`
	MAC       string `json:"mac,omitempty"`
}
