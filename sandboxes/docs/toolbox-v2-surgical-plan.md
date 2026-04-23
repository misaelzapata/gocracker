# Toolbox V2 Surgical Execution Plan

## Purpose

This document is the execution plan for rebuilding the sandbox toolbox path into
a reliable, test-first architecture without guessing and without mixing runtime
control concerns with rich sandbox APIs.

The plan assumes work happens on a dedicated branch and that every change is
validated immediately before moving to the next step.

Current branch for this effort:

- `feat/sandboxes-v2`

## What We Are Solving

The current sandbox stack has the right high-level primitives, but the current
toolbox path is too fragile:

- the runtime control agent (`10022`) is overloaded
- sandbox create/restore still depends too much on runtime `exec`
- large `exec stdin` payloads are a known bad fit for the control path
- toolbox bootstrap is still too entangled with warm readiness
- warm-pool behavior becomes hard to reason about when toolbox install/repair
  happens in the request path

The JSON / KB limit issue is not a cosmetic bug. It is a design smell:

- control messages and bulk data should not share the same critical path
- sandbox readiness should not depend on shipping large binaries through `exec`

## Target End State

The target state is intentionally simpler than the current mixed approach.

### Port split

| Port | Role | Owner | Allowed responsibilities |
|---|---|---|---|
| `10022` | guest control agent | runtime | small `exec`, PTY, resize, health, post-restore re-IP |
| `10023` | toolbox agent | sandboxes | files, git, preview helpers, richer process APIs |

### Template rule

`toolboxguest` must be present in the template or snapshot used by sandboxes.

Normal create/restore must **not** upload the toolbox binary.

The only acceptable uses of `10022` for toolbox lifecycle are:

- verify that the guest is ready
- respawn toolbox if already present
- repair/debug fallback

### Hot path

The happy path for a sandbox lease must become:

1. restore/resume VM
2. wait for `10022` readiness
3. verify `10023`
4. return sandbox

It must **not** become:

1. restore/resume VM
2. create directories
3. upload toolbox over `exec + stdin`
4. chmod/move it in place
5. spawn it
6. probe it
7. retry on failure
8. return sandbox

## Design Constraints

1. Do not break the current runtime API unless absolutely necessary.
2. Do not touch the `gocracker` core by convenience.
3. If a core change becomes unavoidable, stop and request explicit approval.
4. Every phase must finish with a reproducible local test.
5. No phase is “done” based on theory; it is only done after a concrete test
   passes.

## Autonomous Execution Mode

This plan is intended to be executed autonomously, not as a supervised
checklist that requires user approval after every micro-step.

Working rule:

- continue through the phases until the toolbox path is actually working
- stop only for:
  - a required core runtime change
  - a destructive choice with non-obvious consequences
  - a hard external blocker that cannot be resolved from the repo/host

Execution style:

1. make the smallest change that can improve correctness
2. run the nearest direct test immediately
3. run the closest integration test immediately after
4. if the result is good, continue without waiting for supervision
5. if the result is bad, debug the exact failure before changing more code

The goal is not to “finish the plan document”. The goal is to keep moving until
the system is functionally reliable.

## Cache And State Hygiene

Cache and local state must be treated as part of the test harness.

Rule:

- if a phase touches template seeding, restore behavior, warm-pool behavior,
  toolbox versioning, or local startup scripts, clear the relevant caches and
  state before trusting the next result

Minimum cleanup targets:

- `.tmp/sandboxes-local/templates/*`
- `.tmp/sandboxes-local/sandboxd/*`
- `.tmp/sandboxes-local/runtime/*`
- `.tmp/sandboxes-local/cache/*`

Also kill stale local processes before full-stack reruns:

- old `gocracker`
- old `gocracker-sandboxd`
- stale seeded VMs that would invalidate the next measurement

Interpretation rule:

- a result only counts if we know whether it came from a fresh seed/restore or
  from a previously cached artifact
- if we do not know, the run is not trustworthy

## How The Final System Will Work

### Runtime plane (`10022`)

The runtime guest agent stays responsible for:

- `POST /vms/{id}/exec`
- `POST /vms/{id}/exec/stream`
- `POST /vms/{id}/exec/stream/resize`
- post-restore guest re-IP for `network_mode=auto`
- runtime-specific health/readiness

The runtime plane should be treated as a small control plane, not as a generic
data upload channel.

### Toolbox plane (`10023`)

`toolboxguest` becomes the stable in-guest service for sandbox APIs:

- `GET /healthz`
- `GET /info/version`
- `GET /files`
- `GET /files/download`
- `POST /files/upload`
- `DELETE /files`
- `POST /files/mkdir`
- `POST /files/rename`
- `POST /files/chmod`
- `POST /git/clone`
- `POST /git/status`
- preview/HTTP proxy helpers
- future richer process/session endpoints if needed

### Template lifecycle

Template build/seed becomes the place where toolbox is installed:

1. boot source VM
2. provision language/runtime tools
3. install toolbox binary in guest filesystem
4. start toolbox once and verify health/version
5. run ready command
6. snapshot template

The template metadata records:

- toolbox version
- template generation
- source VM id
- snapshot dir

### Sandbox create/restore lifecycle

For a hot or paused lease:

1. runtime resume/restore
2. runtime exec probe on `10022`
3. toolbox health probe on `10023`
4. optional respawn on `10022` if toolbox exists but is not running
5. return sandbox

For a cold fallback:

1. restore from snapshot/template
2. runtime exec probe on `10022`
3. toolbox health probe on `10023`
4. optional respawn only
5. return sandbox

No binary installation is allowed in the normal path.

## Execution Strategy

The work is intentionally split into small, test-gated phases.

### Phase 0: Freeze the battlefield

Goal:

- create a clean branch for the v2 effort
- document the target architecture and the execution plan
- define the cleanup/reset procedure used before full-stack validation

Deliverables:

- branch `feat/sandboxes-v2`
- this document
- RFC already in `guest-agent-split-rfc.md`
- explicit cache/state reset checklist

Test gate:

- `git branch --show-current`
- docs present and linked
- reset procedure documented and usable

### Phase 1: Lock in deterministic repro tests

Goal:

- codify the failures before touching behavior

Work:

- add regression tests for large `exec stdin`
- add regression tests for post-restore exec readiness
- add regression tests for toolbox health after restore
- document which tests require full cache/state cleanup before rerun

Primary files:

- `internal/vsock/vsock_test.go`
- `internal/vsock/vsock_extended_test.go`
- `internal/api/exec_test.go`
- new sandbox integration test under `tests/integration/`

Required checks:

1. payload matrix over `exec stdin`
   - 1 KiB
   - 4 KiB
   - 16 KiB
   - 64 KiB
   - 256 KiB
2. boot -> exec -> snapshot -> restore -> exec
3. template -> restore -> toolbox health

Test gate:

- all repro tests fail or pass in a way that matches the current bug report
- the failure mode is reproducible on demand
- rerunning from a clean cache produces the same result

### Phase 2: Prove or fix control-plane transport correctness

Goal:

- make `10022` trustworthy for small control traffic
- explicitly prove whether large payloads are still unsafe

Work:

- audit `internal/vsock/vsock.go`
- verify backpressure, descriptor exhaustion, packet truncation behavior
- verify framing assumptions in `internal/guestexec/protocol.go`
- keep runtime `exec` reliable for small control payloads

Important note:

The target here is **not** “use `exec` for bulk transfer forever”.
The target is:

- small control payloads must be reliable
- large payload tests must no longer silently corrupt streams
- if large payloads remain intentionally limited, that limit must be explicit
  and outside the hot path

Primary files:

- `internal/vsock/vsock.go`
- `internal/guestexec/protocol.go`
- `internal/guest/init.go`

Test gate:

- control-plane exec/re-IP works repeatedly after restore
- no silent truncation/corruption on tested payload sizes
- if a limit remains, it is explicit and tested, not accidental

### Phase 3: Simplify `10022` semantics

Goal:

- turn the runtime guest agent into a strict control agent

Work:

- define the supported uses of runtime exec
- remove assumptions that it is the generic file/bootstrap channel
- keep PTY, resize, health, re-IP, and small command execution

Primary files:

- `internal/guestexec/protocol.go`
- `internal/guest/init.go`
- `internal/api/exec.go`
- `internal/api/api.go`

Test gate:

- `exec`, `exec/stream`, resize, and restore re-IP still work
- compose/manual smoke tests still pass

### Phase 4: Harden `toolboxguest` as the real sandbox service

Goal:

- make `10023` the primary sandbox API endpoint

Work:

- review and simplify `toolboxguest`
- keep only stable v1 endpoints
- improve errors, health, and lifecycle
- make host proxy code use toolbox directly for files/git

Primary files:

- `sandboxes/internal/toolboxguest/server.go`
- `sandboxes/internal/toolboxguest/vsock_linux.go`
- `sandboxes/internal/toolboxhost/host.go`

Target v1 surface:

- `healthz`
- `info/version`
- `files/*`
- `git/*`
- preview helper endpoints if needed

Test gate:

- files upload/download/list/delete work repeatedly
- git clone/status works repeatedly
- direct host->toolbox vsock proxy is stable under repetition

### Phase 5: Remove toolbox install from the hot path

Goal:

- normal create/restore no longer uploads toolbox through runtime exec

Work:

- move toolbox installation into template seed/build only
- convert `InstallToolboxBinary()` from default path to repair path
- `EnsureToolbox()` becomes:
  - wait for exec
  - health/version check
  - respawn if binary already exists
  - repair only when explicitly allowed

Primary files:

- `sandboxes/internal/toolboxhost/host.go`
- `sandboxes/internal/templates/service.go`
- `sandboxes/internal/model/types.go`

Test gate:

- template seed installs toolbox once
- hot lease does not call upload path
- paused lease does not call upload path
- cold restore does not call upload path in normal operation

### Phase 6: Make templates the unit of toolbox versioning

Goal:

- toolbox upgrades happen by template generation, not by ad-hoc reinstall

Work:

- add toolbox version to template metadata
- add generation mismatch handling
- if version mismatch exists, mark template stale and rotate

Primary files:

- `sandboxes/internal/templates/service.go`
- `sandboxes/internal/store/store.go`
- `sandboxes/internal/model/types.go`

Test gate:

- new template generation created when toolbox version changes
- old warm entries drain cleanly
- new leases come from matching generation

### Phase 7: Rebuild warm readiness around verification, not installation

Goal:

- hot/paused pool capacity reflects truly ready sandboxes

Work:

- warm create path should:
  - restore/resume VM
  - confirm `10022`
  - confirm `10023`
  - mark warm ready
- remove implicit install/repair work from the normal reconcile path

Primary files:

- `sandboxes/internal/pool/manager.go`
- `sandboxes/internal/controlplane/server.go`
- `sandboxes/internal/toolboxhost/host.go`

Test gate:

- warm pool refill no longer blocks on toolbox upload
- hot lease and paused lease stay low-latency
- pool behavior under burst remains predictable

### Phase 8: Real integration and stress

Goal:

- prove the whole stack, not just units

Work:

- clear caches and stale processes before each full-stack run
- run local stack end-to-end
- run SDK examples
- run burst/stress tests
- run preview tests

Required scenarios:

1. hot create -> exec -> files
2. paused create -> resume -> exec -> files
3. cold fallback -> exec -> files
4. preview URL against a real guest HTTP server
5. repeated create/delete cycles
6. repeated pause/resume cycles
7. concurrent burst above pool size

Test gate:

- all scenarios pass locally
- failures are explicit and actionable, not hangs/timeouts

## Immediate First Cut

The very first functional cut we should implement is:

1. prove the transport behavior with regression tests
2. stop relying on upload-over-exec in the normal path
3. seed toolbox into templates
4. reduce `EnsureToolbox()` to verify/respawn

That gives the biggest reliability improvement with the smallest conceptual
change.

## Success Criteria

We can call the toolbox path reliable when all of the following are true:

1. normal sandbox create does not upload the toolbox binary
2. hot and paused leases use verify/respawn only
3. `exec` works reliably after restore
4. file operations do not depend on runtime exec payload hacks
5. preview works through the toolbox path
6. burst behavior degrades to paused/cold predictably, not randomly
7. ARM and x86 both pass the same toolbox readiness scenarios

## Things We Explicitly Will Not Guess

- whether a transport limit is “probably fixed”
- whether a restore race is “probably gone”
- whether warm readiness is “probably okay”

Every one of those gets a test or a measured trace before we move forward.

## Practical Rule For Every Change

For every non-trivial change:

1. edit code
2. run the smallest direct test for that change immediately
3. if that passes, run the nearest integration test
4. only then continue

No batching of ten speculative changes before validating behavior.

## Practical Rule For Full-Stack Revalidation

For every meaningful change in templates, restore logic, toolbox lifecycle, or
warm-pool behavior:

1. stop local sandbox processes
2. clear the relevant `.tmp/sandboxes-local/*` caches/state
3. restart the stack from a known baseline
4. rerun the specific failing scenario
5. only keep the result if it reproduces from the clean state
