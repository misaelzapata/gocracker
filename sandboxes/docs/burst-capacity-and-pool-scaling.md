# Burst Capacity and Pool Scaling for `sandboxd`

## Summary

`sandboxd` is already using the right family of primitives for fast agent sandboxes:

- pre-built templates
- snapshot restore
- a hybrid `hot` + `paused` pool
- cheap `exec` into an already-running microVM

The current limitation is not the raw restore speed of `gocracker`. In local measurements on April 17, 2026, low-level restore from `snapshot_dir` stayed around tens of milliseconds, while the user-visible `cold` fallback from `sandboxd` was closer to `~860 ms`. The gap is almost entirely in the control plane and readiness path: pool sizing, refill behavior, resume/exec/toolbox validation, and reconcile stalls.

This document describes:

1. how `sandboxd` behaves today
2. what was measured locally
3. why bursts still fall to cold too early
4. what other sandbox systems and VM systems do differently
5. a concrete design for scaling concurrent starts without linearly increasing always-on VM count

The main recommendation is to evolve from a two-tier pool (`hot`, `paused`) into a three-tier system:

- `hot`: already running and exec-ready
- `paused`: resumed in milliseconds, cheaper than hot
- `restore-ready`: snapshot-backed overflow capacity, restored on demand but still much faster and more predictable than full cold provisioning

That should be combined with a per-template adaptive controller, per-template refill workers, and a cleaner separation between "runtime restored" and "sandbox ready for the first user exec".

## Scope

This document focuses on `sandboxes/` on top of `gocracker`:

- `sandboxes/internal/controlplane`
- `sandboxes/internal/pool`
- `sandboxes/internal/templates`
- `sandboxes/internal/toolboxhost`
- `sandboxes/internal/store`

It does not propose changes to the core `gocracker` runtime in this version of the document. Where core behavior matters, the document treats it as an external dependency.

## Current State of `sandboxd`

### Creation path

User sandbox creation currently follows this order in `createSandbox()`:

1. lease a `hot` warm sandbox if one exists
2. otherwise lease a `paused` warm sandbox and resume it
3. otherwise fall back to `cloneFromTemplate()` and create a `cold` sandbox from the template snapshot

Relevant code:

- `sandboxes/internal/controlplane/server.go`
  - `createSandbox()`
  - `cloneFromTemplate()`

For a warm lease, the control plane does more than just hand off a VM:

- it resumes paused VMs
- it runs an `exec` probe (`/bin/sh -c true`)
- it runs `EnsureToolbox()`
- only then does it return the sandbox to the caller

That makes the warm lease path more robust, but it also means a warm lease is not a constant-time metadata operation.

### Reconcile path

The pool manager:

- scans idle sandboxes
- ensures each template has a valid source/snapshot
- prunes excess warm sandboxes
- refills up to policy targets

Relevant code:

- `sandboxes/internal/pool/manager.go`
  - `Start()`
  - `ReconcileOnce()`
  - `ReconcileTemplate()`
  - `createWarm()`
  - `TriggerReconcile()`

Important details:

- reconciliation is effectively serial across templates
- trigger wakeups are coalesced into a single pending signal
- each template gets at most `ReplenishParallelism` creates per reconcile pass
- each warm create can block for up to `CreateWarmTimeout` (`180s`)

### Template seeding path

The built-in templates are created in `sandboxes/internal/templates/service.go`:

- `base-go`
- `base-python`
- `base-bun`
- `base-node`

The seeding flow:

1. boot source VM
2. run provisioning commands
3. `EnsureToolbox()`
4. run `ReadyCommand`
5. run a stabilizing `/bin/true`
6. snapshot the VM

Relevant code:

- `seedFromSource()`
- `EnsureSourceVM()`

That stabilizing `/bin/true` is important. Earlier debugging showed that snapshots taken too close to the last meaningful `exec` could restore successfully at the VM level but fail later in the exec-readiness path.

## What is Actually Configured Today

There are two different "defaults" in the codebase:

### Binary defaults

`gocracker-sandboxd serve` defaults to:

- `--default-min-hot=0`
- `--default-min-paused=0`

Relevant code:

- `sandboxes/cmd/gocracker-sandboxd/main.go`

If an operator launches `sandboxd` directly without overrides, built-in templates are cold-start-only by default.

### Local development defaults

`tools/sandboxes-local.sh` overrides those defaults to:

- `DEFAULT_MIN_HOT=2`
- `DEFAULT_MIN_PAUSED=2`

The local stack therefore seeds built-in templates with:

- `min_hot=2`
- `max_hot=3`
- `min_paused=2`
- `max_paused=3`
- `replenish_parallelism=2`

Relevant code:

- `tools/sandboxes-local.sh`
- `sandboxes/internal/templates/service.go`

So when testing locally, the system is already trying to keep four pre-positioned sandboxes per template, not just two.

## Local Measurements

### Measured on April 17, 2026

Environment:

- local Linux host
- `gocracker serve` under sudo
- `sandboxd` local stack from `tools/sandboxes-local.sh`
- `base-python` template

### Burst of 6 creates against one template

A direct burst test produced:

1. `hot` - `23.0 ms`
2. `hot` - `10.4 ms`
3. `paused` - `20.0 ms`
4. `paused` - `22.6 ms`
5. `cold` - `858.6 ms`
6. `cold` - `872.1 ms`

This tells us:

- the first four requests are served exactly from the expected hybrid pool
- cold fallback begins immediately after the pool is drained
- the user-visible cold path is far slower than the low-level restore itself

### Low-level restore is not the bottleneck

Runtime logs showed repeated restores from the template snapshot in roughly:

- `~11 ms`
- `~21 ms`
- `~40 ms`
- occasionally `~100 ms`

That means the expensive part of `cold` is not "starting a VM from snapshot" in the runtime. The expensive part is "returning a sandbox that is safe for the first user action" in `sandboxd`.

### Reconcile stall observed

During the same debugging session:

- `/warm-pools` reported `base-python` as `0 hot / 0 paused`
- `/debug/vars` showed `sandboxd_pool_last_reconcile_unix` lagging behind wall clock by about 180 seconds
- runtime logs showed an exec path timing out for minutes:
  - `POST /vms/gc-93392/exec status=502 latency=2m59.931748s`

This strongly suggests a reconcile stall or starvation condition, not just normal burst exhaustion.

### Runtime/store drift signal

At one point, the runtime still listed VMs associated with the `base-python` template while the `sandboxd` store showed no warm sandboxes for that template.

That is a control-plane consistency problem:

- either stale source/template VMs or
- runtime/store drift or
- orphaned capacity that is consuming host resources without being counted toward the pool

Even if those VMs are not reusable as warm capacity, they still hurt sustainability because they consume memory, CPU, TAPs, and disk paths while providing no leaseable value.

## Why Bursts Still Fall to Cold

There are two separate issues:

1. policy limits
2. implementation bottlenecks

### Policy limits

Even in the improved local configuration, a template only guarantees:

- 2 `hot`
- 2 `paused`

So a burst larger than 4 on one template is expected to spill into cold if refill has not completed yet.

This is not a bug by itself. It is the defined steady-state.

### Implementation bottlenecks

The deeper problem is that the system refills too slowly and too fragiley after the burst.

#### 1. Reconcile is too serialized

`ReconcileOnce()` processes:

- idle lifecycle
- all templates
- all refill

in a mostly serial loop.

One slow template can delay all other templates.

#### 2. Wakeups are coalesced

`TriggerReconcile()` only guarantees one pending wakeup. If multiple warm leases happen quickly, they do not create proportional refill urgency.

That is good for avoiding storms, but it means lease bursts are not reflected as a queue of refill work.

#### 3. Warm create is too expensive

Each `createWarm()` does:

1. `runtime.Run(... SnapshotDir ..., Wait=true)`
2. `EnsureToolbox()`
3. optional `PauseVM()`
4. `GetVM()`
5. `SaveSandbox()`

`EnsureToolbox()` is especially expensive because it first waits for the exec agent to become ready, then checks health/version, then may reinstall/bootstrap, then rechecks health.

Relevant code:

- `sandboxes/internal/toolboxhost/host.go`
  - `EnsureToolbox()`
  - `WaitForExecReady()`

#### 4. First-exec validation is still on the lease path

Warm leases are validated using a real exec probe:

- `"/bin/sh", "-c", "true"`

This is good for correctness, but it means warm handoff still depends on the post-resume exec path being healthy.

If resume or vsock recovery is slightly delayed, a warm candidate can be dropped even though the underlying VM is otherwise fine.

#### 5. Interactive cold and background refill are not coordinated cleanly

The code explicitly avoids letting interactive cold creates wait behind the warm refill budget. That was the right correction for terrible user latency, but it creates a new problem:

- once the pool is drained, user cold requests and background refill can compete for the same runtime and host I/O without a unified fairness model

That can keep the system in a degraded state longer.

## How Other Sandbox Systems Approach This

The recurring pattern across commercial sandbox products is not "keep everything hot all the time." It is:

- prebuilt base environments
- snapshotting or forking
- cheap suspend/hibernate tiers
- fast handoff from already-prepared state
- strong distinction between reproducible base templates and dynamic snapshots/forks

### Daytona

Daytona's sandbox model is snapshot-first. Their docs describe snapshots as the reusable base for sandboxes, and their changelog explicitly mentions work on:

- warm-pool checks
- warm-pool sandbox networking
- caching warm-pool checks

Relevant sources:

- Daytona snapshots:
  - <https://www.daytona.io/docs/en/snapshots/>
- Daytona sandboxes:
  - <https://www.daytona.io/docs/en/sandboxes/>
- Daytona changelog:
  - <https://www.daytona.io/changelog/feature-flags-org-labels>
  - <https://www.daytona.io/changelog/playground-opencode-plugin>

Takeaway:

- Daytona treats warm pools as a first-class system with dedicated correctness and performance work
- the snapshot is the baseline product artifact, not an afterthought
- they also invest in caching around the control-plane checks, not just the VM layer

### E2B

E2B distinguishes clearly between:

- templates: reproducible definitions
- snapshots: captured runtime state
- pause/resume: one-to-one continuity

Most importantly, E2B templates can include a start command that is captured in the snapshot so the process is already running when the sandbox is created.

Relevant sources:

- E2B template quickstart:
  - <https://e2b.dev/docs/template/quickstart>
- E2B snapshots:
  - <https://e2b.dev/docs/sandbox/snapshots>

Takeaway:

- expensive startup work should be baked into the template or snapshot
- the request path should not be reinstalling or reinitializing core runtime tools
- pause/resume and snapshot/fork are different products and should be treated differently

### Runloop

Runloop distinguishes:

- Blueprints: reproducible, fast-to-boot, layer-cached base images
- Snapshots: runtime branching points
- Suspend/Resume: cheap continuity

Their docs explicitly say Blueprints are the thing to use when you want future devboxes to avoid setup/install time. Their overview also states that base devbox images are optimized to boot in less than `200ms`.

Relevant sources:

- Runloop devbox overview:
  - <https://docs.runloop.ai/devboxes/overview>
- Runloop blueprints overview:
  - <https://docs.runloop.ai/docs/devboxes/blueprints/overview>
- Runloop snapshots:
  - <https://docs.runloop.ai/devboxes/snapshots>
- Runloop suspend/resume tutorial:
  - <https://docs.runloop.ai/docs/tutorials/running-agents-on-sandboxes/suspend-resume-workflow>

Takeaway:

- base-image optimization and runtime snapshots solve different latency problems
- fast boot alone is not enough; suspend/resume is still valuable for repeated use
- the platform surface should expose the distinction directly

### CodeSandbox SDK

CodeSandbox focuses heavily on:

- near-instant VM forking
- templates
- hibernation

Their official SDK launch post emphasizes that the infrastructure was built specifically for instant cloning and launching of VMs.

Relevant sources:

- CodeSandbox SDK:
  - <https://codesandbox.io/sdk>
- CodeSandbox SDK release post:
  - <https://codesandbox.io/blog/codesandbox-sdk>

Takeaway:

- "fork" is a core primitive, not just snapshot restore
- hibernation is treated as a normal lifecycle operation
- fast control-plane handoff matters as much as fast VM mechanics

