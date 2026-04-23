# gocracker Go SDK

Zero-external-dep Go client for the sandboxd HTTP control plane + the
in-guest toolbox agent over UDS + CONNECT. Stdlib only (`net/http`,
`net`, `encoding/json`).

Warm-lease latency (measured 2026-04-22 on x86, pool of 8):

- `CreateSandbox(Template=base-python)` p95 = **1.5 ms**
- full `Create → Process.Exec("echo") → Delete` p95 = **~35 ms**

## Install

```go
import gocracker "github.com/gocracker/gocracker/sandboxes/sdk/go"
```

## Quick start

Start `gocracker-sandboxd` with `-kernel-path` (or `GOCRACKER_KERNEL`
in env) so `base-python / base-node / base-bun / base-go` auto-register.
Then:

```go
package main

import (
    "context"
    "fmt"
    gocracker "github.com/gocracker/gocracker/sandboxes/sdk/go"
)

func main() {
    ctx := context.Background()
    c := gocracker.NewClient("http://127.0.0.1:9091")

    sb, err := c.CreateSandbox(ctx, gocracker.CreateSandboxRequest{
        Template: "base-python",
    })
    if err != nil { panic(err) }
    defer sb.Delete(ctx)

    res, err := sb.Process().Exec(ctx, `python3 -c "print(2+2)"`)
    if err != nil { panic(err) }
    fmt.Println(string(res.Stdout)) // "4\n"

    if err := sb.FS().WriteFile(ctx, "/tmp/hi.txt", []byte("hello")); err != nil { panic(err) }
    data, _ := sb.FS().ReadFile(ctx, "/tmp/hi.txt")
    fmt.Println(string(data))

    url, _ := sb.PreviewURL(ctx, 8080)
    fmt.Println(url)
}
```

Low-level / escape hatch — `sb.Toolbox()` returns the flat
`ToolboxClient` (same methods v3 originally exposed):

```go
sb, _ := c.CreateSandbox(ctx, gocracker.CreateSandboxRequest{
    Image:      "alpine:3.20",
    KernelPath: "/abs/vmlinux",
})
sb.Toolbox().Exec(ctx, []string{"echo", "hi"})
```

## Surface

### Client

| Method | What |
|---|---|
| `CreateSandbox(ctx, req)` with `Template: "base-python"` | Warm-restore from registered template. |
| `CreateSandbox(ctx, req)` with `Image`+`KernelPath` | Cold-boot. |
| `LeaseSandbox(ctx, req)` | Warm lease (<5 ms) from a pool. |
| `ListSandboxes / GetSandbox / Delete` | Inventory + teardown. |
| `RegisterPool / ListPools / UnregisterPool` | Warm pool. |
| `CreateTemplate / ListTemplates / GetTemplate / DeleteTemplate` | Template lifecycle. |
| `MintPreview(ctx, id, port)` | Raw signed URL (prefer `sb.PreviewURL`). |

### `Sandbox`

| Method | What |
|---|---|
| `sb.Process().Exec(ctx, cmd)` | `cmd` may be `string` or `[]string`; non-zero exit → `*ProcessExitError`. |
| `sb.Process().ExecStream(ctx, cmd)` / `.Start(ctx, cmd)` | Frame channel. |
| `sb.FS().WriteFile / ReadFile / ListDir / Remove / Mkdir / Chmod / Rename` | Canonical file ops. |
| `sb.PreviewURL(ctx, port)` | Absolute signed URL. |
| `sb.Delete(ctx)` | Async teardown. |
| `sb.Toolbox()` | Flat low-level client (escape hatch). |

### Typed errors

```go
Error                 // base, carries HTTP Status + Body
ErrSandboxNotFound    // 404
ErrInvalidRequest     // 400
ErrConflict           // 409
ErrTemplateNotFound   // CreateSandbox(Template=X) where X is unknown
ErrPoolNotFound       // lease against unregistered pool
ErrPoolExhausted      // pool drained
ErrRuntimeUnreachable // sandboxd can't reach gocracker runtime
ErrSandboxTimeout     // operation deadline exceeded
*ProcessExitError     // carries .ExitCode + .Stdout + .Stderr
```

Use `errors.Is(err, gocracker.ErrTemplateNotFound)` to branch.

## SDK parity

- Python: `sandboxes/sdk/python/`
- JS/Node: `sandboxes/sdk/js/`

Same surface (`template=`, `.process`, `.fs`, `preview_url`, typed
errors). Different language idioms: Go uses `defer sb.Delete(ctx)`
where Python/JS use context managers, and methods are functions
rather than properties.
