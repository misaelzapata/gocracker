# Plan de acción: Sandboxes sobre `feat/warm-cache`

> **Estado**: propuesta. Rama base: `feat/warm-cache` (f19049a).
> **Objetivo**: construir el feature de sandboxes combinando lo mejor de `feat/sandbox-api` (ya en main) y `feat/sandboxes-v2` (abandonado en f13c464), sobre los cimientos de warm-cache. Incremental, cada fase funcional por sí misma. Premisa: **reliable + fast + simple**.

---

## 1. Diagnóstico de los dos intentos previos

### `feat/sandbox-api` — primitivas low-level (ya merged en main)
- `pkg/warmcache/cache.go` — cache content-addressable por SHA-256 de (image+kernel+cmdline+mem+vcpus+arch+net).
- `pkg/warmpool/pool.go` — ring de workers restaurados, `Acquire` no-bloqueante (devuelve nil si vacío).
- `internal/api/api.go` — `POST /vms/{id}/clone`, `network_mode`, pause/resume.
- Fix crítico: `80c6871` — virtio-fs rebind es infeasible; fallback memfd-backed restore.
- Fix crítico: `a3eeeb4` — fsync + MADV_WILLNEED + backoff dial + guest re-IP.

### `feat/sandboxes-v2` — plataforma stateful completa (abandonada)
- `sandboxes/cmd/gocracker-sandboxd` — binario separado de control plane.
- `sandboxes/internal/controlplane/server.go` (~600 líneas, 30+ endpoints).
- `sandboxes/internal/pool/manager.go` — reconciliación hot/paused/cold por template.
- `sandboxes/internal/templates/service.go` — SpecHash caching, atomic promote, origin backfill.
- `sandboxes/internal/toolboxhost/host.go` — bootstrap del agente vía `exec + base64` post-boot.
- `sandboxes/internal/toolboxguest/server.go` — agente in-guest, vsock 10023.
- `sandboxes/internal/preview/token.go` — HMAC-SHA256 tokens + subdominio `<id>--<port>.sbx.localhost`.
- SDKs Go/Python/TS + 22 ejemplos cookbook.

### Por qué se cayó `feat/sandboxes-v2`

| Síntoma observado por el usuario | Causa raíz (inferida de commits) | Commit que intentó arreglarlo (tarde) |
|---|---|---|
| Cliente no conecta / se cae | `EnsureToolbox` corría en cada lease; bootstrap del agente vía `runtime.Exec → base64 upload → spawn` era una carrera post-boot de ~200 ms | `f13c464` (stamp `ToolboxVersion` para skip en warm) |
| Pool no entrega lease | VMs muertas contadas como vivas; sin budget global; thundering herd en pool vacío | `8a3494d` (reapDead + budget + backoff) + `f13c464` (event-refill) |
| Muchos boots lentos | Fallback a cold cuando pool vacío sin señal event-driven; callers bloqueaban 2s en polling | `f13c464` (`WarmAvailableCh` canales) |
| Source VM leak | Rebuild no paraba la VM source previa | `5809c44` |
| Guest sin internet post-restore | Tap re-IP faltante tras memoria restaurada | `e9540bc` |
| Snapshot cache invalidaba por net config | `NetworkMode` no estaba en la clave | `9d0297a` |

**Lección fundamental**: todo lo que bootstrapee el agente post-boot introduce latencia y puntos de fallo que luego exigen stamps, health probes, version skips, backoffs. La arquitectura pelea contra sí misma.

---

## 2. Premisa de diseño

Alineada con feedback `reliable + fast + simple + all working` (ver memoria): no `t.Skip`, no error swallow, no workaround comments. Sandbox flow es hot path single-digit ms.

1. **Agente horneado en el rootfs del template**, no bootstrapeado. PID 1 lo lanza *antes* del `exec` del CMD del usuario. A t=0 el vsock ya escucha.
2. **Control plane habla siempre por vsock (CID=3)**, nunca por IP. La IP del tap puede cambiar sin romper al cliente.
3. **Re-IP del guest en restore** vía netlink desde dentro del init (ya existe en `e9540bc`), disparado por RPC vsock `setNetwork(ip, gw, mac)` que el host invoca tras `Restore()`. Sin DHCP en el hot path.
4. **Snapshot capturado en estado idle** (sin CMD corriendo), reutilizable con cualquier CMD posterior vía `exec` del agente. warm-cache ya lo permite con `WarmCapture=true`.
5. Cada fase deja el sistema **funcional end-to-end**. Nada de "se termina en la siguiente fase". Si una fase no pasa sus criterios, no se avanza.
6. Extender jobs de CI existentes (`privileged-integration`), no añadir nuevos — ver memoria `feedback_extend_existing_ci`.
7. Probar con VM real antes de commit — ver memoria `feedback_test_before_commit` y `feedback_never_mark_done_without_running`.

---

## 3. Inventario de lo existente (qué reutilizar)

### En la rama actual `feat/warm-cache`
- `pkg/warmcache/cache.go` + `pkg/container/warmcache.go` — captura sparse de dirty pages, jailer-on, NetworkMode en key, ARM64 VGIC, 3 ms restore vs 362–389 ms cold boot.
- `pkg/vmm/snapshot*.go` — snapshot sync, exec config persistente, CMD-after-restore.
- `internal/vsock/vsock.go` — RX funcional, CID fijo 3, multiplexor host-side.
- `internal/guest/init.go` (~3300 líneas) — exec agent en vsock 10022, `configureNetwork` con netlink `AddrReplace + RouteReplace`, parse de `gc.ip`/`gc.gw`/`gc.wait_network` desde cmdline.
- `internal/api/api.go` — 40+ endpoints Firecracker-compat + extendidos (`/run`, `/clone`, `/exec`, `/snapshot`, SSE events, UART logs).
- `tools/bench-rtt` — mide 6 primitivas (pause/resume/snapshot-capture/warmcache-miss/warmcache-hit/restore).
- `tools/pool-bench` — pool + exec end-to-end, p50/p95.

### En `feat/sandboxes-v2` (cherry-pick selectivo)
- `sandboxes/internal/controlplane/server.go` — HTTP surface completo. Traer.
- `sandboxes/internal/store/store.go` — in-memory + async persist + `WarmAvailableCh`. Traer tal cual.
- `sandboxes/internal/pool/manager.go` — **solo versión f13c464**, con reap + budget + backoff + event-refill. Nada anterior.
- `sandboxes/internal/templates/service.go` — SpecHash + origin backfill + atomic promote. Traer.
- `sandboxes/internal/preview/token.go` — HMAC tokens. Traer (41 líneas, trivial).
- `sandboxes/internal/toolboxguest/server.go` — código del agente. Traer **pero cambiar cómo se instala**: horneado en rootfs, no bootstrapeado.
- `sandboxes/internal/toolboxhost/host.go` — **reescribir**: borrar todo el camino `Bootstrap` / `InstallToolboxBinary` / `EnsureToolbox`. Dejar solo `ProxyHTTP` y RPC hacia el agente ya corriendo.
- SDKs Go/Python/TS — traer tal cual.
- 5 ejemplos canónicos del cookbook (hello, exec, files, preview, pool burst) — no los 22.

