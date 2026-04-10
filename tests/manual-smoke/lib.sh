#!/usr/bin/env bash

set -euo pipefail

GC_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
: "${GC_BIN:=./gocracker}"
: "${SMOKE_CASES:=all}"
: "${SMOKE_MEM_MB:=128}"
: "${SMOKE_PTY_HELPER:=}"
: "${SMOKE_RUN_TAG:=}"
: "${GOCRACKER_VIRTIOFS_KERNEL:=$GC_REPO_ROOT/artifacts/kernels/gocracker-guest-virtiofs-vmlinux}"

if [[ -z "${SMOKE_LOG_DIR:-}" ]]; then
  SMOKE_LOG_DIR="/tmp/gocracker-manual-smoke/$(date +%Y%m%d-%H%M%S)"
fi

CASE_RESULTS=()
FAILURES=0

fail() {
  echo "error: $*" >&2
  return 1
}

quote_cmd() {
  local out=""
  local arg
  for arg in "$@"; do
    out+="$(printf '%q ' "$arg")"
  done
  printf '%s' "${out% }"
}

init_smoke_env() {
  mkdir -p "$SMOKE_LOG_DIR"
  if [[ -z "$SMOKE_RUN_TAG" ]]; then
    SMOKE_RUN_TAG="$(basename "$SMOKE_LOG_DIR")"
    SMOKE_RUN_TAG="${SMOKE_RUN_TAG//[^a-zA-Z0-9._-]/-}"
  fi
  if [[ -z "$SMOKE_PTY_HELPER" ]]; then
    SMOKE_PTY_HELPER="$SMOKE_LOG_DIR/gocracker-smoke-pty"
  fi
}

require_cmd() {
  local cmd=$1
  command -v "$cmd" >/dev/null 2>&1 || fail "missing required command: $cmd"
}

