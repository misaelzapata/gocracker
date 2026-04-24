# Manual Smoke Pack

Version en espanol. La version principal en ingles vive en
[`README.md`](README.md).

Este folder deja una validacion manual reproducible para `gocracker` sin depender de Docker Engine para correr las VMs.

Los scripts viven en el repo, pero los logs y artefactos se escriben en:

```bash
/tmp/gocracker-manual-smoke/<timestamp>
```

## Prerrequisitos

- Linux con `/dev/kvm`
- `GOCRACKER_KERNEL` apuntando a un guest kernel valido. Recomendado:
  - `./tools/build-guest-kernel.sh`
  - `GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux`
- `sudo -v` ya ejecutado antes de correr el pack
- `debugfs`
- `script`
- `timeout`
- `curl`
- `ip`
- `pgrep`
- red saliente para pulls de registry

## Verificacion Recomendada

Primero corre la base automatizada:

```bash
./tools/build-guest-kernel.sh
go test ./...
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux go test -tags integration ./tests/integration/...
```

Luego corre la matriz manual:

```bash
sudo -v
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux tests/manual-smoke/run_all.sh
```

El script:

- compila `./gocracker`
- valida prerequisitos
- ejecuta la matriz por grupos
- deja un log por caso
- termina con exit code `0` solo si todo pasa

## Casos Disponibles

- `images`: `alpine:latest`, `busybox:latest`, `ubuntu:22.04`, `nginx:latest`, override con `python:3.11-slim`
- `dockerfiles`: `tests/examples/hello-world`, `tests/examples/static-site`, `tests/examples/python-api`
- `extras`: fixture shell-form y fixture `USER`
- `compose`: fixture con `ports`, fixture con `depends_on: service_healthy`, fixture con bind volume + sync-back, y fixture TODO + PostgreSQL
- `exec`: `compose exec` directo sobre `serve`, sin keys ni usuario

Por defecto corre todo. Para limitar grupos:

```bash
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
SMOKE_CASES=images,dockerfiles \
tests/manual-smoke/run_all.sh
```

Variables utiles:

```bash
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux
GC_BIN=./gocracker
SMOKE_LOG_DIR=/tmp/gocracker-manual-smoke/custom-run
SMOKE_CASES=all
```

## Auth De Registry

### Sin login

La matriz publica normal funciona sin login. Ese es el camino por defecto.

### Login opcional

Solo hace falta login para imagenes privadas o si pegas rate limits del registry.

`gocracker` no necesita Docker Engine para correr, pero el camino mas simple para dejar credenciales compatibles es escribir un `config.json` estilo Docker:

```bash
docker login
```

Si no quieres usar `docker login`, puedes poblar manualmente `~/.docker/config.json` con las credenciales del registry.

### Nota importante con `sudo`

Los casos del smoke pack usan `sudo ./gocracker ...`. Eso significa que, si hace falta auth, las credenciales tambien deben existir para root.

La forma mas directa es:

```bash
sudo docker login
```

o dejar el archivo en:

```bash
/root/.docker/config.json
```

## Revisar Fallos

Cada caso deja:

- log serial: `/tmp/gocracker-manual-smoke/<timestamp>/<case>.log`
- la ruta real del `disk.ext4` reportada por `gocracker run`

Los logs quedan en:

```bash
/tmp/gocracker-manual-smoke/<timestamp>
```

El disco real ya no debe asumirse como `/tmp/gocracker-<case-id>/disk.ext4`. Se resuelve desde el log del caso:

```bash
log=/tmp/gocracker-manual-smoke/<timestamp>/<case-id>.log
disk=$(grep -o '/tmp/gocracker-[^[:space:]]*/disk.ext4' "$log" | tail -n1)
echo "$disk"
```

Para inspeccionar un archivo dentro del disco:

```bash
debugfs -R "cat /result.txt" "$disk"
debugfs -R "stat /work/runtime-user.txt" "$disk"
```

Para revisar rapido un log:

```bash
tail -n 80 /tmp/gocracker-manual-smoke/<timestamp>/<case-id>.log
```

Los fixtures extras viven en:

- `tests/manual-smoke/fixtures/shellform/Dockerfile`
- `tests/manual-smoke/fixtures/user/Dockerfile`
- `tests/manual-smoke/fixtures/compose-basic/docker-compose.yml`
- `tests/manual-smoke/fixtures/compose-health/docker-compose.yml`
- `tests/manual-smoke/fixtures/compose-volume/docker-compose.yml`
- `tests/manual-smoke/fixtures/compose-todo-postgres/docker-compose.yml`
- `tests/manual-smoke/fixtures/compose-exec/docker-compose.yml`

## Cobertura Compose

El grupo `compose` valida tres cosas practicas:

- `compose-basic`: levanta un servicio HTTP, publica `18080:8080` y verifica la respuesta con `curl`
- `compose-health`: usa `depends_on.condition: service_healthy` y no da por levantado el stack hasta que el servicio web responde
- `compose-volume`: monta `./data:/data`, detiene el stack limpiamente y valida que el archivo sincronizado vuelva al host
- `compose-todo-postgres`: levanta `postgres` + una app TODO, crea una tarea real por HTTP y comprueba que se leyó de vuelta desde PostgreSQL

Si quieres correr solo Compose:

```bash
sudo -v
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
SMOKE_CASES=compose \
tests/manual-smoke/run_all.sh
```

Para probar un caso real de app + base de datos fuera del harness completo:

```bash
cd <your-gocracker-checkout>
go build -o ./gocracker ./cmd/gocracker
sudo -v

sudo env GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
  ./gocracker compose \
  --file tests/manual-smoke/fixtures/compose-todo-postgres/docker-compose.yml \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --mem 256 \
  --wait
```

En otra terminal:

```bash
curl -fsS http://127.0.0.1:18081/health
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"title":"buy milk"}' \
  http://127.0.0.1:18081/api/todos
curl -fsS http://127.0.0.1:18081/api/todos
```

Si el ultimo request devuelve el item creado, la app y PostgreSQL se hablaron bien dentro de la red Compose.

Notas operativas nuevas:

- El cache compartido ahora vive por default en `/tmp/gocracker/cache`; si repites una corrida normal deberías ver `artifact cache hit`, `reusing cached disk`, y `reusing cached initrd` sin configuración extra.
- Si quieres que la stack quede visible en la API, arranca `serve` y usa `compose --server http://127.0.0.1:8080`.
- En `compose --server`, el netns/TAP y los published ports ahora quedan del lado de `serve`; el comando cliente puede terminar y los puertos siguen vivos mientras la stack siga arriba.
- Los healthchecks Compose ahora corren dentro del guest via exec-agent, así que `CMD` y `CMD-SHELL` pueden usar binarios y scripts locales del contenedor en vez de depender de traducciones host-side.
- `compose exec --server ...` solo necesita reachability al API server; no necesita IP guest, `ports: 22`, usuario ni claves.
- Ejemplo real con `compose --server` + `compose exec`:

```bash
./gocracker serve --addr :8080 --cache-dir /tmp/gocracker/cache

sudo ./gocracker compose \
  --server http://127.0.0.1:8080 \
  --file tests/manual-smoke/fixtures/compose-exec/docker-compose.yml \
  --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux \
  --cache-dir /tmp/gocracker/cache

curl 'http://127.0.0.1:8080/vms?orchestrator=compose&stack=compose-exec&service=debug'

./gocracker compose exec \
  --server http://127.0.0.1:8080 \
  --file tests/manual-smoke/fixtures/compose-exec/docker-compose.yml \
  debug -- echo compose-exec-ok
```

- Ejemplo real con `/run` + API exec:

```bash
./gocracker serve --addr :8080 --cache-dir /tmp/gocracker/cache

curl -fsS -X POST http://127.0.0.1:8080/run \
  -H 'Content-Type: application/json' \
  -d '{
    "image":"alpine:3.19",
    "kernel_path":"./artifacts/kernels/gocracker-guest-standard-vmlinux",
    "mem_mb":256,
    "cmd":["sleep","600"],
    "exec_enabled":true
  }'

curl -fsS -X POST http://127.0.0.1:8080/vms/<vm-id>/exec \
  -H 'Content-Type: application/json' \
  -d '{"command":["echo","api-exec-ok"]}'
```

Para correr solo estos checks manuales:

```bash
sudo -v
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
SMOKE_CASES=exec \
tests/manual-smoke/run_all.sh
```