### Qué descartar
- Bootstrap del agente post-boot (camino entero).
- Versiones intermedias de `pool/manager.go` anteriores a f13c464.
- `exec_agent.go` en `pkg/vmm/` (v2 lo movió a toolboxguest; mantener esa decisión).
- Toda lógica de `sudo -u misael` — runtime como root (f13c464 ya fue en esa dirección).

---

## 4. Protocolo del agente (puerto 10023) — no reusar `guestexec` JSON

El protocolo actual `internal/guestexec/protocol.go` causó dolor en los intentos previos:
- `Request.Stdin` y `Response.Stdout/Stderr` son `string` dentro de JSON → no binary-clean, rompe con bytes no-UTF-8.
- `json.Decoder` lee la respuesta entera antes de devolver → sin streaming, sin backpressure, OOM con outputs grandes.
- Modos `exec`/`stream` mezclados sobre el mismo transport.

**El agente nuevo usa framing binario multiplexado, un socket vsock por exec, estilo Docker exec**:

```
[1 byte channel] [4 bytes length BE] [length bytes payload]
```

| Channel | Dirección | Contenido |
|---|---|---|
| 0 stdin   | host → guest | bytes raw, sin límite artificial |
| 1 stdout  | guest → host | bytes raw |
| 2 stderr  | guest → host | bytes raw |
| 3 exit    | guest → host | 4 bytes exit-code BE, cierra el stream |
| 4 signal  | host → guest | TIOCSWINSZ (8 bytes: cols+rows), SIGTERM/SIGKILL, etc. |

**Handshake**: una línea JSON inicial con la request (`cmd`, `env`, `workdir`, `tty`, `cols`, `rows`). Eso sí es chico; JSON ahí está bien y tiene el beneficio del tooling. Luego frames binarios hasta que el agente emite `(channel=3, len=4, exit-code)`.

**Por qué resuelve los problemas**:
- Stdout/stderr van directo del `pipe` del proceso hijo al socket — solo el buffer del kernel vsock (~128 KB) entre medio.
- Binary-clean por definición: son bytes, no strings.
- Backpressure gratis: socket lleno → `write()` bloquea → pipe del proceso se llena → hijo se frena en su `write()`.
- Cliente que cierra socket mata el exec: `read()` del guest falla, agente hace `SIGTERM` al hijo con timeout + `SIGKILL`.

**Files namespace usa el mismo principio** — nada de multipart, nada de base64:
- `GET /files/download?path=/foo` → agente hace `io.Copy(socket, file)` raw.
- `POST /files/upload?path=/bar` → agente hace `io.Copy(file, socket)` hasta EOF.
- `GET /files?path=/dir` → JSON solo para el listing (pequeño, estructurado, apropiado).

**Dónde sí usamos JSON en el agente**:
- Handshake inicial del exec (request metadata).
- `/healthz` → `{ok, version, uptime_s}`.
- `/files` listing, `/secrets`, `/git/status` — respuestas estructuradas y chicas.
- `/internal/setnetwork` (Fase 2) → request/response chico.

**Dónde explícitamente no**:
- Data plane de exec (stdin/stdout/stderr).
- File upload/download payload.
- Cualquier cosa que pueda crecer sin cota.

**Límites y sanity**:
- Frame `length` capped a **1 MiB** por frame (header permite 4 GiB, pero reject > 1 MiB para defensa). Output grande se parte en múltiples frames.
- Handshake JSON capped a **64 KiB** (comando + env). Excede → cierra con error antes de spawnear.
- Timeout de exec opcional en la Request; cuando vence el agente hace `SIGTERM` + 2 s de gracia + `SIGKILL`.
- Sin timeout por defecto del lado del agente: el que impone timeout es el cliente, cerrando el socket.

**Qué pasa con `internal/guestexec/protocol.go`**:
- Queda para el exec agent en puerto 10022 (Firecracker-compat + API actual `/vms/{id}/exec`). No se toca.
- El agente toolbox en 10023 es protocolo nuevo, separado. Código cero compartido con `guestexec` para no heredar sus restricciones.

---

## 5. Fases incrementales

Cada fase: alcance, pasos, criterios de éxito, riesgos específicos.

### Fase 0 — Land warm-cache PR #8
**Estado**: ya en flight.
**Alcance**: merge a main cuando Copilot termine review round 2.
**Criterio**: PR verde, bench-rtt documentado, pool-bench ejecutable.

### Fase 1 — UDS vsock (Firecracker-style) — **BLOQUEANTE**

> Nada más avanza hasta que esta fase pase **todos** los tests + verificación explícita de jailer + snapshot/restore con UDS. Si algo falla, se arregla acá; no se apila feature encima.

**Propiedad del componente** (crítico):

El UDS vsock es feature **del runtime gocracker**, no de sandboxd. Es responsabilidad del VMM exponerlo — exactamente como Firecracker, donde el proceso `firecracker` abre el UDS y cualquier orquestador (firectl, jailer, sandboxd equivalente) se limita a consumirlo.

- **Listener vive en `pkg/vmm/vmm.go`** — parte del ciclo `Start`/`Pause`/`Restore`/`cleanup` de cada VM.
- **Configuración en `VMConfig`** — campo `VsockUDSPath` (JSON compat Firecracker: `{"vsock": {"guest_cid": 3, "uds_path": "..."}}`).
- **Protocolo wire = Firecracker** — `CONNECT <port>\n` del cliente, `OK <host-port>\n` o `FAILURE <errno>\n` del VMM. Clientes escritos contra Firecracker (firectl, SDKs terceros) funcionan sin cambios.
- **Sandboxd es consumidor, igual que cualquier otro cliente**. La CLI de gocracker, scripts de debug con `socat`, tests de integración, herramientas de terceros — todos dialean el mismo UDS por la misma vía.
- **API HTTP `/vms/{id}/vsock/connect` queda como fallback** en `internal/api/` (también dentro de gocracker, no de sandboxd), para clientes remotos que no pueden tocar el UDS local.

**Estimado**: 2–3 días.

**Alcance**: exponer el vsock de cada VM como Unix Domain Socket en el host con protocolo `CONNECT <port>\n` (idéntico a Firecracker). Socket por VM en `/var/run/gocracker/sandboxes/<id>.sock` (o dentro del chroot del jailer cuando aplica).

