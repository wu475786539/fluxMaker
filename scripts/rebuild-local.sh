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

printf '准备构建 FluxMaker：code_build=%s commit=%s built_at=%s\n' \
    "$FLUXMAKER_BUILD_ID" "$FLUXMAKER_BUILD_COMMIT" "$FLUXMAKER_BUILD_TIME"

"${COMPOSE[@]}" config -q

# 三个应用服务共用同一个镜像。先完整构建并打好标签；构建失败时，当前运行
# 容器完全不受影响。随后 force-recreate 会删除旧应用容器并创建新容器。
"${COMPOSE[@]}" build admin-api
"${COMPOSE[@]}" up -d --no-build --no-deps --force-recreate --remove-orphans \
    admin-api fluxmaker watchdog

printf '\n应用容器已强制替换；PostgreSQL、Redis 和所有数据卷均已保留。\n'
"${COMPOSE[@]}" ps
printf '\n核对新代码版本：\n'
"${COMPOSE[@]}" logs --since=2m --tail=20 admin-api fluxmaker watchdog
