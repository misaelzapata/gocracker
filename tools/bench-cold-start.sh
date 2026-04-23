#!/usr/bin/env bash
# bench-cold-start.sh — measure cold-start latency of gocracker-sandboxd.
#
# Usage:
#   sudo tools/bench-cold-start.sh [--iterations N] [--template NAME] \
#                                  [--sandboxd URL] [--runtime URL] [--token T]
#
# What it measures:
#   end-to-end  — wall clock from POST /sandboxes to POST .../process/execute returning
#   per-phase   — orchestration / vmm_setup / kernel_boot / first_output from
#                 the runtime's /debug/vars expvars (only updated on cold boots)
#
# Prerequisites:
#   • sandboxd running at SANDBOXD_URL (default http://127.0.0.1:9090)
#   • runtime (gocracker serve) running at RUNTIME_URL (default http://127.0.0.1:8080)
#   • curl, jq, bc, awk on PATH
#   • template already seeded (the first run warms the rootfs cache)

set -euo pipefail

# ---- defaults ---------------------------------------------------------------
ITERATIONS=10
TEMPLATE="base-python"
SANDBOXD_URL="${SANDBOXD_URL:-http://127.0.0.1:9090}"
RUNTIME_URL="${RUNTIME_URL:-http://127.0.0.1:8080}"
TOKEN="${SANDBOXD_TOKEN:-}"
WARMUP=1  # number of throw-away runs to prime the rootfs cache

# ---- argument parsing -------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case $1 in
    --iterations|-n) ITERATIONS="$2"; shift 2 ;;
    --template|-t)   TEMPLATE="$2";   shift 2 ;;
    --sandboxd)      SANDBOXD_URL="$2"; shift 2 ;;
    --runtime)       RUNTIME_URL="$2"; shift 2 ;;
    --token)         TOKEN="$2";       shift 2 ;;
    --warmup)        WARMUP="$2";      shift 2 ;;
    -h|--help)
      sed -n '2,20p' "$0" | sed 's/^# *//'
      exit 0
      ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