require_kernel() {
  [[ -n "${GOCRACKER_KERNEL:-}" ]] || fail "GOCRACKER_KERNEL is required"
  [[ -f "$GOCRACKER_KERNEL" ]] || fail "kernel not found: $GOCRACKER_KERNEL"
  if [[ "$GOCRACKER_KERNEL" != /* ]]; then
    GOCRACKER_KERNEL="$(cd "$(dirname "$GOCRACKER_KERNEL")" && pwd)/$(basename "$GOCRACKER_KERNEL")"
  fi
}

require_sudo_ready() {
  if [[ ${EUID:-$(id -u)} -eq 0 ]]; then
    return 0
  fi
  if [[ -x "$GC_REPO_ROOT/gocracker" ]] && sudo -n "$GC_REPO_ROOT/gocracker" --help >/dev/null 2>&1; then
    return 0
  fi
  sudo -n true >/dev/null 2>&1 || fail "sudo credentials are not cached. Run 'sudo -v' first."
}

build_gocracker() {
  local bin_path=$GC_BIN
  local repo_bin_dir vmm_path jailer_path
  if [[ "$bin_path" != /* ]]; then
    bin_path="$GC_REPO_ROOT/${bin_path#./}"
  fi
  repo_bin_dir="$(dirname "$bin_path")"
  vmm_path="$repo_bin_dir/gocracker-vmm"
  jailer_path="$repo_bin_dir/gocracker-jailer"
  mkdir -p "$repo_bin_dir"
  (
    cd "$GC_REPO_ROOT"
    go build -o "$bin_path" ./cmd/gocracker
    go build -o "$vmm_path" ./cmd/gocracker-vmm
    go build -o "$jailer_path" ./cmd/gocracker-jailer
  )
  GC_BIN="$bin_path"
}

build_smoke_pty_helper() {
  local helper_path=$SMOKE_PTY_HELPER
  if [[ "$helper_path" != /* ]]; then
    helper_path="$GC_REPO_ROOT/${helper_path#./}"
  fi
  mkdir -p "$(dirname "$helper_path")"
  (cd "$GC_REPO_ROOT" && go build -o "$helper_path" ./tests/manual-smoke/cmd/ptyrun)
  SMOKE_PTY_HELPER="$helper_path"
}

build_privileged_cmd() {
  local out_name=$1
  shift
  local -n out_ref="$out_name"
  if [[ ${EUID:-$(id -u)} -eq 0 ]]; then
    out_ref=("$@")
    return 0
  fi
  out_ref=(sudo "$@")
}

case_selected() {
  local group=$1
  if [[ "$SMOKE_CASES" == "all" ]]; then
    return 0
  fi
  [[ ",$SMOKE_CASES," == *",$group,"* ]]
}

case_id() {
  local name=$1
  name="${name//[^a-zA-Z0-9._-]/-}"
  printf 'manual-%s-%s' "$name" "$SMOKE_RUN_TAG"
}

case_workdir() {
  printf '/tmp/gocracker-%s' "$(case_id "$1")"
}

case_disk() {
  printf '%s/disk.ext4' "$(case_workdir "$1")"
}

case_log() {
  printf '%s/%s.log' "$SMOKE_LOG_DIR" "$(case_id "$1")"
}

debugfs_read() {
  local disk=$1
  local path=$2
  debugfs -R "cat $path" "$disk" 2>/dev/null
}

debugfs_exists() {
  local disk=$1
  local path=$2
  debugfs -R "stat $path" "$disk" >/dev/null 2>&1
}

extract_disk_from_log() {
  local log=$1
  sed -n 's/^[[:space:]]*disk:[[:space:]]*//p' "$log" 2>/dev/null | tail -n 1
}

resolve_disk_from_log() {
  local log=$1
  local disk
  disk="$(extract_disk_from_log "$log")"
  if [[ -z "$disk" ]]; then
    fail "disk path not found in $log"
    return 1
  fi
  if [[ ! -f "$disk" ]]; then
    fail "disk not found in log-reported path: $disk"
    return 1
  fi
  printf '%s' "$disk"
}

assert_log_contains() {
  local log=$1
  local want=$2
  grep -Fq "$want" "$log" || fail "expected '$want' in $log"
}

assert_log_not_contains() {
  local log=$1
  local needle=$2
  if grep -Fq "$needle" "$log"; then
    fail "did not expect '$needle' in $log"
  fi
}

assert_disk_has_line() {
  local disk=$1
  local path=$2
  local want=$3
  local data
  data="$(debugfs_read "$disk" "$path" | tr -d '\r')"
  grep -Fxq "$want" <<<"$data" || fail "expected $path in $disk to contain line '$want'"
}

assert_disk_missing() {
  local disk=$1
  local path=$2
  if debugfs_exists "$disk" "$path"; then
    fail "expected $path to be absent from $disk"
  fi
}

assert_file_has_line() {
  local path=$1
  local want=$2
  [[ -f "$path" ]] || fail "expected file to exist: $path"
  grep -Fxq "$want" "$path" || fail "expected $path to contain line '$want'"
}

wait_disk_has_line() {
  local log=$1
  local path=$2
  local want=$3

  wait_until 30 bash -lc '
    log="$1"
    guest_path="$2"
    want="$3"
    disk=$(grep -o "/tmp/gocracker-[^[:space:]]*/disk.ext4" "$log" 2>/dev/null | tail -n 1)
    [[ -n "$disk" ]] || exit 1
    debugfs -R "cat $guest_path" "$disk" 2>/dev/null | tr -d "\r" | grep -Fxq "$want"
  ' _ "$log" "$path" "$want"
}

run_with_script() {
  local log=$1
  shift
  local cmd
  cmd="$(quote_cmd "$@")"
  script -qefc "$cmd" "$log"
}

run_with_script_input() {
  local input=$1
  local log=$2
  shift 2
  local cmd
  cmd="$(quote_cmd "$@")"
  script -qefc "$cmd" "$log" < "$input"
}

wait_log_regex() {
  local log=$1
  local pattern=$2
  wait_until 30 bash -lc "grep -Eq '$pattern' '$log'"
}

wait_until() {
  local seconds=$1
  shift
  local deadline=$((SECONDS + seconds))
  while (( SECONDS < deadline )); do
    if "$@"; then
      return 0
    fi
    sleep 1
  done
  "$@"
}

wait_log_contains() {
  local log=$1
  local needle=$2
  local seconds=${3:-30}
  wait_until "$seconds" bash -lc "grep -Fq '$needle' '$log' 2>/dev/null"
}

wait_interactive_shell_ready() {
  local log=$1
  wait_until 45 bash -lc 'grep -Eq "can.?t access tty; job control turned off|no job control in this shell" "$1" 2>/dev/null' _ "$log"
}

wait_http_contains() {
  local url=$1
  local needle=$2
  local seconds=${3:-30}
  wait_until "$seconds" bash -lc "curl -fsS '$url' | grep -Fq '$needle'"
}

wait_http_ok() {
  local url=$1
  wait_until 30 bash -lc "curl -fsS '$url' >/dev/null"
}

wait_for_child_pid() {
  local parent_pid=$1
  local child_pid=""
  if wait_until 10 bash -lc "pgrep -P '$parent_pid' >/dev/null 2>&1"; then
    child_pid="$(pgrep -P "$parent_pid" | head -n 1 || true)"
  fi
  printf '%s' "$child_pid"
}

wait_for_pid_exit() {
  local pid=$1
  local seconds=${2:-15}
  local deadline=$((SECONDS + seconds))
  while (( SECONDS < deadline )); do
    if ! kill -0 "$pid" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  ! kill -0 "$pid" >/dev/null 2>&1
}

stop_background_command() {
  local parent_pid=$1
  local child_pid=${2:-}
  local rc

  if [[ -n "$child_pid" ]]; then
    kill -INT "$child_pid" >/dev/null 2>&1 || true
  fi
  kill -INT "$parent_pid" >/dev/null 2>&1 || true

  if ! wait_for_pid_exit "$parent_pid" 15; then
    if [[ -n "$child_pid" ]]; then
      kill -TERM "$child_pid" >/dev/null 2>&1 || true
    fi
    kill -TERM "$parent_pid" >/dev/null 2>&1 || true
  fi

  if ! wait_for_pid_exit "$parent_pid" 10; then
    if [[ -n "$child_pid" ]]; then
      kill -KILL "$child_pid" >/dev/null 2>&1 || true
    fi
    kill -KILL "$parent_pid" >/dev/null 2>&1 || true
  fi

  if wait "$parent_pid"; then
    rc=0
  else
    rc=$?
  fi

  case "$rc" in
    0|130|143)
      return 0
      ;;
    *)
      fail "background command exited with unexpected status $rc"
      ;;
  esac
}