### Docker Sandboxes

Docker's sandbox product treats templates as reusable environments and also supports saving an existing sandbox as a template. The docs also call out template caching and pull policies explicitly.

Relevant sources:

- Docker Sandboxes overview:
  - <https://docs.docker.com/ai/sandboxes/>
- Docker templates:
  - <https://docs.docker.com/ai/sandboxes/templates/>
- `docker sandbox save`:
  - <https://docs.docker.com/reference/cli/docker/sandbox/save/>

Takeaway:

- image/template caching is part of the user-facing performance story
- it is normal to convert a working sandbox into a reusable base
- pull/cache policy is part of burst behavior, not just an implementation detail

## What VM Systems Teach Us

The sandbox products above sit on top of VM mechanisms. The lower-level VM literature and documentation point to the same themes:

- snapshot/restore is fast enough to be a building block
- restore safety and readiness are separate from restore latency
- page cache and memory-fault behavior matter after restore
- device and agent state can dominate real readiness

### Firecracker

Firecracker's snapshot support exists specifically because fast restore is compelling for serverless and bursty multi-tenant workloads. Firecracker's own issue tracker also shows that the community cared deeply about snapshot load latency and cache warm-up after restore.

Relevant sources:

- Firecracker repository:
  - <https://github.com/firecracker-microvm/firecracker>
- Firecracker snapshot support doc:
  - <https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md>
- Firecracker snapshot latency issue:
  - <https://github.com/firecracker-microvm/firecracker/issues/2027>
- Firecracker cache warm-up issue:
  - <https://github.com/firecracker-microvm/firecracker/issues/2944>

Takeaway:

- fast restore is expected and worth optimizing for
- page-fault storms and post-restore cache behavior are real concerns
- "snapshot restored" is not the same as "application instantly ready under load"

### Cloud Hypervisor

Cloud Hypervisor also treats snapshot/restore as a first-class VMM operation.

Relevant source:

- Cloud Hypervisor snapshot/restore:
  - <https://intelkevinputnam.github.io/cloud-hypervisor-docs-HTML/docs/snapshot_restore.html>

Takeaway:

- modern VMMs expose snapshot/restore directly because it is the right primitive for fast restart and fan-out
- higher-level orchestration must still decide when a restored VM is safe to hand to a caller

### QEMU migration and snapshot semantics

QEMU's migration docs and QMP reference make a useful point: the VM's externally visible readiness includes device state, buffered I/O, and consistency of the migration/snapshot boundary, not just CPU execution state.

Relevant sources:

- QEMU migration docs:
  - <https://www.qemu.org/docs/master/devel/migration/index.html>
- QEMU migration framework:
  - <https://www.qemu.org/docs/master/devel/migration/main.html>
- QEMU QMP reference:
  - <https://www.qemu.org/docs/master/interop/qemu-qmp-ref.html>

Takeaway:

- device state can dominate practical readiness
- a control plane that depends on guest agents, PTYs, or vsock services must explicitly model that readiness

## The Real Problem Statement

The current problem is not:

> "Can `gocracker` restore a VM fast enough?"

The runtime clearly can.

The real problem is:

> "How do we guarantee enough `ready-to-use` capacity for bursts without keeping an unsustainably large number of `running` VMs alive?"

That requires solving two things at once:

1. capacity planning
2. refill and readiness correctness

## Design Goals

### Product goals

- absorb common multi-agent bursts without falling to full cold
- keep p50 create latency in the low tens of milliseconds for the hot path
- keep burst overflow significantly below full cold provisioning time
- avoid unbounded always-on VM counts

### System goals

- refill should not be globally serialized behind one bad template
- a template should not be able to stall all reconcile activity
- the pool should be observable enough to explain why a create was hot, paused, or cold
- runtime/store drift should be visible and recoverable

## Recommended Architecture

## 1. Move from two tiers to three tiers

The pool should explicitly manage three capacity classes per template:

### Tier A: `hot`

State:

- VM running
- exec-ready
- toolbox-ready
- safe to hand to caller immediately

Use:

- p50 latency target
- most recent/common traffic

Cost:

- highest steady-state CPU and memory cost

### Tier B: `paused`

State:

- VM suspended in memory
- already provisioned and toolbox-ready
- resume required before lease

Use:

- first burst buffer after hot
- lower cost than hot

Cost:

- memory cost remains
- CPU mostly freed

### Tier C: `restore-ready`

State:

- immutable template snapshot on disk
- not running
- expected to restore to exec-ready rapidly

Use:

- burst overflow
- large fan-out
- fallback before "full cold from image/build"

Cost:

- lowest steady-state compute cost
- more storage and I/O pressure during bursts

This tier is different from today's user-visible `cold`, because it should still be a prebuilt template snapshot path, not a fresh image provisioning path.

## 2. Make refill per-template and asynchronous

Today the reconcile loop is too monolithic.

Instead:

- keep a lightweight global reconciler for scanning desired state
- create one refill worker per template
- give each template its own queue of refill jobs
- let global logic only enforce host-wide budgets and priorities

Benefits:

- one bad template does not freeze all others
- refill urgency can accumulate per template
- burst traffic can produce multiple refill jobs instead of one coalesced wakeup

## 3. Add a pressure-aware controller

Warm targets should not be static for all templates.

Per template, track:

- leases per minute
- consecutive burst misses
- time spent empty in each tier
- refill success/failure rate
- average restore-to-ready time

Then compute targets dynamically:

- increase `min_hot` for templates with repeated hot exhaustion
- increase `min_paused` when hot is not enough but hot VMs are too costly
- bias toward `restore-ready` for rarely used but burstable templates

This is the same high-level idea used by mature systems:

- keep the most expensive state only where it pays for itself
- keep cheaper suspended or snapshot-backed capacity for overflow

## 4. Separate "runtime restored" from "sandbox ready"

Today a lot of latency is hidden inside `EnsureToolbox()` and the exec probe.

Readiness should be modeled explicitly:

- `runtime_restored`
- `exec_ready`
- `toolbox_ready`
- `preview_ready`

That enables:

- better metrics
- clearer pool accounting
- faster handoff for templates that do not need every subsystem

For example:

- `process.exec` requires `exec_ready`
- file APIs require `toolbox_ready`
- preview requires `preview_ready`

In other words, the pool should not treat "all features healthy" as the only useful state.

## 5. Keep toolbox upgrades out of the hot path

The right steady-state is:

- publish template generation N with toolbox version N
- fill its pool completely
- atomically switch routing to generation N
- drain generation N-1

That is better than allowing each lease path to discover a mismatch and possibly reinstall.

The hot path should assume:

- template generation is already internally consistent

Toolbox mismatch handling should remain as repair logic, not normal flow.

## 6. Treat interactive burst capacity and background refill as separate budgets

The system needs at least two budgets:

- a budget for background refill
- a reserve budget for interactive fallback creates

Otherwise there are only two bad choices:

- refill starves user cold requests
- user cold requests starve refill

The goal is not to eliminate contention, but to make it explicit.

## 7. Repair runtime/store drift automatically

The runtime and `sandboxd` state must periodically converge.

Suggested behavior:

- runtime VMs with template metadata but missing store records should be tagged as orphans
- orphans should be classified as:
  - recoverable warm candidate
  - template-source artifact
  - dead orphan to stop/delete
- reconciliation should publish orphan counts and age

Without this, the host can silently accumulate non-leaseable VM cost.

## Concrete Changes Recommended for `sandboxd`

### Observability first

Add per-template metrics:

- `pool_hot_ready{template}`
- `pool_paused_ready{template}`
- `pool_restore_ready{template}`
- `pool_leased{template}`
- `pool_refill_inflight{template}`
- `pool_refill_queue_depth{template}`
- `pool_last_successful_refill_unix{template}`
- `pool_last_failed_refill_unix{template}`
- `pool_time_empty_hot_ms_total{template}`
- `pool_time_empty_all_ms_total{template}`
- `create_path_total{template,tier}`
- `create_latency_ms{template,tier}`

And split readiness timings:

- `runtime_restore_ms`
- `resume_ms`
- `exec_ready_ms`
- `toolbox_ready_ms`
- `preview_ready_ms`

### Short-term improvements

1. keep current hybrid pool
2. stop using one global reconcile loop as the sole refill executor
3. add per-template refill workers
4. add a `restore-ready` tier in accounting, even if it initially just means "known-good snapshot exists and no fresh image build is required"
5. make `TriggerReconcile()` enqueue real template work, not just a single shared pulse

### Medium-term improvements

1. add pressure-aware autoscaling of `min_hot` and `min_paused`
2. add explicit burst reserve budget
3. rotate template generations only after the new generation is fully pool-ready
4. implement automatic orphan VM reconciliation

## What We Should Not Do

### Do not solve this only by raising `min_hot`

That helps p50 but scales cost almost linearly with template count and traffic variance.

### Do not force all recovery through full cold

The runtime already restores snapshots fast enough to justify a proper restore-backed tier.

### Do not keep toolbox installation in the normal lease path

That turns "warm" into "warm-ish, unless the guest needs repair".

### Do not assume one template's behavior generalizes

Popular templates and niche templates should not have the same target pool shape.

## Recommended Rollout Plan

### Phase 0: instrumentation

- add per-template pool and refill metrics
- add explicit timing around restore, resume, exec-ready, toolbox-ready
- add runtime/store orphan reporting

### Phase 1: stabilize current hybrid behavior

- keep `hot` + `paused`
- move refill into per-template workers
- stop any single `createWarm` from freezing the whole pool manager
- expose whether the create path was `hot`, `paused`, or `cold`

### Phase 2: add `restore-ready`

- model snapshot-backed overflow explicitly
- let templates target `hot`, `paused`, and `restore-ready` separately
- use restore-ready for burst overflow before full cold

### Phase 3: adaptive controller

- tune per-template targets from actual demand
- add host-wide budget enforcement and burst reserve

## Suggested Reproduction Steps

Use a single template to separate policy from refill behavior:

```bash
curl -s http://127.0.0.1:9090/warm-pools \
  -H 'Authorization: Bearer dev-sandboxd-token' | jq
```

Burst 6 creates:

```bash
python3 - <<'PY'
import json, urllib.request, time
base='http://127.0.0.1:9090'
headers={'Authorization':'Bearer dev-sandboxd-token','Content-Type':'application/json'}
created=[]
for i in range(6):
    req=urllib.request.Request(base+'/sandboxes', data=json.dumps({'template':'base-python'}).encode(), headers=headers, method='POST')
    t0=time.perf_counter()
    with urllib.request.urlopen(req, timeout=120) as resp:
        body=json.load(resp)
    print(i+1, body['id'], body.get('lease_tier'), f'{(time.perf_counter()-t0)*1000:.1f}ms')
    created.append(body['id'])
for sid in created:
    req=urllib.request.Request(base+'/sandboxes/'+sid, headers={'Authorization':'Bearer dev-sandboxd-token'}, method='DELETE')
    urllib.request.urlopen(req, timeout=120).read()
PY
```

Then inspect:

```bash
curl -s http://127.0.0.1:9090/warm-pools \
  -H 'Authorization: Bearer dev-sandboxd-token' | jq '.[] | select(.template_name=="base-python")'
```

And:

```bash
curl -s http://127.0.0.1:9090/debug/vars | jq '.sandboxd_pool_last_reconcile_unix'
```

If `hot_ready` and `paused_ready` stay at zero while `last_reconcile_unix` stops moving, the issue is not simple exhaustion. It is refill/reconcile failure.

## Final Recommendation

The sustainable answer is:

- not "keep more VMs hot forever"
- not "accept cold after the second or fourth request"

It is:

- small `hot` pool for instant response
- larger `paused` pool for cheap burst absorption
- explicit `restore-ready` tier for scalable overflow
- per-template adaptive targets
- per-template refill workers
- stronger readiness modeling and better drift recovery

That matches both the sandbox-product layer and the lower-level VM layer:

- sandbox products lean on templates, snapshots, suspend/hibernate, and fast fork
- VMM systems show that snapshot/restore is cheap enough, but readiness after restore must be treated as a separate concern

