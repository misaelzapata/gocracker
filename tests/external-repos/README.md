# External Repo Sweep

Este folder deja un harness reproducible para validar repos externos con `gocracker`. Hay **un único entrypoint**, [`sweep.py`](sweep.py), que corre contra una lista de ids (una por línea). Los wrappers de bash históricos (`run_one.sh`, `run_all.sh`, `run_historical_pass.sh`, `run_historical_tty.sh`, `run_compose_tty.sh`, `run_fixed_50.sh`) + el tool Go `cmd/gocracker-sweep/` se removieron el 2026-04-19 — pasaban `--server <url>` al CLI `gocracker repo`, flag que nunca existió, lo que hacía colapsar toda la matriz a `FAIL exit status 2` por parse error. `sweep.py` corre `gocracker repo` local (sin `--server`), con concurrencia configurable, cleanup per-caso y reintento ante rate-limit de Docker Hub.

Los resultados se escriben fuera del worktree:

```bash
/tmp/gocracker-external-repos/<timestamp>   # logs y reportes
/tmp/gocracker-external-repos/cache         # clones reutilizables
```

## Que Hace

- clona o reutiliza cada repo de la muestra
- resuelve una ruta Dockerfile o Compose declarada en el manifiesto
- arranca el caso con `gocracker repo` o `gocracker compose`
- guarda un log por repo
- escribe un `results.tsv` maquina-legible
- escribe un `summary.md` humano-legible
- clasifica cada caso en `PASS` o `FAIL`

El exit code del runner es `0` solo si no hay `FAIL`.

## Prerrequisitos

- Linux con `/dev/kvm`
- `GOCRACKER_KERNEL` apuntando a un guest kernel valido. Recomendado:
  - `./tools/build-guest-kernel.sh`
  - `GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux`
- `sudo -v` ya ejecutado antes de correr el barrido
- `go`
- `git`
- `curl`
- `ip`
- `pgrep`
- `timeout`
- red saliente para `git clone` y pulls de imagen base

## Uso

Setup único (build + kernel + sudo cred):

```bash
cd /home/misael/Desktop/projects/gocracker
./tools/build-guest-kernel.sh
go build -o gocracker ./cmd/gocracker
sudo -v
```

### Un solo repo

```bash
echo "traefik-whoami" > /tmp/ids.txt
sudo -E python3 tests/external-repos/sweep.py /tmp/ids.txt --concurrency 1
```

### Regression gate (historical-pass minus historical-unstable)

```bash
comm -23 \
  <(grep -v '^#' tests/external-repos/historical-pass.ids | sort -u) \
  <(grep -v '^#' tests/external-repos/historical-unstable.ids | sort -u) \
  > /tmp/gate-ids.txt

sudo -E python3 tests/external-repos/sweep.py /tmp/gate-ids.txt \
  --concurrency 3 --boot-timeout 900 \
  --log-dir /tmp/gate-logs \
  --results /tmp/gate-results.tsv \
  --summary /tmp/gate-summary.txt
```

### Listas de ids disponibles

- `historical-pass.ids`: 115 casos que dieron `PASS` en la revalidación del 2026-04-14
- `historical-unstable.ids`: 113 excluidos del gate (upstream drift, release-tarball pattern, build >15min, etc.)
- `historical-tty.ids`: subconjunto dockerfile/service que valida guest + PTY real
- `compose-tty.ids`: pares `repo-id + service` para validar guest + PTY vía `compose exec`
- `curated-50.ids`: 50 repos adicionales curados pendientes de validación inicial
- `manifest.tsv`: TSV completo con `id, kind, url, ref, path, stack, mode, probe_type, probe_target, probe_expect, mem_mb, disk_mb, notes, dockerfile`

Orden recomendado cuando validás cambios en `pkg/vmm`, `pkg/container`, `internal/oci`:

1. `go test ./... -tags integration` (unit + integration)
2. `sweep.py historical-pass.ids` con `--exclude` de `historical-unstable.ids`
3. (opcional) `sweep.py historical-tty.ids`
4. (opcional) `sweep.py compose-tty.ids`

Snapshot manual documentado al 2026-04-06:

```bash
go build -o ./gocracker ./cmd/gocracker
go build -o ./gocracker-vmm ./cmd/gocracker-vmm
go build -o ./gocracker-jailer ./cmd/gocracker-jailer
sudo -v
```

Casos revalidados uno por uno:

| Caso | Estado al 2026-04-08 | Comando exacto | Resultado esperado |
| --- | --- | --- | --- |
| guest smoke one-shot | `PASS` | `sudo -n ./gocracker run --image alpine:3.20 --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --cmd 'echo alpine-ok' --wait --tty off` | imprime `alpine-ok` y termina limpio |
| guest smoke con PTY | `PASS` | `sudo -n ./gocracker run --image alpine:3.20 --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --wait --tty force` | aparece prompt; `_`, backspace, `Ctrl-C` y `exit` funcionan sobre la PTY del exec-agent |
| `distribution-registry` | `PASS` | `echo distribution-registry \| sudo -E python3 tests/external-repos/sweep.py /dev/stdin --concurrency 1` | termina con `PASS distribution-registry` |
| `actual-server-compose` | `PASS` | `echo actual-server-compose \| sudo -E python3 tests/external-repos/sweep.py /dev/stdin --concurrency 1` | termina con `PASS actual-server-compose` |
| `go-gitea-gitea` | `Pending rerun` | `echo go-gitea-gitea \| sudo -E python3 tests/external-repos/sweep.py /dev/stdin --concurrency 1` | al 2026-04-06 seguia en `make backend` y todavia no habia llegado a boot del guest |

