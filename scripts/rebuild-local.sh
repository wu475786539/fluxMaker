#!/usr/bin/env bash

set -Eeuo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"

if docker compose version >/dev/null 2>&1; then
    COMPOSE=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE=(docker-compose)
else
    printf '未找到 Docker Compose。请先安装 docker compose 或 docker-compose。\n' >&2
    exit 1
fi

commit="$(git rev-parse --short=12 HEAD 2>/dev/null || printf 'unknown')"
dirty=""
if [[ -n "$(git status --porcelain --untracked-files=no 2>/dev/null || true)" ]]; then
    dirty="-dirty"
fi
build_time="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
build_stamp="$(date -u +%Y%m%dT%H%M%SZ)"

export FLUXMAKER_BUILD_COMMIT="${FLUXMAKER_BUILD_COMMIT:-$commit$dirty}"
export FLUXMAKER_BUILD_TIME="${FLUXMAKER_BUILD_TIME:-$build_time}"
export FLUXMAKER_BUILD_ID="${FLUXMAKER_BUILD_ID:-$commit$dirty-$build_stamp}"

wait_for_service_health() {
    local service="$1" container_id="" status="" attempt
    for attempt in $(seq 1 60); do
        container_id="$("${COMPOSE[@]}" ps -q "$service" 2>/dev/null || true)"
        if [[ -n "$container_id" ]]; then
            status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container_id" 2>/dev/null || true)"
            case "$status" in
                healthy|running)
                    printf '%s 已就绪。\n' "$service"
                    return 0
                    ;;
                exited|dead)
                    printf '%s 启动失败，状态=%s。\n' "$service" "$status" >&2
                    "${COMPOSE[@]}" logs --tail=100 "$service" >&2 || true
                    return 1
                    ;;
            esac
        fi
        sleep 2
    done
    printf '等待 %s 健康超时，最后状态=%s。\n' "$service" "${status:-missing}" >&2
    "${COMPOSE[@]}" logs --tail=100 "$service" >&2 || true
    return 1
}

printf '准备构建 FluxMaker：code_build=%s commit=%s built_at=%s\n' \
    "$FLUXMAKER_BUILD_ID" "$FLUXMAKER_BUILD_COMMIT" "$FLUXMAKER_BUILD_TIME"

"${COMPOSE[@]}" config -q

# 三个应用服务共用同一个镜像。先完整构建并打好标签；构建失败时，当前运行
# 容器完全不受影响。随后 force-recreate 会删除旧应用容器并创建新容器。
"${COMPOSE[@]}" build admin-api

# --no-deps 只适合 PostgreSQL、Redis 已经健康的情况。本地 Docker/Colima
# 重启或执行过 stop 后，数据容器可能处于 Exited(0)；先恢复并等待依赖健康，
# 避免 admin-api 在 PostgreSQL 认证阶段收到 EOF 后陷入重启循环。
"${COMPOSE[@]}" up -d --no-build postgres redis
wait_for_service_health postgres
wait_for_service_health redis

if ! "${COMPOSE[@]}" up -d --no-build --no-deps --force-recreate --remove-orphans \
    admin-api fluxmaker watchdog; then
    printf '\n应用容器启动失败，当前状态：\n' >&2
    "${COMPOSE[@]}" ps -a >&2 || "${COMPOSE[@]}" ps >&2 || true
    printf '\n最近启动日志：\n' >&2
    "${COMPOSE[@]}" logs --tail=150 admin-api fluxmaker watchdog >&2 || true
    exit 1
fi

printf '\n应用容器已强制替换；PostgreSQL、Redis 和所有数据卷均已保留。\n'
"${COMPOSE[@]}" ps
printf '\n核对新代码版本：\n'
"${COMPOSE[@]}" logs --since=2m --tail=20 admin-api fluxmaker watchdog