# ---- helpers ----------------------------------------------------------------
AUTH_HEADER=()
if [[ -n "$TOKEN" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer $TOKEN")
fi

sdx() {  # sandboxd request
  curl -s -f "${AUTH_HEADER[@]}" "$@"
}

runtime_vars() {  # fetch expvars from runtime, return JSON
  curl -s -f "$RUNTIME_URL/debug/vars" 2>/dev/null || echo '{}'
}

snapshot_vars() {  # capture expvar snapshot before a run
  local raw
  raw="$(runtime_vars)"
  echo "$raw"
}

read_var() {  # read_var JSON_SNAPSHOT VARNAME
  echo "$1" | jq -r --arg k "$2" '.[$k] // 0'
}

delta_ms() {  # delta_ms BEFORE_JSON AFTER_JSON VARNAME
  local before after
  before="$(read_var "$1" "$3")"
  after="$(read_var "$2" "$3")"
  echo "$after - $before" | bc
}

delete_sandbox() {
  local id="$1"
  sdx -X DELETE "$SANDBOXD_URL/sandboxes/$id" > /dev/null || true
}

# ---- create + exec + delete, return wall-clock ms --------------------------
run_once() {
  local t_start t_end sb_id sb_json exec_result
  local e2e_ms

  t_start=$( date +%s%3N )

  # Create sandbox (pool lease → cold boot if pool is empty)
  sb_json=$(sdx -X POST "$SANDBOXD_URL/sandboxes" \
    -H "Content-Type: application/json" \
    -d "{\"template\":\"$TEMPLATE\"}" )
  sb_id=$(echo "$sb_json" | jq -r '.id')

  # Execute a trivial command — this is what the user waits for
  exec_result=$(sdx -X POST "$SANDBOXD_URL/sandboxes/$sb_id/process/execute" \
    -H "Content-Type: application/json" \
    -d '{"command":["/bin/sh","-c","true"]}' )

  t_end=$( date +%s%3N )
  e2e_ms=$(( t_end - t_start ))

  delete_sandbox "$sb_id"
  echo "$e2e_ms"
}

# ---- warmup -----------------------------------------------------------------
if [[ "$WARMUP" -gt 0 ]]; then
  echo "▶ Warming rootfs cache with $WARMUP throw-away run(s) ..." >&2
  for (( i=0; i<WARMUP; i++ )); do
    run_once > /dev/null
  done
  echo "  Warmup done." >&2
fi

# ---- baseline expvar snapshot -----------------------------------------------
before_json="$(snapshot_vars)"

# ---- measured runs ----------------------------------------------------------
echo ""
echo "▶ Measuring $ITERATIONS cold-start iterations (template=$TEMPLATE)"
echo "  (each iteration: POST /sandboxes + POST .../process/execute + DELETE)"
echo ""

total_e2e=0
min_e2e=999999
max_e2e=0
samples=()

for (( i=1; i<=ITERATIONS; i++ )); do
  ms="$(run_once)"
  samples+=("$ms")
  total_e2e=$(( total_e2e + ms ))
  if (( ms < min_e2e )); then min_e2e=$ms; fi
  if (( ms > max_e2e )); then max_e2e=$ms; fi
  printf "  iter %3d: %d ms\n" "$i" "$ms"
done

# ---- after expvar snapshot --------------------------------------------------
after_json="$(snapshot_vars)"

# ---- compute statistics -----------------------------------------------------
mean_e2e=$(( total_e2e / ITERATIONS ))

# Median via sort + pick middle element
sorted=( $( printf '%s\n' "${samples[@]}" | sort -n ) )
median_idx=$(( ITERATIONS / 2 ))
median_e2e="${sorted[$median_idx]}"

# p90
p90_idx=$(( ITERATIONS * 9 / 10 ))
p90_e2e="${sorted[$p90_idx]}"

echo ""
echo "════════════════════════════════════════"
echo "  Cold-start end-to-end latency (POST /sandboxes → exec \"true\" returns)"
printf "  p50 (median) : %d ms\n" "$median_e2e"
printf "  p90          : %d ms\n" "$p90_e2e"
printf "  mean         : %d ms\n" "$mean_e2e"
printf "  min          : %d ms\n" "$min_e2e"
printf "  max          : %d ms\n" "$max_e2e"
echo ""

# ---- per-phase breakdown from runtime expvars --------------------------------
count_delta="$(delta_ms "$before_json" "$after_json" cold_phase_count)"
if [[ "$count_delta" == "0" || "$count_delta" == "-"* ]]; then
  echo "  Per-phase breakdown: no new cold boots recorded in runtime expvars."
  echo "  (Sandboxes were likely served from the warm pool — re-run with a"
  echo "   depleted pool or --warmup 0 to force cold boots.)"
else
  orch_ms="$(  delta_ms "$before_json" "$after_json" cold_phase_orchestration_ms_sum)"
  vmm_ms="$(   delta_ms "$before_json" "$after_json" cold_phase_vmm_setup_ms_sum)"
  kernel_ms="$(delta_ms "$before_json" "$after_json" cold_phase_kernel_boot_ms_sum)"
  output_ms="$(delta_ms "$before_json" "$after_json" cold_phase_first_output_ms_sum)"
  total_ms="$( delta_ms "$before_json" "$after_json" cold_phase_total_ms_sum)"

  mean_orch=$(  echo "scale=1; $orch_ms   / $count_delta" | bc )
  mean_vmm=$(   echo "scale=1; $vmm_ms    / $count_delta" | bc )
  mean_kernel=$( echo "scale=1; $kernel_ms / $count_delta" | bc )
  mean_output=$( echo "scale=1; $output_ms / $count_delta" | bc )
  mean_total=$(  echo "scale=1; $total_ms  / $count_delta" | bc )

  echo "  Per-phase breakdown (mean over $count_delta cold boot(s)):"
  printf "    orchestration (rootfs+initrd+disk) : %6.1f ms\n" "$mean_orch"
  printf "    vmm_setup  (KVM_CREATE_VM + load)  : %6.1f ms\n" "$mean_vmm"
  printf "    kernel_boot (KVM_RUN → first byte) : %6.1f ms\n" "$mean_kernel"
  printf "    first_output → exec-ready          : %6.1f ms\n" "$mean_output"
  printf "    total (runtime internal)           : %6.1f ms\n" "$mean_total"
fi

echo "════════════════════════════════════════"
echo ""

# ---- target check -----------------------------------------------------------
TARGET_MS=100
if (( median_e2e <= TARGET_MS )); then
  echo "✓ p50 ${median_e2e}ms ≤ target ${TARGET_MS}ms — PASS"
  exit 0
else
  echo "✗ p50 ${median_e2e}ms > target ${TARGET_MS}ms — needs optimization"
  echo ""
  echo "  Optimization hints (check per-phase breakdown above):"
  echo "    orchestration high → rootfs rebuild on every run; check artifact cache"
  echo "    vmm_setup high     → KVM setup or kernel decompression; try minimal kernel"
  echo "    kernel_boot high   → guest kernel device probe; use CONFIG with only virtio-mmio"
  echo "    first_output high  → guest init doing too much before exec agent"
  exit 1
fi