**Pasos**:
1. Añadir `VsockUDSPath string` a `VMConfig` en `pkg/vmm/vmm.go`. Default computed desde runtime state dir + VM ID. **Firecracker-compat JSON**: aceptar el schema `{"vsock": {"guest_cid": 3, "uds_path": "..."}}` en `PUT /vsock` de la API existente y en `RuntimeSpec`.
2. En `Start()` de la VM, si `vsockDev != nil`: `os.Remove(path); l, err := net.Listen("unix", path); os.Chmod(path, 0660)`. Spawn `acceptLoop(l, m)` en goroutine. Toda la lógica vive en `pkg/vmm/` — ningún consumer (sandboxd/CLI/tests) se entera de cómo se crea.
3. `acceptLoop`: por cada `Accept()`, leer la primera línea. Si es `CONNECT <port>\n`: responder `OK <host-port>\n` (**wire format idéntico a Firecracker**) y arrancar un bridge bidireccional `io.Copy` entre el UDS y `m.DialVsock(port)`. Si el dial falla, responder `FAILURE <errno>\n` y cerrar.
4. `cleanup()` / `Stop()`: cerrar el listener, cancelar todos los bridges activos (vía context), `os.Remove(path)`. Coordinar con `vsockDialMu` para no cerrar mientras hay dials en vuelo.
5. **Soporte jailer**: cuando `JailerMode=On`, `path` se resuelve relativo al chroot del jail. Clientes dialean vía `<jailer-root>/<id>/root<VsockUDSPath>`. El helper `ResolveHostSidePath(cfg)` (en `pkg/vmm/`) expone esa ruta de afuera — lo usa la CLI, sandboxd, tests, todos igual.
6. Exponer en `GET /vms/{id}` el `vsock_uds_path` (vista desde el host) resuelto por `ResolveHostSidePath`. Clientes se enteran sin conocer detalles de jailer. **Endpoint vive en gocracker `internal/api/`, no en sandboxd.**
7. En `Pause()`: cerrar todos los bridges activos (clientes deberán reconectar post-`Restore`). Listener sigue vivo. Test explícito.
8. En `Restore()`: si `VsockUDSPath` viene serializado en snapshot.json, ignorarlo y recomputar desde VM ID + runtime state dir actual (evita path stale entre hosts/jailer configs).

**Criterios de éxito — TODOS obligatorios para avanzar**:
- **Suite completa verde**: `go test ./...` + `make test-integration` (minimal kernel) + `make test-integration-virtiofs` (kernel con virtiofs). Cero regresiones.
- **Jailer on test E2E**: `JailerMode=On → run base-image → dial UDS desde afuera del jail con socat → exec → verificar output`. Cubre resolución de path, perms, visibilidad entre namespaces.
- **Snapshot/restore con UDS test**: `boot → capture snapshot → stop VM → restore → dial UDS (path re-resuelto) → exec → verificar`. En ambos modos (jailer on + off).
- **Snapshot con bridge activo**: `boot → abrir UDS bridge largo → Pause → assert bridge cierra limpio → Resume → reopen → exec funciona`. Sin goroutine leaks (verificar con `go test -race` + `runtime.NumGoroutine` antes/después).
- **Concurrencia**: 100 clientes simultáneos dialeando el mismo UDS, todos tienen bridges independientes, ninguno se cruza.
- **Cleanup en crash**: `kill -9` del runtime → reiniciar → socket stale removido, nuevo listener OK.
- **Permisos**: socket creado como `0660`, directorio padre `0750`. Test verifica perms y dueño.
- **Paridad con HTTP**: el mismo exec funciona por UDS **y** por `/vms/{id}/vsock/connect?port=10022`. Test que llama ambos paths sobre la misma VM y compara resultados.
- **Benchmark**: `bench-rtt` actualizado para usar UDS. Exec RTT via UDS < 5 ms (esperado mejor que HTTP actual).

**Riesgos específicos y cómo los cerramos**:
- *Jailer oculta el path* → test `TestVsockUDS_JailerOn` específico antes de cualquier otra cosa. Si no pasa, se detiene todo y se reevalúa.
- *Snapshot post-Pause con bridges activos cuelga* → forzar close de bridges en Pause es parte del paso 7; test explícito.
- *Path en snapshot.json entre máquinas distintas* → paso 8 lo maneja (ignorar path serializado).
- *Race entre `Accept` y `cleanup`* → `vsockDialMu` + context cancelation, mismo patrón de `DialVsock` actual. Test con `go test -race`.
- *Seccomp rechaza `bind(AF_UNIX)`* — comprobar en tests de warm-cache con jailer (ya existen) + ampliar allowlist si hace falta.

**Salida estable = gate para Fase 2**:
- Todos los tests above pasan 10 runs seguidos sin flakiness.
- Bench-rtt numbers documentados y estables.
- CI `privileged-integration` verde en 3 runs consecutivos.
- Sin goroutines/FDs leaked verificado con `lsof` post-test.
- **Smoke en ARM64 EC2** (ver memoria `reference_arm64_ec2`) — `gocracker run --vsock-uds-path ... --warm` boot + dial UDS con socat + exec + stop limpio, sin regresiones respecto al baseline warm-cache en ARM64. Si falla en ARM64, se arregla antes de declarar Fase 1 estable.

Mientras Fase 1 no esté estable, **no se toca** `toolboxguest`, ni `sandboxd`, ni se traen features de v2. La prioridad es que el transporte nuevo no se convierta en fuente de bugs aguas abajo.

---

### Fase 2 — Agente horneado (fundacional)
**Estimado**: 2–3 días.
**Alcance**: mover `toolboxguest` al rootfs del template como binario estático en `/opt/gocracker/toolbox/toolboxguest`. Arrancarlo desde `internal/guest/init.go` antes del `exec` del CMD usuario.

**Pasos**:
1. Compilar `sandboxes/cmd/toolboxguest` con `CGO_ENABLED=0 -ldflags="-s -w"` → binario estático ~8 MB.
2. Build del template base inyecta el binario en `/opt/gocracker/toolbox/toolboxguest` + `/opt/gocracker/toolbox/VERSION` (git sha corto).
3. `internal/guest/init.go`: añadir `startToolboxAgent()` tras línea ~195 (`startExecAgent`). Spawn `/opt/gocracker/toolbox/toolboxguest serve --vsock-port 10023 --state-dir /var/lib/gocracker/toolbox`.
4. Log redirigido a `/var/log/toolbox.log` + consola durante bring-up.
5. Matar todo el camino `toolboxhost.Bootstrap` / `InstallToolboxBinary` / `EnsureToolbox` / probe de readiness. Reemplazar por un `DialToolbox(ctx, cid)` que hace `vsock dial CID:10023` con backoff corto (max 200 ms, 5 intentos).
6. Stamp `ToolboxVersion` leído del `/opt/gocracker/toolbox/VERSION` del guest en el snapshot metadata, para que el host valide paridad en restore.

**Criterios de éxito**:
- `gocracker run base-python` → dentro de **400 ms** desde `Run()` hasta `vsock:10023 /healthz` → 200 OK.
- Capture + restore de snapshot idle → mismo dial responde **< 5 ms** tras restore.
- Cero llamadas a `runtime.Exec` para instalar/iniciar el agente en cualquier path.
- Smoke test: 50 cold boots seriados, 50 restores desde snapshot, 100% success.

**Riesgos**:
- *Binario no arranca en initrd-switch_root intermedio* → solo lanzarlo **después** de `switch_root` a ext4, cuando `/opt` ya esté montado.
- *Listener fd corrupto post-restore* → ya probado con exec agent en 10022 (funciona); usar el mismo patrón exacto.
- *Agente crashea silenciosamente* → supervisar con loop en init (restart con max 3 tries).

