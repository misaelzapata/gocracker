# gocracker-sandboxd

HTTP control plane for `gocracker` sandboxes. Wraps the runtime with
warm pools, content-addressed templates, HMAC-signed preview URLs,
and a per-sandbox Firecracker-style UDS that speaks directly to the
baked-in toolbox agent on `vsock 10023`.

## Quick start

```bash
sudo ./gocracker-sandboxd serve \
  --addr 127.0.0.1:9091 \
  --state-dir /var/lib/gocracker-sandboxd \
  --kernel-path ./artifacts/kernels/gocracker-guest-standard-vmlinux
```

Create a sandbox (cold boot ~80 ms on cached image, ~40 ms on warm restore):

```bash
SB=$(curl -sS http://127.0.0.1:9091/sandboxes -X POST \
  -H 'Content-Type: application/json' \
  -d '{"image":"alpine:3.20","network_mode":"auto","cmd":["sleep","3600"]}' | jq -r .sandbox.id)

UDS=$(curl -s http://127.0.0.1:9091/sandboxes/$SB | jq -r .uds_path)
./toolbox-cli exec -uds $UDS -- sh -c 'echo hi'
curl -sS -X DELETE http://127.0.0.1:9091/sandboxes/$SB
```

## SDKs

Most users reach this daemon through one of the typed SDKs rather
than raw HTTP:

- [Python SDK](../../sdk/python/) — `pip install gocracker-sandboxes`
- [Go SDK](../../sdk/go/)
- [JS / Node SDK](../../sdk/js/)

All three expose the same Daytona-style surface (`sandbox.create()`
/ `runCommand()` / `recycle()` / context-manager lifecycle) with
typed errors and pipelined CONNECT.

## Reference

- **Daemon overview, perf numbers, bench reproduction**: [Sandboxd section in the main README](../../../README.md#sandboxd-sandbox-control-plane).
- **HTTP example end-to-end**: [Example #10 in the main README](../../../README.md#10-sandbox-control-plane--raw-http).
- **Cookbook (Python)**: [sandboxes/examples/python/cookbook/](../../examples/python/cookbook/).
- **Bench harnesses**: [Python](../../examples/python/bench/), [Go](../../examples/go/bench/), [JS](../../examples/js/bench/).

## Routes

```
GET    /healthz
POST   /sandboxes              — create cold-boot sandbox
GET    /sandboxes              — list
GET    /sandboxes/{id}         — fetch one
POST   /sandboxes/{id}/recycle — return-to-pool semantics
DELETE /sandboxes/{id}         — stop + remove
POST   /pools                  — register a warm pool
POST   /sandboxes/lease        — lease from a warm pool
POST   /templates              — register a Spec'd template
GET    /templates              — list
GET    /templates/{id}
DELETE /templates/{id}
POST   /preview/mint           — HMAC-signed preview URL
GET    /preview/serve          — proxy to in-sandbox HTTP
```
