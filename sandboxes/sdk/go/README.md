# gocracker Go SDK

Stdlib-only client for the gocracker sandboxd HTTP control plane +
the in-guest toolbox agent over UDS. No third-party dependencies.

## Install

```go
import gocracker "github.com/gocracker/gocracker/sandboxes/sdk/go"
```

## Quick start

```go
ctx := context.Background()
client := gocracker.NewClient("http://127.0.0.1:9091")

sb, err := client.CreateSandbox(ctx, gocracker.CreateSandboxRequest{
    Image:      "alpine:3.20",
    KernelPath: "/abs/path/to/vmlinux",
})
if err != nil { log.Fatal(err) }
defer sb.Delete(ctx)

result, err := sb.Toolbox().Exec(ctx, []string{"echo", "hello"})
if err != nil { log.Fatal(err) }
fmt.Println(string(result.Stdout))  // "hello\n"
```

## Surface

Mirrors Python + JS SDKs. Same endpoint + error shape.

**Control plane** (`client.`):
- `CreateSandbox` / `ListSandboxes` / `GetSandbox` / `Delete`
- `RegisterPool` / `ListPools` / `UnregisterPool` / `LeaseSandbox`
- `CreateTemplate` / `ListTemplates` / `GetTemplate` / `DeleteTemplate`
- `MintPreview`
- `Healthz`

**Toolbox** (`sb.Toolbox()`):
- `Health` · `Exec` · `ExecStream` (returns a channel of Frames)
- `ListFiles` · `Download` · `Upload` · `DeleteFile`
- `Mkdir` · `Rename` · `Chmod`
- `GitClone` · `GitStatus`
- `SetSecret` · `ListSecrets` · `DeleteSecret`

**Errors**: concrete type `*Error` with `.Status` and `.Body`;
sentinel values for `errors.Is` matching:

```go
_, err := client.GetTemplate(ctx, "missing")
if errors.Is(err, gocracker.ErrTemplateNotFound) { ... }
```

Sentinels: `ErrSandboxNotFound`, `ErrTemplateNotFound`,
`ErrPoolNotFound`, `ErrInvalidRequest`, `ErrConflict`.