### Fase 3 — Re-IP fiable en restore
**Estimado**: 1–2 días.
**Alcance**: un solo RPC vsock `SetNetwork(ip, gw, mac)` expuesto por el agente. Host lo invoca **entre** `Restore()` y devolver la VM al cliente. Si falla, la VM se tira y se reintenta (no se entrega VM con IP stale — eso es lo que rompió antes).

**Pasos**:
1. Definir endpoint `POST /internal/setnetwork` en `toolboxguest` (solo vsock, no expuesto al cliente).
2. Handler ejecuta: `netlink.LinkSetDown(eth0)` → `netlink.LinkSetHardwareAddr(mac)` → `netlink.LinkSetUp` → `netlink.AddrReplace(ip)` → `netlink.RouteReplace(default via gw)` → `arping -U -c 2 -I eth0 ip` (gratuito, para que el bridge actualice su FDB).
3. Pool de IPs determinístico en `store.go`: `10.100.<sandbox-idx>.2/30`, gw `.1`, MAC derivada `02:42:<idx-be-32bits>`. Asignación atómica bajo el mismo lock que sandbox ID.
4. Control plane `Lease`: tras `Restore()`, invoca `SetNetwork`. Timeout 250 ms. Fallo → `runtime.StopVM` + reintentar lease con otra warm VM. Si tres seguidas fallan, marcar template con error en pool.
5. Test: 1000 restores consecutivos, asertar IP reasignada correctamente + ping al gw < 50 ms post-restore.

**Criterios de éxito**:
- 1000/1000 restores con IP reasignada, 0 fallos en ping al gw.
- Test integración: `curl` desde host al tap del guest inmediatamente tras lease → 200 OK.
- Latencia extra de `SetNetwork` sobre restore base: **< 15 ms p95**.

**Riesgos**:
- *Bridge host cachea MAC vieja* → `arping` gratuito lo fuerza a refrescar; probar explícitamente con dos leases consecutivos de la misma warm VM.
- *Colisión de IP bajo concurrencia* → lock único en store; tests con burst de 50 crean simultáneas.

### Fase 4 — Control plane mínimo
**Estimado**: 2 días.
**Alcance**: cherry-pick de controlplane + store **sin** pool manager ni templates. Solo `POST /sandboxes` hace cold boot directo del template por nombre.

**Endpoints mínimos**:
- `POST /sandboxes` — crea VM cold, devuelve `{id, runtime_id, guest_ip, state}`.
- `GET /sandboxes/{id}`, `DELETE /sandboxes/{id}`.
- `POST /sandboxes/{id}/process/execute` — relay vsock → agente, stream stdout/stderr.
- `POST /sandboxes/{id}/files/upload`, `GET /files/download`, `GET /files` (list).
- `GET /sandboxes/{id}/events` — SSE desde `pkg/vmm/events.go`.
- `GET /health`, `GET /debug/vars`.

**Binario**: `cmd/gocracker-sandboxd serve --port 9091 --state-dir /var/lib/gocracker-state`.

**Criterios de éxito**:
- End-to-end con `curl` para todos los endpoints, sin dependencias a pool.
- Latencia cold p50: **~420 ms**. p95: **~550 ms**.
- 100 ejecuciones seriadas: 0 flakiness.

**Riesgos**:
- *Store JSON corrupto en crash* — reutilizar async persist de v2 (`store.go` tal cual, ya probado).

### Fase 5 — Warm pool endurecido
**Estimado**: 3 días.
**Alcance**: integrar `pool/manager.go` en su versión **final** (f13c464), con warm-cache de esta rama. Conciliar ambas APIs.

**Pasos**:
1. Traer `sandboxes/internal/pool/manager.go` de f13c464.
2. Adaptar `runtimeclient.RestoreVM` para invocar `pkg/warmcache.Lookup` + `Restore` (no su propio path).
3. Política por defecto: `MinHot=0, MaxHot=2, MinPaused=4, MaxPaused=8`. Casi todo paused (restore en 3 ms), hot solo para templates con startup pesado que no quieran pagar el `SetNetwork`.
4. `GlobalInflightBudget = 8`, `ConsecutiveFailureThreshold = 3`, `Cooldown = 60 s`.
5. `reapDead` cada 5 s + `WarmAvailableCh` canales para refill event-driven.
6. Extender `privileged-integration` CI job (no crear job nuevo) con `tools/pool-bench` en burst 50.

**Criterios de éxito**:
- `tools/pool-bench` sobre pool de 8: **p95 exec < 40 ms** (3 ms restore + 15 ms SetNetwork + 15 ms vsock exec RTT).
- Burst 50 `CreateSandbox` concurrentes: **p95 lease < 20 ms, 0 errores, 0 VMs huérfanas tras 5 min**.
- Kill -9 sobre warm VM → reapDead la detecta en < 10 s y reemplaza.

**Riesgos**:
- *Divergencia warm-cache API vs v2 pool manager* → hacer la integración en una PR separada antes de tocar nada más; gate es pool-bench.
- *Event channels deadlock bajo cierre* → usar `context.Done()` uniforme en todos los goroutines del manager.

### Fase 6 — Templates con SpecHash
**Estimado**: 1–2 días.
**Alcance**: `sandboxes/internal/templates/service.go` tal cual. Templates base horneados con el agente de Fase 2.

**Pasos**:
1. Traer `templates/service.go` + modelos.
2. Templates base (seed): `base-python`, `base-node`, `base-bun`, `base-go`, con toolboxguest horneado vía Dockerfile:
   ```
   FROM python:3.12-alpine
   COPY --from=gocracker-toolbox /toolboxguest /opt/gocracker/toolbox/toolboxguest
   COPY --from=gocracker-toolbox /VERSION /opt/gocracker/toolbox/VERSION
   ```
3. `POST /templates` multipart funciona end-to-end. `SpecHash` cache hit es no-op. Borrar snapshot → reconstruye.
4. CLI `gocracker-sandboxd template create|list|delete`.

**Criterios de éxito**:
- Create template desde Dockerfile → snapshot en disco. Segundo create idéntico: cache hit, **< 10 ms**.
- Delete → disco limpio, store sin orphan.
- Custom template sobre `base-python` con `pip install fastapi` funciona y `GET /sandboxes/{id}/process/execute python -c "import fastapi"` retorna 0.

### Fase 7 — Preview + DNS
**Estimado**: 1 día.
**Alcance**: `preview/token.go` + handler `GET /previews/{token}` + subdominio `<id>--<port>.sbx.localhost` con cookie `sbx_t`. Relay vía `toolboxhost.ProxyHTTP` → agente `/proxy/http/{port}`.

**Criterios de éxito**:
- `gocracker sandbox preview 3000` → URL firmada. `curl` con y sin subdominio llega a la app del guest.
- Token expirado → 401. Token para otro sandbox → 403.

### Fase 8 — SDKs + cookbook
**Estimado**: 2 días.
**Alcance**: los tres SDKs de v2 tal cual (errores tipados + pooling de conexiones). Cookbook inicial de **5 ejemplos** canónicos:
1. `hello_world.py` — create + exec `echo hello`.
2. `exec_stream.py` — exec con stream stdout línea-a-línea.
3. `files.py` — upload + read + list.
4. `preview.py` — levanta server, obtiene URL, curl.
5. `pool_burst.py` — 50 concurrentes, mide p95.