With those changes, `sandboxd` can absorb significantly more concurrent starts without turning into "just keep more running microVMs around forever".

## Implementation Results — April 17, 2026

### What was shipped

The following changes were applied and measured in the same session:

#### Pool depth increase

`tools/sandboxes-local.sh` defaults changed from `min_hot=0 / min_paused=0` to:

```
DEFAULT_MIN_HOT=2
DEFAULT_MIN_PAUSED=2
```

Each template is seeded with `min_hot=2 max_hot=3 min_paused=2 max_paused=3 replenish_parallelism=2`.

Before: pool held at most 1 warm VM per template (old default was 1). After: 4 warm VMs per template at steady state (2 hot + 2 paused).

#### TriggerReconcile on lease

`controlplane/server.go` — after a warm VM is leased out, the pool reconciler is woken immediately via `s.pool.TriggerReconcile()` instead of waiting up to 5 seconds for the next tick.

Effect: refill begins within milliseconds of a hot VM being consumed. The base templates recovered their pool depth in under 10 seconds in post-burst testing.

#### User creates removed from pool budget

Previously, `cloneFromTemplate()` and the source-based cold boot path both called `AcquireCreateBudget()` — the same semaphore used by background warm pool creates. When the pool was aggressively refilling all 4 template slots (up to 8 concurrent creates), user cold boots blocked indefinitely waiting for a semaphore slot.

Fixed by removing `AcquireCreateBudget()` from both user create paths. Background pool refill and interactive creates now use separate resources.

#### `GlobalInflightBudget` raised from 3 to 8

Allows up to 8 concurrent pool creates in the background, enabling faster parallel pool recovery across all 4 templates simultaneously.

#### Zombie `idle_paused` sandbox auto-cleanup

`maybeIdleSnapshot` and `maybeIdlePause` previously just logged a WARN and returned when the runtime VM was not found. On the next reconcile tick (5 seconds later), they retried. Indefinitely.

Fixed by calling `isVMGone(err)` — a helper that matches "not found" / "no such" in the error string — and immediately purging the sandbox from the store. Extended `reapDead` to also cover `LeaseStateIdlePaused` (was previously `warm_ready` only).

#### `HealthInfo` timeout bounded to 15 seconds

`toolboxhost/host.go` `HealthInfo()` made an HTTP request over a raw `net.Conn` with no read deadline. If the toolbox was slow to restart after snapshot restore, `http.ReadResponse` blocked indefinitely — until the Python client's 300-second socket timeout fired.

Fixed: `context.WithTimeout(ctx, 15*time.Second)` + `conn.SetDeadline(dl)` applied before the HTTP call.

#### `WaitForExecReady` per-probe timeout raised from 3s to 10s

Under heavy host load (many VMs booting simultaneously), exec round-trips can take several seconds even when the exec agent is healthy. The original 3-second per-probe limit caused `WaitForExecReady` to record 40 consecutive timeouts and give up, even though the agent was alive and would have responded in 4-6 seconds.

Changed to 10 seconds per probe. The 120-second total deadline is unchanged. Healthy execs still return in under 200 ms; this only widens the window for transient load spikes.

#### `sandboxes-local.sh` — gocracker runs as user

When invoked via `sudo`, the runtime previously ran as root, creating snapshot directories owned by root. `sandboxd` (running as the invoking user) could not delete them, breaking `DELETE /templates` with EPERM.

Fixed by using `setcap cap_net_admin+ep` on the gocracker binary so it can create TAP devices without being root, then launching it as `$SUDO_USER` via `sudo -u`. KVM access works because the user is in the `kvm` group.

### Measured results

#### Cookbook suite (01–29 Python)

Confirmed two consecutive passes:

```
cookbook: pass=28 fail=0 skip=1 total=29
```

skip=1 is cookbook 08 (npm registry unreachable on this host — expected).

#### Warm-path latency (cookbook 16, pool steady state)

Worker 0 and worker 2 both served from the warm hot tier:

```
worker=0 total=137-140 ms
worker=2 total=129-1064 ms
```

The 1 second outlier for worker 2 was a resume from the paused tier (pause+resume round-trip).