start_compose_case() {
  local out_name=$1
  local name=$2
  local compose_file=$3
  local log=$4
  local -a cmd
  local -n out_ref="$out_name"
  shift 4

  build_privileged_cmd cmd "$GC_BIN" compose \
    --file "$compose_file" \
    --kernel "$GOCRACKER_KERNEL" \
    --mem 256 \
    --wait \
    "$@"
  (
    cd "$(dirname "$compose_file")"
    exec "${cmd[@]}"
  ) >"$log" 2>&1 &
  out_ref="$!"
}

start_serve_case() {
  local out_name=$1
  local log=$2
  local port=$3
  local bin_dir jailer_bin vmm_bin
  local -a cmd
  local -n out_ref="$out_name"

  bin_dir="$(dirname "$GC_BIN")"
  jailer_bin="$bin_dir/gocracker-jailer"
  vmm_bin="$bin_dir/gocracker-vmm"
  build_privileged_cmd cmd "$GC_BIN" serve \
    --addr "127.0.0.1:$port" \
    --cache-dir /tmp/gocracker/cache \
    --jailer-binary "$jailer_bin" \
    --vmm-binary "$vmm_bin"
  "${cmd[@]}" >"$log" 2>&1 &
  out_ref="$!"
}

run_case() {
  local group=$1
  local name=$2
  shift 2

  if ! case_selected "$group"; then
    return 0
  fi

  echo "==> $group/$name"
  if "$@"; then
    CASE_RESULTS+=("PASS $group/$name")
    echo "PASS $group/$name"
  else
    CASE_RESULTS+=("FAIL $group/$name")
    FAILURES=$((FAILURES + 1))
    echo "FAIL $group/$name"
  fi
}

prepare_case_dir() {
  local name=$1
  rm -rf "$(case_workdir "$name")"
}