Las secuencias crudas `\x1b[1;1R` y `\x1b[?2004h/l` cuentan como ruido de terminal, no como output valido del guest. Los gates TTY ahora las filtran y, si reaparecen en transcript, el caso debe considerarse fallido.
Al 2026-04-09, el camino interactivo nuevo quedo cubierto por integration tests privilegiados para `run`, `compose exec`, healthchecks Compose in-guest y aislamiento/cleanup por stack:

- `TestAPIServeRunExec`
- `TestCLIComposeServeExec`
- `TestCLIRunInteractiveExec`
- `TestCLIComposeExecInteractive`
- `TestCLIComposeServeHealthcheckExecBinary`
- `TestComposeStackIsolationAndCleanup`

Listar la muestra versionada:

```bash
awk -F'\t' '$1 !~ /^#/ && NF {print $1}' tests/external-repos/manifest.tsv
```

## Variables Utiles

```bash
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux
GC_BIN=./gocracker
EXT_REPO_LOG_DIR=/tmp/gocracker-external-repos/custom-run
EXT_REPO_CACHE_DIR=/tmp/gocracker-external-repos/cache
EXT_REPO_FILTER=python
EXT_REPO_IDS=traefik-whoami,mlflow
EXT_REPO_LIMIT=10
EXT_REPO_REFRESH=1
EXT_REPO_BOOT_TIMEOUT=45
EXT_REPO_SERVICE_WINDOW=10
EXT_REPO_MIN_FREE_GB=12
```

`EXT_REPO_REFRESH=1` fuerza reclone limpio del cache local.
`EXT_REPO_MIN_FREE_GB` evita arrancar un caso nuevo si el host ya no tiene espacio libre suficiente en el filesystem del repo o en `/tmp`.

## Manifiesto

La muestra vive en:

```bash
tests/external-repos/manifest.tsv
```

El manifiesto tiene exactamente 200 entradas y cada fila declara:

- id estable del caso
- tipo: `dockerfile` o `compose`
- repo GitHub
- ref fija
- path relativo a Dockerfile/subdir o compose file
- stack principal
- modo de verificacion
- probe minima
- memoria sugerida
- disco sugerido (`disk_mb`)

Para filas `dockerfile`, el `path` del manifiesto se interpreta como ruta al Dockerfile; el harness usa el repo root como `ContextDir` por defecto cuando el path apunta a un archivo.

Las entradas nuevas se validan y fijan con:

```bash
./tools/validate-external-candidates.py candidates.tsv --output validated.tsv
```

El manifiesto checked-in ya no usa refs flotantes.

Los casos setup-heavy o prebuild-heavy que no queremos en la muestra principal se documentan en el `README` raiz, en las secciones `Current Sweep Failures` y `Excluded / Setup-Heavy Examples`.

Los runners de TTY no reemplazan el barrido normal de boot/probe: agregan una capa interactiva arriba del guest para detectar regresiones de consola, shell y `compose exec` sin volver a depender de pruebas ad hoc.

## Auth De Registry

El barrido no depende de Docker Engine, pero muchas bases siguen leyendo credenciales estilo Docker desde `~/.docker/config.json`.

Para imagenes publicas el camino por defecto es sin login.

Si aparece rate limit o usas bases privadas:

```bash
docker login
sudo docker login
```

La segunda llamada importa porque los procesos que lanzan las VMs corren con `sudo`, asi que root tambien necesita acceso a `/root/.docker/config.json`.

## Compose: Estado Practico Actual

El soporte Compose ya no esta en el estado “solo parsea”:

- `image/build`
- `command`
- `entrypoint`
- `environment`
- `working_dir`
- `depends_on`
- `ports` TCP con bridge y forward local
- `volumes` bind y named basicos
- `mem_limit`
- healthcheck basico para `service_healthy`

Limitaciones que siguen vigentes en este harness:

- healthchecks solo para patrones HTTP/TCP comunes
- drivers de volumen remotos mas alla del soporte actual del runtime pueden fallar
- el barrido real sigue requiriendo KVM/TUN/PTX sanos y `sudo -v`
- no se persiguen features avanzadas de Compose/BuildKit fuera de lo ya cubierto por `compose-go` y el parser AST de BuildKit

## Salidas

Cada corrida deja:

- `results.tsv`: tabla completa por repo
- `summary.md`: resumen legible rapido
- `<repo-id>.log`: log serial/compose del caso

Ejemplo:

```bash
cat /tmp/gocracker-external-repos/<timestamp>/results.tsv
sed -n '1,80p' /tmp/gocracker-external-repos/<timestamp>/summary.md
tail -n 120 /tmp/gocracker-external-repos/<timestamp>/traefik-whoami.log
```

## Importante

Workflow recomendado antes de una corrida real:

```bash
./tools/check-host-devices.sh
./tools/trace-dev-access.sh -- ./gocracker run --image alpine:latest --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --wait --tty off
```

El harness sigue pudiendo correr la muestra completa, pero el flujo recomendado para validar regresiones ahora es manual y supervisado, un repo por vez, verificando cleanup y espacio libre despues de cada caso.

Cuando cambie el tamaño fijo de la muestra, recuerda mantener sincronizado `EXT_REPO_EXPECTED_COUNT` en `tests/external-repos/lib.sh`.
