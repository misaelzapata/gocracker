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