run_interactive_image_case() {
  local name=$1
  local image=$2
  local log input disk_file disk
  local -a cmd
  log="$(case_log "$name")"
  input="$SMOKE_LOG_DIR/$(case_id "$name").input"
  disk_file="$SMOKE_LOG_DIR/$(case_id "$name").disk"

  prepare_case_dir "$name"
  cat >"$input" <<'EOF'
printf 'READY\n' > /marker.txt; pwd > /pwd.txt; exit
EOF
  rm -f "$disk_file"

  build_privileged_cmd cmd "$GC_BIN" run \
    --id "$(case_id "$name")" \
    --image "$image" \
    --kernel "$GOCRACKER_KERNEL" \
    --mem "$SMOKE_MEM_MB" \
    --wait

  if ! "$SMOKE_PTY_HELPER" \
    --log "$log" \
    --input "$input" \
    --disk-path-file "$disk_file" \
    --ready-timeout 60s \
    --exit-timeout 90s \
    -- "${cmd[@]}"; then
    return 1
  fi

  if [[ ! -s "$disk_file" ]]; then
    fail "interactive helper did not emit disk path for $name"
    return 1
  fi
  disk="$(tr -d '\r\n' <"$disk_file")"
  [[ -f "$disk" ]] || {
    fail "disk not found in helper-reported path: $disk"
    return 1
  }
  assert_disk_has_line "$disk" /marker.txt READY
  assert_disk_has_line "$disk" /pwd.txt /
}

run_timeout_case() {
  local name=$1
  shift
  local log
  log="$(case_log "$name")"
  prepare_case_dir "$name"
  : >"$log"

  local cmd
  cmd="$(quote_cmd "$@")"
  if timeout 10s script -qefc "$cmd" "$log"; then
    fail "expected timeout for $name"
    return 1
  else
    local rc=$?
    [[ $rc -eq 124 ]] || {
      fail "expected timeout exit code 124 for $name, got $rc"
      return 1
    }
  fi
  assert_log_not_contains "$log" "System halted"
}

run_nginx_case() {
  local -a cmd
  build_privileged_cmd cmd "$GC_BIN" run \
    --id "$(case_id nginx-latest)" \
    --image nginx:latest \
    --kernel "$GOCRACKER_KERNEL" \
    --mem 256 \
    --wait
  run_timeout_case nginx-latest "${cmd[@]}"
}

run_override_case() {
  local name=python-override
  local log
  local -a cmd
  log="$(case_log "$name")"
  prepare_case_dir "$name"

  build_privileged_cmd cmd "$GC_BIN" run \
    --id "$(case_id "$name")" \
    --image python:3.11-slim \
    --kernel "$GOCRACKER_KERNEL" \
    --mem 256 \
    --cmd 'python3 -c "print(123)"' \
    --wait
  run_with_script "$log" "${cmd[@]}"

  assert_log_contains "$log" "123"
}

run_dockerfile_service_case() {
  local name=$1
  local dockerfile=$2
  local context_dir=$3
  local expected=${4:-}
  local log
  local -a cmd
  log="$(case_log "$name")"

  build_privileged_cmd cmd "$GC_BIN" run \
    --id "$(case_id "$name")" \
    --dockerfile "$dockerfile" \
    --context "$context_dir" \
    --kernel "$GOCRACKER_KERNEL" \
    --mem 256 \
    --wait
  run_timeout_case "$name" "${cmd[@]}"

  if [[ -n "$expected" ]]; then
    assert_log_contains "$log" "$expected"
  fi
}

run_shellform_fixture_case() {
  local name=shellform-fixture
  local dockerfile="$GC_REPO_ROOT/tests/manual-smoke/fixtures/shellform/Dockerfile"
  local context_dir
  local disk
  local -a cmd
  context_dir="$(dirname "$dockerfile")"
  disk="$(case_disk "$name")"

  prepare_case_dir "$name"
  build_privileged_cmd cmd "$GC_BIN" run \
    --id "$(case_id "$name")" \
    --dockerfile "$dockerfile" \
    --context "$context_dir" \
    --kernel "$GOCRACKER_KERNEL" \
    --mem "$SMOKE_MEM_MB" \
    --wait
  run_with_script "$(case_log "$name")" "${cmd[@]}"

  disk="$(resolve_disk_from_log "$(case_log "$name")")" || return 1
  assert_disk_has_line "$disk" /result.txt shellform-ok
}

run_user_fixture_case() {
  local name=user-fixture
  local dockerfile="$GC_REPO_ROOT/tests/manual-smoke/fixtures/user/Dockerfile"
  local context_dir
  local disk
  local -a cmd
  context_dir="$(dirname "$dockerfile")"
  disk="$(case_disk "$name")"

  prepare_case_dir "$name"
  build_privileged_cmd cmd "$GC_BIN" run \
    --id "$(case_id "$name")" \
    --dockerfile "$dockerfile" \
    --context "$context_dir" \
    --kernel "$GOCRACKER_KERNEL" \
    --mem "$SMOKE_MEM_MB" \
    --wait
  run_with_script "$(case_log "$name")" "${cmd[@]}"

  disk="$(resolve_disk_from_log "$(case_log "$name")")" || return 1
  assert_disk_has_line "$disk" /work/build-user.txt 1000
  assert_disk_has_line "$disk" /work/runtime-user.txt 1000
  assert_disk_has_line "$disk" /work/pwd.txt /work
}