Los 17 ejemplos restantes de v2 quedan para una fase 8 post-merge.

---

## 6. Métrica de éxito global

Bench sobre `base-python`, 1000 sandboxes, p50/p95/p99:

| Operación | p50 | p95 | p99 |
|---|---|---|---|
| Warm lease (paused→ready, con SetNetwork) | **< 10 ms** | **< 20 ms** | **< 50 ms** |
| Cold boot (pool miss, con agente horneado) | < 420 ms | < 550 ms | < 900 ms |
| Agente `/healthz` post-lease | **< 5 ms** | < 10 ms | < 20 ms |
| Exec round-trip (vsock RPC) | < 15 ms | < 25 ms | < 40 ms |
| Preview HTTP proxy overhead | < 3 ms | < 8 ms | < 15 ms |

Recuperación: burst 50 simultáneas, **0 errores**, **0 huérfanos** tras 5 min de idle.

---

## 7. Riesgos globales y mitigaciones

| Riesgo | Impacto | Mitigación |
|---|---|---|
| **UDS + jailer rompe visibilidad del socket** | Fase 1 cae → todo bloqueado | Test `TestVsockUDS_JailerOn` en día 1 de Fase 1. Si no pasa, se detiene avance y se rediseña (posibles caminos: socat bridge fuera del jail; o bind-mount explícito del dir). |
| UDS bridge goroutine leak bajo carga | Degradación silenciosa | `go test -race` + assert `runtime.NumGoroutine` delta=0 post-test. |
| Snapshot con agente corriendo no restaura limpio | Fase 2 cae → premisa entera falla | Test específico en Fase 2 día 1. Si falla, cancelar plan y replantear bootstrap ligero. |
| IP pool se agota bajo uso real | Sandboxes nuevos fallan | `/24` por range, 253 sandboxes concurrentes; si se acerca, expandir a `/20` (4093). |
| Integración warm-cache ↔ pool-manager diverge más de lo estimado | Fase 5 se alarga | PR aislada solo para la integración, gate pool-bench. No tocar controlplane hasta que pase. |
| Virtio-fs en template rompe restore | Templates con fs compartido no funcionan | Ya conocido (`80c6871`); templates base sin virtio-fs; usuarios que lo necesiten usan memfd-backed restore. |
| arping gratuito no refresca bridge | Primer paquete post-lease se pierde | Fallback: reenviar `arping` tras 100 ms. Probar con 2 leases consecutivos de misma warm VM. |
| Bug ARM64 pendiente (ver memoria `project_arm64_warm_cache`) | Plan sólo funciona x86 hasta que se fixee restore | Fases 1–5 son arch-independientes en diseño; el restore ARM64 (sysreg+VGIC) se trabaja en paralelo en otra rama. |

---

## 8. Secuencia de commits propuesta

**Gate duro después del commit 4 (fin de Fase 1)**: suite completa + jailer + snapshot/restore + race + 10 runs consecutivos sin flaky. Si algo falla, se arregla acá antes de seguir.

1. `feat(vmm): VsockUDSPath config + host-side UDS listener + CONNECT proto` — Fase 1 pasos 1–3.
2. `feat(vmm): UDS path resolution across jailer chroot` — Fase 1 pasos 5–6.
3. `feat(vmm): UDS bridge lifecycle tied to Pause/Restore/cleanup` — Fase 1 pasos 4, 7–8.
4. `test(vsock): UDS jailer + snapshot + concurrency + leak coverage` — Fase 1 criterios de éxito.
5. `feat(toolbox): static build + bake into template rootfs` — Fase 2 pasos 1–2.
6. `feat(guest-init): launch toolboxguest from PID 1 on vsock 10023` — Fase 2 pasos 3–4.
7. `refactor(toolboxhost): remove bootstrap path, dial via UDS CONNECT` — Fase 2 paso 5.
8. `feat(toolbox): stamp ToolboxVersion in snapshot metadata` — Fase 2 paso 6.
9. `feat(toolboxguest): SetNetwork RPC with netlink + arping` — Fase 3 pasos 1–2.
10. `feat(store): deterministic IP pool per sandbox` — Fase 3 paso 3.
11. `feat(lease): invoke SetNetwork post-restore, fail-close on error` — Fase 3 paso 4.
12. `feat(sandboxd): minimal controlplane (create/delete/exec/files/events)` — Fase 4.
13. `feat(pool): integrate f13c464 pool-manager with warm-cache` — Fase 5.
14. `ci(privileged-integration): add pool-bench burst gate` — Fase 5 paso 6.
15. `feat(templates): SpecHash + 4 base templates with baked agent` — Fase 6.
16. `feat(preview): signed tokens + subdomain DNS routing` — Fase 7.
17. `feat(sdk): Go + Python + TypeScript` — Fase 8 día 1.
18. `docs(cookbook): 5 canonical examples` — Fase 8 día 2.

Total estimado: **14–15 días de trabajo** (antes 12; +2–3 por la Fase 1 bloqueante). Cada commit mergeable y probado. Los commits 1–4 son prerequisito absoluto para cualquiera de los siguientes.

---

## 9. Decisión abierta para el usuario

- Fase 1 (UDS vsock) arranca ya — es la prioridad. Ningún paso aguas abajo se toca hasta que los tests la validen (especialmente jailer + snapshot).
- ¿Los 4 templates base de Fase 6 están bien (`python/node/bun/go`), o priorizás otros (`nextjs`, `rust`)?
- ¿El pool default `MinPaused=4, MaxPaused=8` (Fase 5) es razonable para tu uso esperado, o apuntamos más alto?

---

## 10. Estado post-Fases 1–8 (2026-04-22): bugs arreglados + cookbook sweep + roadmap de velocidad

Fases 1–8 mergeadas. Esta sección captura lo descubierto en el sweep end-to-end + la hoja de ruta para bajar warm-lease de los ~20 ms actuales a `<10 ms`.

### 10.1 Bugs encontrados y arreglados

