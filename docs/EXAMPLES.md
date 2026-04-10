# Example Applications

gocracker ships with 16 example applications across 10 languages, plus a
multi-service Compose example. Each example includes a Dockerfile and can be
booted directly as a microVM.

## Quick Start

Any example can be booted with:

```bash
sudo gocracker run \
  --dockerfile ./tests/examples/<name>/Dockerfile \
  --context ./tests/examples/<name> \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --wait \
  --jailer off
```

Or from a pre-built OCI image (where the example uses a public base):

```bash
sudo gocracker run \
  --image python:3.12-slim \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --wait \
  --jailer off
```

## Example Table

| Name | Language / Runtime | Framework | Boot command |
|------|-------------------|-----------|--------------|
| `hello-world` | Go | stdlib net/http | `gocracker run --dockerfile ./tests/examples/hello-world/Dockerfile ...` |
| `fiber-api` | Go | Fiber | `gocracker run --dockerfile ./tests/examples/fiber-api/Dockerfile ...` |
| `express-api` | Node.js | Express | `gocracker run --dockerfile ./tests/examples/express-api/Dockerfile ...` |
| `fastapi-api` | Python | FastAPI | `gocracker run --dockerfile ./tests/examples/fastapi-api/Dockerfile ...` |
| `python-api` | Python | stdlib http.server | `gocracker run --dockerfile ./tests/examples/python-api/Dockerfile ...` |
| `django-blog` | Python | Django + gunicorn | `gocracker run --dockerfile ./tests/examples/django-blog/Dockerfile ...` |
| `sinatra-ruby` | Ruby | Sinatra | `gocracker run --dockerfile ./tests/examples/sinatra-ruby/Dockerfile ...` |
| `actix-rust` | Rust | Actix Web | `gocracker run --dockerfile ./tests/examples/actix-rust/Dockerfile ...` |
| `spring-boot-java` | Java | Spring Boot | `gocracker run --dockerfile ./tests/examples/spring-boot-java/Dockerfile ...` |
| `slim-php` | PHP | Slim | `gocracker run --dockerfile ./tests/examples/slim-php/Dockerfile ...` |
| `plug-elixir` | Elixir | Plug | `gocracker run --dockerfile ./tests/examples/plug-elixir/Dockerfile ...` |
| `deno-oak` | TypeScript (Deno) | Oak | `gocracker run --dockerfile ./tests/examples/deno-oak/Dockerfile ...` |
| `bun-hono` | TypeScript (Bun) | Hono | `gocracker run --dockerfile ./tests/examples/bun-hono/Dockerfile ...` |
| `sqlite-web` | Go | stdlib + SQLite | `gocracker run --dockerfile ./tests/examples/sqlite-web/Dockerfile ...` |
| `static-site` | Alpine (static HTML) | none | `gocracker run --dockerfile ./tests/examples/static-site/Dockerfile ...` |

## Languages Covered

- **Go** -- hello-world, fiber-api, sqlite-web
- **Python** -- python-api, fastapi-api, django-blog
- **Node.js** -- express-api
- **Ruby** -- sinatra-ruby
- **Rust** -- actix-rust
- **Java** -- spring-boot-java
- **PHP** -- slim-php
- **Elixir** -- plug-elixir
- **TypeScript (Deno)** -- deno-oak
- **TypeScript (Bun)** -- bun-hono

## Compose Example: Next.js + PostgreSQL

The `nextjs-compose` example demonstrates a multi-service stack: a Next.js 14
standalone app connected to a PostgreSQL 16 database.

```yaml
# tests/examples/nextjs-compose/docker-compose.yml
services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: nextjs
      POSTGRES_USER: nextjs
      POSTGRES_PASSWORD: nextjs
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U nextjs -d nextjs"]
      interval: 1s
      timeout: 3s
      retries: 20

  web:
    build:
      context: ./web
    depends_on:
      db:
        condition: service_healthy
    environment:
      DATABASE_URL: postgresql://nextjs:nextjs@db:5432/nextjs
    ports:
      - "13000:3000"
```

Boot it:

```bash
sudo gocracker compose \
  --file tests/examples/nextjs-compose/docker-compose.yml \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --wait \
  --jailer off
```

Then probe the health endpoint:

```bash
curl -fsS http://127.0.0.1:13000/api/health
```

Each service runs in its own microVM with isolated kernel, memory, and network.
gocracker creates a virtual network so services can reach each other by name
(e.g. `db:5432` from the web service).

## Adding Your Own

1. Write a `Dockerfile` that produces a runnable image.
2. Boot it: `sudo gocracker run --dockerfile ./Dockerfile --context . --kernel ./vmlinux --wait --jailer off`
3. For multi-service stacks, write a `docker-compose.yml` and use `gocracker compose`.

No Docker daemon is required. gocracker pulls OCI layers, builds with its own
builder, and boots the result as a real Linux VM via KVM.