run_compose_basic_case() {
  local name=compose-basic
  local compose_file="$GC_REPO_ROOT/tests/manual-smoke/fixtures/compose-basic/docker-compose.yml"
  local log parent_pid child_pid
  log="$(case_log "$name")"

  prepare_case_dir "$name"
  start_compose_case parent_pid "$name" "$compose_file" "$log"
  child_pid="$(wait_for_child_pid "$parent_pid")"

  wait_log_contains "$log" "Stack started:" || {
    stop_background_command "$parent_pid" "$child_pid" || true
    fail "stack did not report startup for $name"
    return 1
  }
  wait_http_contains "http://127.0.0.1:18080/" "compose-basic" || {
    stop_background_command "$parent_pid" "$child_pid" || true
    fail "HTTP probe failed for $name"
    return 1
  }

  stop_background_command "$parent_pid" "$child_pid"
}

run_compose_health_case() {
  local name=compose-health
  local compose_file="$GC_REPO_ROOT/tests/manual-smoke/fixtures/compose-health/docker-compose.yml"
  local log parent_pid child_pid
  log="$(case_log "$name")"

  prepare_case_dir "$name"
  start_compose_case parent_pid "$name" "$compose_file" "$log"
  child_pid="$(wait_for_child_pid "$parent_pid")"

  wait_log_contains "$log" "Stack started:" || {
    stop_background_command "$parent_pid" "$child_pid" || true
    fail "stack did not report startup for $name"
    return 1
  }
  assert_log_contains "$log" "web"
  assert_log_contains "$log" "app"
  assert_log_not_contains "$log" "did not become healthy"

  stop_background_command "$parent_pid" "$child_pid"
}