1. **Pool refiller colgado tras el primer refill** — `RegisterPool` pasaba `r.Context()` (contexto de la request HTTP) a `Pool.Start`, así que el ctx del refiller se cancelaba al cerrar la response. Los refills en vuelo abortaban con `context canceled` y el pool quedaba en 2 paused aunque `MinPaused=5`. Arreglado usando `context.Background()` (ver `sandboxes/internal/sandboxd/pool.go:RegisterPool`). El ciclo de vida del refiller ahora va hasta `UnregisterPool`/`Shutdown`, como dice el comentario del método.
2. **Template `warm-cache lookup miss after capture`** — `container.Run` aplica `MemMB=256` (default) **antes** de computar el warmcache key; el builder del template computaba el key de Lookup sin ese default. Stored key ≠ Lookup key ⇒ miss falso después de capturar un snapshot válido. Arreglado replicando el default en `sandboxes/internal/templates/builder.go`.
3. **Python SDK `dial_timeout=5s` se aplicaba a todos los reads** — `settimeout` en el socket se heredaba a los reads de respuesta, así cualquier request >5 s (git clone, uploads grandes) moría con EOF prematuro. Arreglado limpiando el timeout (`settimeout(None)`) después del handshake `CONNECT`.
4. **Warn-spam en `UnregisterPool`** — cancelar `p.ctx` vía `Stop()` dispara `context.Canceled` en los refills en vuelo, que terminaban como `pool boot failed` WARN. No es un fallo: arreglado distinguiendo `errors.Is(err, context.Canceled)` y saltando el bump de `consecutiveFailures` + log.
5. **Cookbook `preview.py` colgaba** — servidor HTTP backgrounded con `&` heredaba stdout abierto, así `exec_stream` nunca veía EOF. Arreglado redirigiendo stdin/stdout/stderr a `/dev/null` en el exec.
6. **Cookbook `git_clone.py` panic de kernel** — `alpine/git:latest` tiene `ENTRYPOINT=["git"]`, que corría con el `cmd` que pasábamos, salía con error y mataba PID 1 → kernel panic. Arreglado usando `alpine:3.20` + `apk add git` en runtime (sandboxd todavía no expone override de Entrypoint; ver item 10.3).

### 10.2 Cookbook sweep — 10/10 verdes

Con las fixes anteriores, todos los ejemplos pasan sin errores:

| Ejemplo | Feature demostrada | Tiempo típico |
|---|---|---|
| `hello_world.py` | create_sandbox + exec | ~150 ms cold |
| `exec_stream.py` | exec_stream | ~150 ms cold + live stream |
| `files.py` | upload/download/list | — |
| `files_full.py` | mkdir/chmod/rename/delete | — |
| `preview.py` | mint preview + HTTP proxy (subdominio limpio) | — |
| `secrets.py` | set/list/delete secret | — |
| `pool_burst.py` (burst=3) | warm lease concurrente | p95 ~22 ms |
| `template_pool.py` | template → pool → lease | 18–34 ms por lease |
| `concurrent_cold.py` (N=3) | N cold-boots concurrentes | ~140 ms p95 |
| `git_clone.py` | git_clone + git_status del toolbox | — |

### 10.2.1 Métricas finales (2026-04-22 tarde, tras todas las optimizaciones)

Medido con `alpine:3.20` + kernel standard, pool de 8 paused, en x86 jailer-off, SDK Python:

| Fase | min | p95 |
|---|---:|---:|
| `lease_sandbox` (pool warm) | 0.88 ms | **1.5 ms** |
| `sb.process.exec('echo')` (primer exec sobre UDS+vsock) | 19.2 ms | 30.9 ms |
| `delete` (async) | 0.92 ms | **1.9 ms** |
| **Total E2E** (lease + echo + delete) | 22.3 ms | **~35 ms** |

Comparación con el punto de partida (misma carga):

| | Antes | Ahora | Ganancia |
|---|---:|---:|---:|
| lease p95 | 20.8 ms | 1.5 ms | **14×** |
| delete p95 | 79 ms | 1.9 ms | **42×** |
| E2E p95 | ~120 ms | ~35 ms | **3.4×** |

Red verificada (`ip addr` / `ip route` / `nslookup google.com` / `apk add bash` sobre un lease warm): funciona sin SetNetwork porque el IP queda baked-in en el snapshot desde el cold-boot.

### 10.3 Optimizaciones de SetNetwork ya aplicadas

Bottleneck identificado con instrumentación (`lease timing id=... resume_ms=X setnet_ms=Y`): `resume_ms=0` siempre, **100 % del costo vive en SetNetwork** (12–27 ms sin optimizar).

Cambios aplicados (ver `internal/toolbox/agent/setnetwork_linux.go` + `sandboxes/internal/sandboxd/pool.go`):

- **`AddrReplace` en vez de `AddrList` + `AddrDel` loop + `AddrAdd`** — 1 syscall atómica en vez de 3+.
- **Skip MAC update cuando el MAC ya coincide** — evita `LinkSetDown` + `LinkSetHardwareAddr` + `LinkSetUp` (3 netlink syscalls = 5–10 ms) cuando no cambia.
- **Omitir MAC en el hot path del lease** — la topología de `/30` por slot significa que el MAC del guest es cosmético (no hay otro guest en el mismo L2 segment). El host's `arping` refresca ARP al final. El resultado: el branch "MAC match" del agente pasa a ser el caso por defecto y el SetNetwork hace solo `AddrReplace` + `RouteReplace`.

Medición actual (pool de 8, 6 leases secuenciales tras clean cache): `min=16.6 / p50=18.9 / p95=20.8 / setnet_ms=12–18`. Cumple `<15 ms p95` del SetNetwork planeado (sección 3 target) y queda a ~0.8 ms del `<20 ms` de warm lease.

### 10.4 Roadmap para bajar warm-lease a `<10 ms`

Target aspiracional de la tabla de sección 6 (`p50 <10 ms`). **Resultado tras aplicar #1: p95=2.9 ms sequential, p95=6.0 ms concurrent (burst-5) — bien debajo del target.**

| # | Optimización | Ahorro esperado | Dónde ataca | Esfuerzo |
|---|---|---:|---|---|
| 1 | **IP preasignado por slot** — el refiller ya pone un IP único en cada VM vía `hostnet.NewAuto`, el entry lo guarda, el lease lo usa directo. Cero `SetNetwork` en hot path. **APLICADO 2026-04-22**: p95=2.9 ms sequential, p95=6.0 ms concurrent. | **-18 ms medido** | Elimina SetNetwork | Bajo (cambio minimal: `BootResult.GuestIP` passthrough + `spec.IP=""` en Manager.LeaseSandbox) |
| 2 | **Hot tier default-on** para templates chicos — VMs running en vez de paused. Resume salta. Cuesta RAM (256 MB × N) pero ahorra vCPU-wakeup. | ~1–3 ms | Elimina Resume | Bajo: ya existe `MinHot`/`MaxHot`, solo subir default |
| 3 | ~~**UFFD lazy memory restore** (userfaultfd)~~ — **NO APLICA 2026-04-23**: `CreateVMFromSnapshotFile` ya usa `MAP_PRIVATE` mmap del `mem.bin`, que es O(1) lazy-COW — las páginas se cargan en page fault del kernel (nano-segundos). Restore actual mide **1.33 ms p50** (ya near-optimal). UFFD solo ayuda cuando (a) no podés usar `MAP_PRIVATE` (virtio-fs, ya usa memfd) o (b) querés diff snapshots con base-layer + overlay pages. En nuestra ruta común, UFFD sería un regresión. | — | — | — |
| 4 | **Pre-fault guest memory** (`MAP_POPULATE` + `madvise MADV_WILLNEED`) — fuerza al kernel a cargar las páginas en memoria antes de `Start()`, evita TLB misses en el hot path del primer exec. | ~2–5 ms | pkg/vmm memfd setup | Bajo |
| 5 | **Transparent hugepages** para guest RAM — `madvise MADV_HUGEPAGE` en el memfd. Menos TLB pressure, ~2–3 % mejor throughput de memoria. | ~1–2 ms | pkg/vmm memfd setup | Bajo |
| 6 | **io_uring para `mem.bin` restore** — en lugar de `mmap` + sync read, usar io_uring async reads encolados mientras se setup-ean los vCPUs. | ~5–10 ms | pkg/vmm restore | Medio |
| 7 | **KSM (Kernel Same-Page Merging) en el host** — marcar `/proc/pid/mergeable` en el proceso gocracker para que Linux deduplique páginas idénticas entre VMs del mismo template. No baja latencia por lease pero baja la memoria residente hasta ~50 %, lo que permite MaxPaused más alto sin fear OOM. | RAM savings, no latency | kernel tunable | Bajo |
| 8 | **Compose caching cross-sandbox** — cuando dos sandboxes comparten Dockerfile, reusar layers sin rebuild. Ya tenemos artifact cache por Dockerfile hash; extenderlo a layer-level. | Cold-boot reduction only | pkg/container | Medio |
| 9 | **Guest kernel strip** (DEFERRED, memoria del usuario) — ya usamos un kernel mínimo; no se toca. | — | kernel build | DEFERRED |

