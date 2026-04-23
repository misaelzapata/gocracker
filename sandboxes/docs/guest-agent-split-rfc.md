# RFC: Split Guest Control Agent (`10022`) and Toolbox Agent (`10023`)

## Summary

`gocracker` already has the right primitive for host-to-guest control: a small
guest agent on vsock port `10022` used by `exec`, PTY, resize, and post-restore
network reconfiguration.

For `sandboxes`, that agent is currently overloaded. It is asked to do all of
the following:

- provide the core `exec` control path
- recover quickly after restore
- support post-restore re-IP for `network_mode=auto`
- bootstrap `toolboxguest`
- carry large `stdin` payloads during file upload / toolbox installation
- indirectly gate warm-pool readiness

This RFC recommends splitting responsibilities into two planes:

- `10022`: **Guest control agent**
- `10023`: **Toolbox agent**

The short version:

- keep `10022` minimal, stable, and required for the runtime
- move rich sandbox APIs to `10023`
- stop using `exec + large stdin` as the primary file/bootstrap transport
- preinstall `toolboxguest` in templates / snapshots so the hot path does not
  upload it on every create

This is the closest fit to patterns used by serious VM/sandbox systems:

- Firecracker / crosvm / Cloud Hypervisor: transport only, custom agent above
- QEMU Guest Agent: formal admin RPCs for guest operations
- Kata agent: persistent runtime agent over vsock
- Daytona / E2B / Docker Sandboxes: toolbox/controller baked into images,
  templates, or snapshots instead of late bootstrap in the request path

## Problem Statement

The current architecture works well for low-volume, small-payload guest control,
but it is fragile for sandbox workloads for three reasons:

1. `10022` is in the critical path for too many unrelated concerns.
2. Large `stdin` payloads over `exec` are a poor fit for the current transport.
3. Sandbox hot/warm readiness depends on a control path that was designed first
   for VM management, not for rich sandbox APIs.

Observed symptoms:

- restore succeeds at the VM level but warm creation stalls waiting for `exec`
- `EnsureToolbox()` may reinstall or respawn in the critical path
- ARM showed a reproducible failure threshold for larger `exec stdin` payloads
- `toolboxguest` bootstrap is less stable than the VM lifecycle itself

The issue is not that an agent exists. The issue is that a **single small
control agent is carrying both control-plane and data-plane responsibilities**.

## Current State

### Control agent (`10022`)

The existing protocol and default port live in:

- [`internal/guestexec/protocol.go`](../../internal/guestexec/protocol.go)

Guest-side handling lives in:

- [`internal/guest/init.go`](../../internal/guest/init.go)

Host/API entry points live in:

- [`internal/api/exec.go`](../../internal/api/exec.go)
- [`internal/api/api.go`](../../internal/api/api.go)

Current responsibilities already handled through `10022`:

- command execution
- exec streaming / PTY
- terminal resize
- memory stats
- memory hotplug support
- post-restore guest re-IP scripts

### Toolbox path (`10023`)

The richer sandbox agent currently lives in:

- [`sandboxes/internal/toolboxguest/`](../internal/toolboxguest)

Host-side proxy / bootstrap logic lives in:

- [`sandboxes/internal/toolboxhost/host.go`](../internal/toolboxhost/host.go)

Current intent for `10023`:

- health/version
- file operations
- process helpers
- git
- preview assistance

However, in practice, `10023` still depends on `10022` for:

- installation
- spawn
- readiness gating
- recovery

That is acceptable, but the current bootstrap method is not.

## Design Goals

1. Preserve `gocracker` runtime compatibility.
2. Keep the core VM control plane small and reliable.
3. Support fast warm/hot sandbox starts.
4. Avoid large-payload transport through `exec stdin`.
5. Make `toolboxguest` versioned and upgradable without destabilizing the VM.
6. Support future richer APIs without bloating the core guest agent.

## Non-Goals

- Replacing vsock with SSH as the primary control path
- Removing the guest agent entirely
- Turning the core runtime into a full sandbox platform
- Requiring `virtio-fs` for the base design

## Proposed Architecture

### Port split

| Port | Role | Required by runtime | Required by sandboxes | Payload profile |
|---|---|---:|---:|---|
| `10022` | Guest control agent | Yes | Yes | Small control messages |
| `10023` | Toolbox agent | No | Yes | Rich sandbox API / file & process ops |

### `10022`: Guest control agent

`10022` should remain the runtime's minimal, always-present control channel.

Responsibilities:

- `ping` / ready check
- small `exec`
- PTY / stream bootstrap
- resize
- post-restore re-IP / route repair
- optional low-volume diagnostics
- memory / balloon / hotplug hooks

Explicitly **not** its responsibility:

- large file upload/download
- toolbox binary distribution in the hot path
- rich filesystem API
- git operations
- language server integration
- preview proxying

Design rule:

> If the operation can be expressed as "small command + small response", it can
> stay on `10022`. If it becomes a service or moves bytes in bulk, it should not.

### `10023`: Toolbox agent

`10023` should be the persistent sandbox-facing API endpoint.

Responsibilities:

- health and version
- file operations
- process operations beyond the base runtime `exec`
- git operations
- preview/port helpers
- future LSP or richer IDE-like APIs

Transport options:

- HTTP over vsock
- ttrpc over vsock
- custom framed RPC over vsock

For v1 of the split, HTTP over vsock is acceptable because:

- it is easy to debug
- the SDK shape already maps well to resource endpoints
- it aligns with Daytona's toolbox model

Longer term, a ttrpc/Kata-style RPC layer may be better for lower overhead and
typed APIs, but it is not required to fix the current problem.

## Bootstrap Model

### Recommended model

`toolboxguest` should be **present in the base image, template, or snapshot**.

That means:

- source/base templates include the toolbox binary
- hot/warm clones do not upload the toolbox
- create/restore only needs to verify or respawn it

Allowed recovery path:

- if `10023` is not healthy, use `10022` to respawn it
- if the version is wrong, mark the template generation stale and rotate

Discouraged path:

- uploading `toolboxguest` through `exec + stdin` during normal create/restore

### Why this matters

The hot path should be:

1. restore/resume VM
2. wait for `10022`
3. verify `10023`
4. return sandbox

It should **not** be:

1. restore/resume VM
2. upload toolbox binary
3. write files through shell fragments
4. spawn toolbox
5. retry health
6. maybe reinstall
7. return sandbox

The second path wastes the benefit of snapshots and makes cold/warm behavior far
more fragile.

## Alternatives Considered

### A. Keep everything on `10022`

Pros:

- minimal component count
- no second agent/service

Cons:

- grows the runtime agent into an unbounded sandbox API
- increases blast radius of every change
- keeps file/bootstrap transport coupled to VM control
- makes restore and warm readiness harder to reason about

Verdict:

- not recommended

### B. Remove the guest agent and use SSH

Pros:

- familiar tooling

Cons:

- depends on networking and guest userspace being ready
- poor fit for post-restore re-IP
- worse for hot/warm control plane
- introduces credentials/session management complexity

Verdict:

- useful as human access, not as primary sandbox control plane

### C. Use `virtio-fs` / shared mounts as the main bootstrap path

Pros:

- strong solution for large file injection
- great for dev/compose-style shared workspaces

Cons:

- not always available or desirable
- does not replace exec/readiness/re-IP
- can complicate snapshot/clone stories depending on backend constraints

Verdict:

- good optional complement, not the main answer

### D. QEMU Guest Agent-style unified RPC

Pros:

- proven model
- formal file and exec operations

Cons:

- larger surface area than we need
- conflates runtime admin and sandbox APIs again if copied wholesale

Verdict:

- good inspiration, but copy the ideas selectively

### E. Kata-style runtime agent + service split

Pros:

- closest conceptual fit for microVM sandbox runtime
- structured RPC over vsock
- clean host/runtime contract

Cons:

- bigger migration
- more up-front design work

Verdict:

- best long-term direction, especially if `10023` later evolves from HTTP to
  typed RPC

## Recommended Direction

### Phase target

The recommended steady state is:

- `10022` = runtime control agent
- `10023` = sandbox toolbox agent
- templates/snapshots include `toolboxguest`
- `sandboxd` only verifies/respawns toolbox, not bulk-installs it

### What to keep on `10022`

- `exec`
- `exec/stream`
- `resize`
- `ping`
- guest re-IP / route repair
- runtime-specific diagnostics

### What to move to `10023`

- file upload/download/list/delete
- git clone/status/etc.
- preview helpers
- richer process/session APIs
- future LSP / interpreter APIs

## Compatibility Constraints

The migration must not break:

- existing `/vms/{id}/exec`
- existing `/vms/{id}/exec/stream`
- clone / pause / resume / restore flows
- `network_mode=auto` post-restore logic
- current non-sandbox runtime usage

That means:

- `10022` remains supported and stable
- `sandboxes` moves first
- the runtime API does not need to become toolbox-aware

## Migration Plan

### Phase 0: Clarify ownership

Immediate documentation and code ownership rule:

- runtime owns `10022`
- sandboxes own `10023`

No behavior changes required yet.

### Phase 1: Stop using `10022` for bulk transfer

Goal:

- remove large payload bootstrap from the normal create path

Actions:

- keep `InstallToolboxBinary()` only as a repair/debug fallback
- mark large `exec stdin` paths as non-hot-path and non-default
- prefer template-seeded toolbox binaries

Expected impact:

- fewer create failures
- less restore fragility
- more honest warm/hot timings

### Phase 2: Seed toolbox into templates

Goal:

- every `base-*` template contains the correct toolbox binary already

Actions:

- install `toolboxguest` during template build/seed
- record toolbox version in template metadata
- treat version mismatch as template generation mismatch, not as create-time
  reinstall pressure

Expected impact:

- hot starts stop depending on binary upload
- paused/hot pools become much more predictable

### Phase 3: Make `10023` the real sandbox API

Goal:

- host-side `toolboxhost` routes rich APIs to `10023`

Actions:

- move file APIs to toolbox-backed implementation first
- move git next
- keep process APIs dual-path if useful during transition

Expected impact:

- cleaner split between runtime control and sandbox services

### Phase 4: Shrink `10022` semantics

Goal:

- define `10022` as a strict runtime interface

Actions:

- avoid adding new rich sandbox operations there
- keep the protocol compact and stable

Expected impact:

- lower blast radius of runtime changes

### Phase 5: Optional RPC evolution

Goal:

- if HTTP over vsock becomes limiting, evolve `10023` to typed RPC

Candidates:

- ttrpc
- gRPC-like internal RPC
- custom framed RPC

This phase is optional and should happen only after the split is paying off.

## Operational Rules

1. A sandbox create/restore should never need a large binary upload in the hot path.
2. A toolbox version upgrade should rotate template generations, not force every
   lease to reinstall.
3. Warm readiness must mean:
   - VM is resumed or running
   - `10022` is ready
   - `10023` is healthy
4. `sandboxd` should treat toolbox respawn as a repair path, not the default.

## Anti-Patterns to Avoid

- using `exec` as a generic bulk file transport
- adding more and more sandbox features to `10022`
- snapshotting templates immediately after unstable control-plane activity
- conflating "VM restored" with "sandbox ready"
- making warm-pool reconciliation depend on slow reinstall flows

## Why This Fits Other Systems

- **Firecracker / crosvm / Cloud Hypervisor**: they give you vsock and VM
  primitives; the service model above that is yours to design.
- **QEMU Guest Agent**: validates the idea of a formal guest control API, but
  its full surface is broader than what `gocracker` needs in the runtime.
- **Kata agent**: validates the persistent in-guest runtime-agent model over
  vsock and is the strongest architectural reference for long-term evolution.
- **Daytona**: validates the toolbox-as-service model inside the sandbox.
- **E2B / Docker Sandboxes**: validate baking the controller/tooling into
  templates/snapshots instead of re-installing on every create.

## Decision

Adopt the split:

- `10022` = runtime guest control agent
- `10023` = sandbox toolbox agent

And adopt the bootstrap rule:

- toolbox is template-baked by default
- runtime `exec` bootstrap is fallback only

That gives `gocracker` a cleaner runtime boundary while letting `sandboxes`
grow richer without destabilizing the VM control path.

## References

- QEMU Guest Agent:
  - https://www.qemu.org/docs/master/interop/qemu-ga.html
  - https://www.qemu.org/docs/master/interop/qemu-ga-ref.html
- Firecracker vsock and snapshot limitations:
  - https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md
  - https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md
- Cloud Hypervisor:
  - https://www.cloudhypervisor.org/docs/prologue/commands/
- crosvm / ChromiumOS vsock docs:
  - https://chromium.googlesource.com/chromiumos/platform2/+/HEAD/vm_tools/docs/vsock.md
- Daytona:
  - https://www.daytona.io/docs/en/sandboxes/
  - https://www.daytona.io/docs/ja/architecture/
- E2B:
  - https://e2b.dev/docs/template/how-it-works
  - https://e2b.dev/docs/sandbox/snapshots
- Docker Sandboxes:
  - https://docs.docker.com/ai/sandboxes/templates/
  - https://docs.docker.com/ai/sandboxes/agents/