run_compose_volume_case() {
  local name=compose-volume
  local fixture_dir="$GC_REPO_ROOT/tests/manual-smoke/fixtures/compose-volume"
  local case_dir compose_dir compose_file data_dir data_file writer_bin virtiofs_kernel
  local log parent_pid child_pid
  log="$(case_log "$name")"

  prepare_case_dir "$name"
  case_dir="$(case_workdir "$name")"
  compose_dir="$case_dir/compose-volume"
  compose_file="$compose_dir/docker-compose.yml"
  data_dir="$compose_dir/data"
  data_file="$data_dir/result.txt"
  writer_bin="$compose_dir/writer"
  virtiofs_kernel="$GOCRACKER_VIRTIOFS_KERNEL"
  if [[ "$virtiofs_kernel" != /* ]]; then
    virtiofs_kernel="$(cd "$(dirname "$virtiofs_kernel")" && pwd)/$(basename "$virtiofs_kernel")"
  fi
  [[ -f "$virtiofs_kernel" ]] || {
    fail "virtio-fs kernel not found: $virtiofs_kernel"
    return 1
  }

  mkdir -p "$compose_dir" "$data_dir"
  cp "$fixture_dir/docker-compose.yml" "$compose_file"
  cp "$fixture_dir/Dockerfile" "$compose_dir/Dockerfile"
  (cd "$fixture_dir" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$writer_bin" .)
  rm -f "$data_file"
  start_compose_case parent_pid "$name" "$compose_file" "$log" \
    --kernel "$virtiofs_kernel" \
    --x86-boot acpi
  child_pid="$(wait_for_child_pid "$parent_pid")"

  wait_log_contains "$log" "Stack started:" || {
    stop_background_command "$parent_pid" "$child_pid" || true
    fail "stack did not report startup for $name"
    return 1
  }
  sleep 8
  stop_background_command "$parent_pid" "$child_pid"
  assert_file_has_line "$data_file" "compose-volume"
}

run_compose_todo_postgres_case() {
  local name=compose-todo-postgres
  local compose_file="$GC_REPO_ROOT/tests/manual-smoke/fixtures/compose-todo-postgres/docker-compose.yml"
  local log parent_pid child_pid
  log="$(case_log "$name")"

  prepare_case_dir "$name"
  start_compose_case parent_pid "$name" "$compose_file" "$log"
  child_pid="$(wait_for_child_pid "$parent_pid")"

  wait_log_contains "$log" "Stack started:" 120 || {
    stop_background_command "$parent_pid" "$child_pid" || true
    fail "stack did not report startup for $name"
    return 1
  }
  wait_http_contains "http://127.0.0.1:18081/health" '"status":"ok"' 60 || {
    stop_background_command "$parent_pid" "$child_pid" || true
    fail "health probe failed for $name"
    return 1
  }
  curl -fsS \
    -X POST \
    -H 'Content-Type: application/json' \
    -d '{"title":"buy milk"}' \
    "http://127.0.0.1:18081/api/todos" | grep -Fq '"title":"buy milk"' || {
    stop_background_command "$parent_pid" "$child_pid" || true
    fail "create todo request failed for $name"
    return 1
  }
  wait_http_contains "http://127.0.0.1:18081/api/todos" '"title":"buy milk"' 60 || {
    stop_background_command "$parent_pid" "$child_pid" || true
    fail "todo list probe failed for $name"
    return 1
  }

  stop_background_command "$parent_pid" "$child_pid"
}

run_compose_exec_case() {
  local name=compose-exec
  local compose_file="$GC_REPO_ROOT/tests/manual-smoke/fixtures/compose-exec/docker-compose.yml"
  local serve_log compose_log exec_log serve_pid serve_child_pid server_url
  local port=18082
  local -a up_cmd down_cmd
  serve_log="$SMOKE_LOG_DIR/$(case_id "$name").serve.log"
  compose_log="$(case_log "$name")"
  exec_log="$SMOKE_LOG_DIR/$(case_id "$name").exec.log"
  server_url="http://127.0.0.1:$port"

  prepare_case_dir "$name"

  start_serve_case serve_pid "$serve_log" "$port"
  serve_child_pid="$(wait_for_child_pid "$serve_pid")"
  wait_http_ok "$server_url/vms" || {
    stop_background_command "$serve_pid" "$serve_child_pid" || true
    fail "API server did not become ready for $name"
    return 1
  }

  build_privileged_cmd up_cmd "$GC_BIN" compose \
    --server "$server_url" \
    --file "$compose_file" \
    --kernel "$GOCRACKER_KERNEL" \
    --cache-dir /tmp/gocracker/cache \
    --mem 256
  (
    cd "$(dirname "$compose_file")"
    exec "${up_cmd[@]}"
  ) >"$compose_log" 2>&1 || {
    stop_background_command "$serve_pid" "$serve_child_pid" || true
    fail "compose up failed for $name"
    return 1
  }

  # Note: stack name is suffixed with a fnv hash by projectName(); we filter
  # by service+orchestrator only since `debug` is unique within this fixture.
  curl -fsS "$server_url/vms?orchestrator=compose&service=debug" | \
    grep -Fq '"service_name":"debug"' || {
    stop_background_command "$serve_pid" "$serve_child_pid" || true
    fail "compose VM metadata was not discoverable for $name"
    return 1
  }

  "$GC_BIN" compose exec \
    --server "$server_url" \
    --file "$compose_file" \
    debug -- echo smoke-compose-exec-ok >"$exec_log" 2>&1 || {
    build_privileged_cmd down_cmd "$GC_BIN" compose down --server "$server_url" --file "$compose_file"
    (
      cd "$(dirname "$compose_file")"
      "${down_cmd[@]}"
    ) >/dev/null 2>&1 || true
    stop_background_command "$serve_pid" "$serve_child_pid" || true
    fail "compose exec command failed for $name"
    return 1
  }
  assert_log_contains "$exec_log" "smoke-compose-exec-ok"

  build_privileged_cmd down_cmd "$GC_BIN" compose down --server "$server_url" --file "$compose_file"
  (
    cd "$(dirname "$compose_file")"
    exec "${down_cmd[@]}"
  ) >/dev/null 2>&1 || {
    stop_background_command "$serve_pid" "$serve_child_pid" || true
    fail "compose down failed for $name"
    return 1
  }

  stop_background_command "$serve_pid" "$serve_child_pid"
}

print_summary() {
  echo
  echo "Summary"
  local line
  for line in "${CASE_RESULTS[@]}"; do
    echo "$line"
  done
  if (( FAILURES > 0 )); then
    return 1
  fi
}