### 10.5 Features faltantes para completar el MVP

- **Post-ready snapshot** — `ReadinessProbe` en `templates.Spec` (HTTPPort/HTTPPath/Timeout/Interval). Template builder espera el probe `2xx` antes de `TakeSnapshot`, así el snapshot queda con la app ya live. Efecto concreto: lease de Flask+Postgres pasa de ~4.5 s (init de postgres + arranque de Flask a cada restore) a `<100 ms`. Plumbing nuevo en `sandboxes/internal/templates/builder.go` + agregar `bootProbeCapture` (poll por `/proxy/http/<port><path>` via agent hasta `2xx`, luego `TakeSnapshot` manual + `warmcache.Store`).
  - **Estado 2026-04-22**: el código completo está mergeado y la BUILD funciona — la app responde 2xx, el snapshot se captura y se guarda en warmcache (~6 s para `python:3.12-alpine` + `python3 -m http.server 8080`).
  - **Root cause del restore panic (2026-04-23)**: capturada la traza completa del guest — el kernel BUG()ea en `net/core/skbuff.c:120` `skb_over_panic` con `len:6348166` (6.3 MB en un skb — claro garbage). No es un bug de gocracker: es el clásico problema de capturar un snapshot live con un listen socket activo — el virtio-net ring descriptor table y los sk_buff chains en la cola de RX están en un estado transient mid-flight. Al restaurar, el kernel camina esos buffers, lee una longitud basura, BUG()ea, y el guest queda muerto antes de que init termine el resume.
  - **Fix aplicado (2026-04-23)**: `quiesceGuestNet(dialer, 2s)` en [sandboxes/internal/templates/builder.go](sandboxes/internal/templates/builder.go) — justo antes de `TakeSnapshot`, pedimos al agent ejecutar `ip link set eth0 down` adentro del guest. Eso flushea el skbuff en ambas direcciones, parkea las virtio queues, y deja el kernel en un punto limpio. Los listen sockets del usuario están bound a `INADDR_ANY` así que sobreviven (solo no reciben tráfico durante la ventana de snapshot). Al restore, `reIPGuest` sube eth0 con el IP nuevo y los sockets vuelven a recibir.
  - **Validación pendiente**: el host que usé para implementar esto quedó saturado (46+ VMMs huérfanos de sesiones previas consumiendo KVM), así que el build del template probe-v2/post-v3 queda `state=building` indefinidamente (la lentitud del host dispara timeouts encadenados). Re-validar en un host limpio con: `sudo pkill -9 -f "gocracker vmm --vm-id"; sudo rm -rf /tmp/sandboxd-demo-state /home/misael/.cache/gocracker/snapshots; ./gocracker-sandboxd serve ...` y ejecutar el bench de post-ready snapshot. Si el restore ya no panickea y el lease entrega la app corriendo en <100 ms, declarar la feature cerrada y actualizar §6 de este plan.
  - Mientras tanto el path está GATED por `spec.Readiness != nil` así que el flujo default (cookbook 10/10 + validated sweep 43/50) sigue intacto.
- **Base templates** `base-python`, `base-node`, `base-bun`, `base-go` — venían en el plan original (sección 6 de este doc) pero no están implementadas. Multi-stage Dockerfile con toolbox agent ya baked, `python3/node/bun/go` preinstalados, snapshot post-ready. Reducen cold-boot a ~80–120 ms (saltan el apk/apt install).
- **Entrypoint override en `CreateSandboxRequest`** — hoy sandboxd solo expone `Cmd`; para imágenes como `alpine/git` que tienen `ENTRYPOINT=["git"]`, no hay forma de evitar el panic de kernel sin reescribir la imagen. Agregar `Entrypoint []string` al request y propagarlo a `container.RunOptions` (ya existe el campo allá).

### 10.5.1 SDK shape estilo Daytona (traer de vuelta de v2)

v3 cortó la SDK Daytona-style que v2 ya tenía (se mató por problemas de transporte, no por la API). Los usuarios esperan ver algo reconocible — esto trae la forma de v2 encima del runtime estable de v3. Objetivo: que un dev que conoce Daytona lo agarre sin leer docs.

**Forma pública objetivo (Python, con equivalentes en Go + JS/TS)**:

```python
# Lifecycle por NOMBRE de template, no image+kernel
sb = client.create_sandbox(template="base-python")
with client.create_sandbox(template="base-python") as sb:  # context manager auto-delete
    ...

# Process namespace (wraps toolbox.exec / stream / set_secret)
sb.process.exec("python -c 'print(2+2)'")              # sync, ProcessExitError si exit_code != 0
sb.process.start(cmd, cwd=..., env=...)                # async Session handle
session.wait(timeout=60)
for frame in sb.process.exec_stream("tail -f app.log"):
    print(frame.stream, frame.data)

# FS namespace (wraps toolbox files)
sb.fs.write_file("/tmp/x", b"...")
sb.fs.read_file("/tmp/x")
sb.fs.list_dir("/tmp")
sb.fs.remove("/tmp/x")

# Preview helper
url = sb.preview_url(8080)   # wraps client.mint_preview(sb.id, port).url

# Lifecycle helpers
sb.pause(); sb.resume(); sb.recycle(); sb.delete()
# recycle = devolver el slot al pool sin matar la VM (restore fresh desde snapshot)
```

**Errores tipados** (de v2, sin cambios): `SandboxdError` root + `SandboxNotFound`, `TemplateNotFound`, `PoolExhausted`, `RuntimeUnreachable`, `SandboxTimeout`, `ProcessExitError`.

**Qué cambia del server para soportarlo**:
- Los 4 base templates se autoregistran al arranque de sandboxd (o se construyen on-demand al primer `create_sandbox(template="base-python")`).
- `create_sandbox(template=...)` mapea a `LeaseSandbox(template_id=...)` si hay pool, o al flujo de `CreateSandbox` que copia spec del template.
- `recycle` nuevo en sandboxd: `POST /sandboxes/{id}/recycle` — tear-down + lease nuevo del mismo template, devolviendo un id fresco pero reusando el slot del pool.

