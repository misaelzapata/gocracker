# External Repo Sweep

Este folder deja un harness reproducible para validar repos externos con `gocracker`. El entrypoint canonico ahora es `go run ./cmd/gocracker-sweep`, y los scripts `run_one.sh`, `run_historical_pass.sh`, `run_historical_tty.sh`, `run_compose_tty.sh` y `run_fixed_50.sh` son wrappers versionados arriba de ese runner.

Los scripts viven en el repo, pero los logs, clones cacheados y reportes se escriben fuera del worktree:

```bash
/tmp/gocracker-external-repos/<timestamp>   # logs y reportes
/tmp/gocracker-external-repos/cache         # clones reutilizables
```

## Que Hace

- clona o reutiliza cada repo de la muestra
- resuelve una ruta Dockerfile o Compose declarada en el manifiesto
- arranca el caso con `gocracker repo`, `gocracker run --dockerfile` o `gocracker compose`, segun la fila del manifiesto
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

Para el gate Darwin firmado, el runner cambia a Apple Silicon + binarios ya firmados con `entitlements.plist`. En ese caso:

- correr `make build-darwin-e2e` antes del sweep
- usar `GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-arm64-Image`
- si los binarios firmados viven fuera del repo root, exportar `GC_BIN`, `GC_VMM_BIN` y `GC_JAILER_BIN`

## Uso

Flujo recomendado, un repo por vez:

```bash
cd /home/misael/Desktop/projects/gocracker
./tools/build-guest-kernel.sh
sudo -v
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux tests/external-repos/run_one.sh traefik-whoami
```

Baseline historico:

```bash
tests/external-repos/historical-pass.ids
tests/external-repos/historical-unstable.ids
tests/external-repos/historical-tty.tsv
tests/external-repos/compose-tty.tsv
```

- `historical-pass.ids` es la lista base de casos que antes dieron `PASS`
- `historical-unstable.ids` separa casos historicos que hoy ya no son un gate reproducible por drift upstream o Dockerfiles release-shaped
- `historical-tty.tsv` fija `id + command + expect` para validar guest + PTY real
- `compose-tty.tsv` fija `repo-id + service + command + expect` para validar guest + PTY via exec interactivo
- `ollama-ollama` hoy vive en `historical-unstable.ids` porque el caso ya supero regresiones reales del extractor/builder, pero su stage CUDA sigue siendo demasiado pesado para el gate manual estable

Barrido amplio o shardeado:

```bash
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
  EXT_REPO_SHARD_INDEX=0 \
  EXT_REPO_SHARD_TOTAL=4 \
  go run ./cmd/gocracker-sweep \
    --manifest tests/external-repos/manifest.tsv \
    --ids-file tests/external-repos/historical-pass.ids \
    --exclude-ids-file tests/external-repos/historical-unstable.ids
```

Ese camino queda para CI y corridas supervisadas; para validar una regresion puntual sigue siendo mejor el flujo manual caso-por-caso.
Los wrappers `run_historical_pass.sh` y `run_fixed_50.sh` ya restan `historical-unstable.ids` automaticamente para que el gate bloqueante no vuelva a meter casos marcados como inestables.

Gates reproducibles recomendados antes del `fixed-50`:

```bash
GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
  tests/external-repos/run_historical_pass.sh

GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
  tests/external-repos/run_historical_tty.sh

GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux \
  tests/external-repos/run_compose_tty.sh
```

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
| `distribution-registry` | `PASS` | `GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux EXT_REPO_IDS=distribution-registry tests/external-repos/run_historical_tty.sh` | termina con `PASS distribution-registry` |
| `actual-server-compose` | `PASS` | `GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux EXT_REPO_IDS=actual-server-compose tests/external-repos/run_compose_tty.sh` | termina con `PASS actual-server-compose` |
| `go-gitea-gitea` | `Pending rerun` | `GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux EXT_REPO_IDS=go-gitea-gitea tests/external-repos/run_historical_tty.sh` | al 2026-04-06 seguia en `make backend` y todavia no habia llegado a boot del guest |

Las secuencias crudas `\x1b[1;1R` y `\x1b[?2004h/l` cuentan como ruido de terminal, no como output valido del guest. Los gates TTY ahora las filtran y, si reaparecen en transcript, el caso debe considerarse fallido.
Al 2026-04-09, el camino interactivo nuevo quedo cubierto por integration tests privilegiados para `run`, `compose exec`, healthchecks Compose in-guest y aislamiento/cleanup por stack:

- `TestAPIServeRunExec`
- `TestCLIComposeServeExec`
- `TestCLIRunInteractiveExec`
- `TestCLIComposeExecInteractive`
- `TestCLIComposeServeHealthcheckExecBinary`
- `TestComposeStackIsolationAndCleanup`

Orden recomendado:

1. smoke minimo del guest
2. `run_historical_pass.sh`
3. `run_historical_tty.sh`
4. `run_compose_tty.sh`
5. `run_fixed_50.sh`

Listar la muestra versionada:

```bash
tests/external-repos/run_one.sh --list
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
EXT_REPO_SUDO=1
EXT_REPO_SHARD_INDEX=0
EXT_REPO_SHARD_TOTAL=4
```

`EXT_REPO_REFRESH=1` fuerza reclone limpio del cache local.
`EXT_REPO_SUDO=1` hace que el runner prefije `sudo -n` a los comandos `gocracker` cuando el host Linux lo necesita.

## Manifiesto

La muestra vive en:

```bash
tests/external-repos/manifest.tsv
```

El manifiesto checked-in hoy tiene 328 entradas versionadas y cada fila declara:

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
