// Package spec is the no-dependency source of truth for toolbox
// constants shared between guest init.go (must stay tiny — no big
// transitive imports), pkg/container/ disk build, and the embed
// package itself. Putting Path here keeps internal/guest/init from
// pulling in toolboxembed.Binary (~6 MB of embedded data) just to
// know where to exec the agent.
package spec

// BinaryPath is where pkg/container writes the agent into every guest
// disk and where internal/guest/init.go execs it from after switch_root.
const BinaryPath = "/opt/gocracker/toolbox/toolboxguest"

// VersionFilePath sits next to the binary; written at disk-build time
// and read by snapshot metadata to detect agent-version drift.
const VersionFilePath = "/opt/gocracker/toolbox/VERSION"

// Version is bumped when the wire protocol or behavior changes
// incompatibly. Snapshot metadata records the version that was baked
// at capture time; restore validates parity.
const Version = "0.1.0"

// VsockPort is the agent's listening port. The host UDS listener (Fase 1)
// proxies "CONNECT 10023\n" to this port. Distinct from the legacy
// internal/guestexec port (10022) so both can coexist.
const VsockPort uint32 = 10023