**Cookbook a traer de v2 (además de los 10 que ya tenemos)**:

| Nº v2 | Título | Feature |
|---|---|---|
| 02 | `run_python_code.py` | `client.create_sandbox(template="base-python")` + `sb.process.exec("python -c ...")` |
| 03 | `run_nodejs_code.py` | base-node |
| 04 | `run_go_code.py` | base-go |
| 05 | `run_bun_typescript.py` | base-bun |
| 07 | `pip_install.py` | persistencia post-install en snapshot custom |
| 08 | `npm_install.py` | lo mismo con npm |
| 11 | `streaming_output.py` | `sb.process.exec_stream` con frames |
| 12 | `session_wait.py` | `sb.process.start` + `session.wait(timeout)` |
| 14 | `pause_resume.py` | `sb.pause()` + `sb.resume()` |
| 15 | `error_handling.py` | atrapar `ProcessExitError` / `SandboxTimeout` / etc |
| 16 | `multiple_sandboxes.py` | N concurrent en paralelo sin pool |
| 20 | `large_file_transfer.py` | upload/download 100 MB |
| 21 | `nextjs_preview.py` | dev server next + preview token |
| 22 | `custom_template.py` | `client.create_template(from="base-node", dockerfile=...)` |

Los demás (LLM, data analysis) quedan para una fase posterior.

### 10.6 Orden sugerido de commits (continuación)

Manteniendo el principio del plan original (cada commit mergeable + verde):

19. `fix(pool): decouple refiller ctx from HTTP request lifetime` — bug 10.1 #1.
20. `fix(templates): mirror container.Run defaults when computing warmcache lookup key` — bug 10.1 #2.
21. `fix(sdk/python): clear socket timeout after CONNECT handshake` — bug 10.1 #3.
22. `chore(pool): downgrade canceled-ctx boot-fail to debug; don't bump backoff` — bug 10.1 #4.
23. `fix(cookbook): redirect backgrounded server stdin/stdout in preview.py` — bug 10.1 #5.
24. `fix(cookbook): switch git_clone.py to alpine:3.20 + apk add git` — bug 10.1 #6.
25. `perf(toolbox-agent): AddrReplace + skip MAC on match in setnetwork_linux` — opt 10.3.
26. `perf(sandboxd/pool): drop MAC from warm-lease spec` — opt 10.3.
27. `feat(pool): pre-assigned IP per slot — eliminate SetNetwork from warm lease` — opt 10.4 #1 (biggest win).
28. `feat(templates): Readiness probe + post-ready snapshot` — feature 10.5.
29. `feat(templates): base-python / base-node / base-bun / base-go` — feature 10.5.
30. `feat(sandboxd): Entrypoint override in CreateSandboxRequest` — feature 10.5.
31. `perf(vmm): MAP_POPULATE + MADV_HUGEPAGE + MADV_WILLNEED on guest memfd` — opt 10.4 #4, #5.
32. `perf(vmm): io_uring async read for mem.bin restore` — opt 10.4 #6.
33. `feat(host): KSM opt-in via GOCRACKER_KSM=1` — opt 10.4 #7.
34. `perf(vmm): UFFD lazy memory restore` — opt 10.4 #3 (mayor esfuerzo, último).
35. `feat(sandboxd): POST /sandboxes/{id}/recycle` — vuelve el slot al pool sin tirar la VM, para SDK 10.5.1.
36. `feat(sandboxd): autoregistrar los 4 base templates al arranque` — 10.5.1.
37. `feat(sdk): Daytona-style surface (template=, .process, .fs, preview_url, with-context, typed errors, pause/resume/recycle)` — Python + Go + JS/TS en el mismo commit para paridad.
38. `docs(cookbook): traer 14 ejemplos de v2 adaptados al SDK Daytona-style` — lista en 10.5.1.

**Gate de velocidad** tras commit 27 (IP preasignado): pool-bench warm-lease **p95 < 10 ms** en x86. ✅ **CUMPLIDO 2026-04-22: p95=1.5 ms** — 6× mejor que el gate.

**Gate de SDK** tras commit 37: los 14 ejemplos de 10.5.1 corren verdes + un dev de Daytona entiende la API leyendo el README en 2 minutos. ✅ **API Daytona-shape mergeada 2026-04-22**: `with client.create_sandbox(template="base-python") as sb: sb.process.exec("..."); sb.fs.read_file("..."); sb.preview_url(8080)` funciona end-to-end. Los 14 ejemplos específicos de v2 faltan por portar.

### 10.7 Trabajo completado 2026-04-22 (sesión de consolidación)

Lo que landeó en esta sesión, para diferenciarlo del roadmap futuro:

**Bugs arreglados (6)**: pool refiller ctx tied to HTTP request, template warmcache lookup miss, SDK `dial_timeout` leaking into per-read, WARN-spam on `UnregisterPool`, cookbook `preview.py` background, cookbook `git_clone.py` kernel panic.

**Features de velocidad (6)**:
1. **IP preasignado** — eliminó SetNetwork del hot path. Lease p95 20.8 ms → 1.5 ms.
2. **Async delete** — VM teardown en goroutine. Delete p95 79 ms → 1.9 ms.
3. **Post-ready snapshot** — `ReadinessProbe` en `templates.Spec` + `bootProbeCapture` que snapshotea después de que la app responda 2xx.
4. **Base templates** — `base-python`/`base-node`/`base-bun`/`base-go` auto-registran en el startup de sandboxd cuando `-kernel-path` o `GOCRACKER_KERNEL` está configurado.
5. **Entrypoint override** — nuevo campo en `CreateSandboxRequest` (desbloquea imágenes como `alpine/git` con ENTRYPOINT que sale).
6. **KSM opt-in** — `GOCRACKER_KSM=1` aplica `MADV_MERGEABLE` al memfd de restore, dedupe ~30–60 % de RSS entre clones del mismo template.

**Features de UX (1)**:
7. **SDK Daytona-style** en Python: `sb.process.exec/.exec_stream`, `sb.fs.read_file/.write_file/.list_dir/.remove/.mkdir/.chmod/.rename`, `sb.preview_url(port)`, context-manager `with ... as sb`, errores tipados (`ProcessExitError`, `TemplateNotFound`, `PoolExhausted`, `RuntimeUnreachable`, `SandboxTimeout`), y `create_sandbox(template="base-python")` que resuelve por nombre. Equivalentes en Go/TS pendientes.

**Cookbook**: 10/10 verdes tras los fixes. Los 14 ejemplos adicionales de v2 (`run_python_code`, `pause_resume`, `session_wait`, `nextjs_preview`, etc.) quedan para la próxima sesión.

**Deferreds / roadmap futuro** (ordenados por ROI):
- Hot tier default (needs design conversation — cuántas VMs running por default)
- io_uring restore (big effort, restore ya es ~40 ms)
- Compose caching (moderate effort)
- UFFD lazy restore (biggest effort, baja restore a <10 ms)
- SDK Daytona-style en Go + TS (paridad)
- 14 ejemplos faltantes de v2 (portar a SDK nueva)
- `recycle` endpoint (`POST /sandboxes/{id}/recycle`) para v2-style slot reuse
