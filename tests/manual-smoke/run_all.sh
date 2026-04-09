#!/usr/bin/env bash

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

source "$REPO_ROOT/tests/manual-smoke/lib.sh"

init_smoke_env
require_cmd go
require_cmd sudo
require_cmd debugfs
require_cmd script
require_cmd timeout
require_cmd curl
require_cmd ip
require_cmd pgrep
require_kernel
require_sudo_ready
build_gocracker
build_smoke_pty_helper

run_case images alpine-latest run_interactive_image_case alpine-latest alpine:latest
run_case images busybox-latest run_interactive_image_case busybox-latest busybox:latest
run_case images ubuntu-22-04 run_interactive_image_case ubuntu-22-04 ubuntu:22.04

run_case images nginx-latest run_nginx_case
run_case images python-override run_override_case

run_case dockerfiles hello-world run_dockerfile_service_case \
  hello-world \
  "$REPO_ROOT/tests/examples/hello-world/Dockerfile" \
  "$REPO_ROOT/tests/examples/hello-world"

run_case dockerfiles static-site run_dockerfile_service_case \
  static-site \
  "$REPO_ROOT/tests/examples/static-site/Dockerfile" \
  "$REPO_ROOT/tests/examples/static-site"

run_case dockerfiles python-api run_dockerfile_service_case \
  python-api \
  "$REPO_ROOT/tests/examples/python-api/Dockerfile" \
  "$REPO_ROOT/tests/examples/python-api"

run_case extras shellform run_shellform_fixture_case
run_case extras user-fixture run_user_fixture_case

run_case compose compose-basic run_compose_basic_case
run_case compose compose-health run_compose_health_case
run_case compose compose-volume run_compose_volume_case
run_case compose compose-todo-postgres run_compose_todo_postgres_case

run_case exec compose-exec run_compose_exec_case

print_summary