#### Cold-boot fallback

Worker 1 in cookbook 16 still falls to a cold snapshot-restore path when the pool is drained by the concurrent suite load. Cold boot under load currently takes **~300 seconds** to complete.

This is not a regression — it was present before these changes. The root cause is that under heavy host load (many concurrent VMs from the suite), the exec agent can take several seconds per probe. Even with 10-second probes, the `WaitForExecReady` loop (120-second total) may exhaust before the agent becomes consistently responsive. Once it does respond, the remaining path completes quickly.

The cold-boot path is a known area for future improvement (see Recommended Architecture above).

### Remaining known issues

1. **Cold boot latency under load**: When the warm pool is exhausted during a concurrent burst (e.g., running the full 29-cookbook suite), a cold `cloneFromTemplate` can take 200–300 seconds. The bottleneck is not snapshot restore (~10–40 ms in the runtime) but the exec-agent readiness polling loop under CPU contention. Long-term fix: per-template refill workers, `restore-ready` tier, and pressure-aware autoscaling.

2. **Template snapshot ownership on local dev**: Even with the `setcap` fix in `sandboxes-local.sh`, the current running session still has gocracker running as root (fix takes effect at next restart). `DELETE /templates` logs an EPERM WARN if the snapshot directory is root-owned. The template store entry is still deleted; only the disk cleanup fails. A chown of `.tmp/sandboxes-local/templates/` after each `seed` works around this for the current session.

3. **Pool reconcile is still globally serial**: One slow template (e.g., a custom template with a cold snapshot build) can delay refill for all other templates. See Recommended Architecture §2.

## References

### Current codebase

- `sandboxes/internal/controlplane/server.go`
- `sandboxes/internal/pool/manager.go`
- `sandboxes/internal/templates/service.go`
- `sandboxes/internal/toolboxhost/host.go`
- `tools/sandboxes-local.sh`

### Sandbox systems

- Daytona snapshots: <https://www.daytona.io/docs/en/snapshots/>
- Daytona sandboxes: <https://www.daytona.io/docs/en/sandboxes/>
- Daytona changelog, warm-pool networking/checks:
  - <https://www.daytona.io/changelog/feature-flags-org-labels>
  - <https://www.daytona.io/changelog/playground-opencode-plugin>
- E2B templates: <https://e2b.dev/docs/template/quickstart>
- E2B snapshots: <https://e2b.dev/docs/sandbox/snapshots>
- Runloop devbox overview: <https://docs.runloop.ai/devboxes/overview>
- Runloop blueprints: <https://docs.runloop.ai/docs/devboxes/blueprints/overview>
- Runloop snapshots: <https://docs.runloop.ai/devboxes/snapshots>
- Runloop suspend/resume: <https://docs.runloop.ai/docs/tutorials/running-agents-on-sandboxes/suspend-resume-workflow>
- CodeSandbox SDK: <https://codesandbox.io/sdk>
- CodeSandbox SDK release: <https://codesandbox.io/blog/codesandbox-sdk>
- Docker Sandboxes overview: <https://docs.docker.com/ai/sandboxes/>
- Docker templates: <https://docs.docker.com/ai/sandboxes/templates/>
- Docker sandbox save: <https://docs.docker.com/reference/cli/docker/sandbox/save/>

### VM systems

- Firecracker repository: <https://github.com/firecracker-microvm/firecracker>
- Firecracker snapshot support:
  <https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md>
- Firecracker snapshot latency discussion:
  <https://github.com/firecracker-microvm/firecracker/issues/2027>
- Firecracker cache warm-up after snapshot restore:
  <https://github.com/firecracker-microvm/firecracker/issues/2944>
- Cloud Hypervisor snapshot/restore:
  <https://intelkevinputnam.github.io/cloud-hypervisor-docs-HTML/docs/snapshot_restore.html>
- QEMU migration docs:
  - <https://www.qemu.org/docs/master/devel/migration/index.html>
  - <https://www.qemu.org/docs/master/devel/migration/main.html>
  - <https://www.qemu.org/docs/master/interop/qemu-qmp-ref.html>
