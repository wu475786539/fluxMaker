#!/usr/bin/env bash

set -Eeuo pipefail

ARCHIVE="${1:-}"
EXPECTED_SHA256="${2:-}"
DEPLOY_ROOT="${3:-}"
RELEASE_ID="${4:-}"
BACKEND_IMPL="${5:-}"
ADMIN_PORT="${6:-}"
UPLOADED_ENV="${7:--}"
KEEP_RELEASES="${8:-3}"
RELEASE_DIR="$DEPLOY_ROOT/releases/$RELEASE_ID"
SHARED_DIR="$DEPLOY_ROOT/shared"
ENV_FILE="$SHARED_DIR/.env"
CURRENT_LINK="$DEPLOY_ROOT/current"
GENERATED_ADMIN_PASSWORD=""

fail() {
    printf '服务器部署失败：%s\n' "$*" >&2
    exit 1
}

log() {
    printf '\n[server] %s\n' "$*"
}

cleanup_upload() {
    case "$ARCHIVE" in
        /tmp/fluxmaker-deploy-*/source.tgz)
            rm -rf "$(dirname "$ARCHIVE")"
            ;;
    esac
}
trap cleanup_upload EXIT

[[ "$(id -u)" == "0" ]] || fail "远程部署脚本必须以 root 运行"
[[ -f "$ARCHIVE" ]] || fail "发布包不存在"
[[ "$EXPECTED_SHA256" =~ ^[0-9a-fA-F]{64}$ ]] || fail "发布包校验值无效"
[[ "$DEPLOY_ROOT" =~ ^/[A-Za-z0-9._/-]+$ ]] || fail "部署目录无效"
[[ "$RELEASE_ID" =~ ^[0-9]{14}-[0-9]+$ ]] || fail "发布编号无效"
[[ "$BACKEND_IMPL" == "java" || "$BACKEND_IMPL" == "go" ]] || fail "后台实现无效"
[[ "$ADMIN_PORT" =~ ^[0-9]+$ ]] && (( ADMIN_PORT >= 1 && ADMIN_PORT <= 65535 )) || fail "后台端口无效"
[[ "$KEEP_RELEASES" =~ ^[0-9]+$ ]] && (( KEEP_RELEASES >= 1 && KEEP_RELEASES <= 20 )) || fail "保留发布数无效"

install_base_packages() {
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl openssl tar
    elif command -v dnf >/dev/null 2>&1; then
        dnf -y install ca-certificates curl openssl tar
    elif command -v yum >/dev/null 2>&1; then
        yum -y install ca-certificates curl openssl tar
    else
        fail "不支持当前 Linux 包管理器，请使用 Ubuntu/Debian/CentOS/RHEL/Rocky/AlmaLinux"
    fi
}

install_docker_apt() {
    # Docker 官方仓库同时提供 Engine、Buildx 和 Compose 插件。
    . /etc/os-release
    case "${ID:-}" in
        ubuntu|debian) docker_distribution="$ID" ;;
        *) fail "当前 apt 系统不受支持：${ID:-unknown}" ;;
    esac
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL "https://download.docker.com/linux/$docker_distribution/gpg" -o /etc/apt/keyrings/docker.asc
    chmod a+r /etc/apt/keyrings/docker.asc
    architecture="$(dpkg --print-architecture)"
    codename="${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}"
    [[ -n "$codename" ]] || fail "无法识别系统版本代号"
    cat > /etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/$docker_distribution
Suites: $codename
Components: stable
Architectures: $architecture
Signed-By: /etc/apt/keyrings/docker.asc
EOF
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

install_docker_rpm() {
    . /etc/os-release
    case "${ID:-}" in
        centos|rhel) docker_distribution="$ID" ;;
        rocky|almalinux) docker_distribution="centos" ;;
        *) fail "当前 RPM 系统不受支持：${ID:-unknown}" ;;
    esac
    if command -v dnf >/dev/null 2>&1; then
        package_manager="dnf"
        dnf -y install dnf-plugins-core
    else
        package_manager="yum"
        yum -y install yum-utils
    fi
    "$package_manager" config-manager --add-repo "https://download.docker.com/linux/$docker_distribution/docker-ce.repo"
    "$package_manager" -y install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

ensure_docker() {
    if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
        log "安装 Docker Engine、Buildx 和 Docker Compose"
        if command -v apt-get >/dev/null 2>&1; then
            install_docker_apt
        else
            install_docker_rpm
        fi
    fi
    if command -v systemctl >/dev/null 2>&1; then
        systemctl enable --now docker
    else
        service docker start
    fi
    docker info >/dev/null
    docker compose version >/dev/null
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

env_value() {
    local key="$1" file="$2"
    [[ -f "$file" ]] || return 0
    sed -n "s/^${key}=//p" "$file" | tail -n 1
}

set_env_value() {
    local key="$1" value="$2" file="$3" temporary
    temporary="$(mktemp)"
    if [[ -f "$file" ]]; then
        grep -v "^${key}=" "$file" > "$temporary" || true
    fi
    printf '%s=%s\n' "$key" "$value" >> "$temporary"
    install -m 0600 "$temporary" "$file"
    rm -f "$temporary"
}

ensure_secret() {
    local key="$1" generator="$2" current
    current="$(env_value "$key" "$ENV_FILE")"
    if [[ -z "$current" || "$current" == replace-with-* ]]; then
        current="$(eval "$generator")"
        set_env_value "$key" "$current" "$ENV_FILE"
        if [[ "$key" == "ADMIN_PASSWORD" ]]; then
            GENERATED_ADMIN_PASSWORD="$current"
        fi
    fi
}

# 这些密钥一旦生成就不能变:数据卷和已有凭证都依赖它们。
GUARDED_SECRETS=(CREDENTIAL_MASTER_KEY POSTGRES_PASSWORD REDIS_PASSWORD)

guarded_reason() {
    case "$1" in
        CREDENTIAL_MASTER_KEY) printf '会使已有交易所凭证无法解密' ;;
        POSTGRES_PASSWORD)     printf '数据卷仍是旧密码,应用将连不上数据库' ;;
        REDIS_PASSWORD)        printf '数据卷仍是旧密码,应用将连不上 Redis' ;;
        *)                     printf '会破坏已有数据' ;;
    esac
}

prepare_environment() {
    mkdir -p "$SHARED_DIR"
    if [[ "$UPLOADED_ENV" != "-" ]]; then
        [[ -f "$UPLOADED_ENV" ]] || fail "指定的环境文件没有上传成功"
        local previous_env="" key old_value new_value
        if [[ -f "$ENV_FILE" ]]; then
            previous_env="$(mktemp)"
            install -m 0600 "$ENV_FILE" "$previous_env"     # 先备份现有 .env
            # 上传的 env 若把受保护密钥改成了不同的非空值,直接拒绝。
            for key in "${GUARDED_SECRETS[@]}"; do
                old_value="$(env_value "$key" "$previous_env")"
                new_value="$(env_value "$key" "$UPLOADED_ENV")"
                if [[ -n "$old_value" && -n "$new_value" && "$old_value" != "$new_value" ]]; then
                    rm -f "$previous_env"
                    fail "拒绝替换 $key（$(guarded_reason "$key")）；请从上传的环境文件中移除该项或改回原值"
                fi
            done
        fi
        install -m 0600 "$UPLOADED_ENV" "$ENV_FILE"
        # 上传的 env 即使漏写了受保护密钥,也用原值补回,避免被后面重新随机生成。
        if [[ -n "$previous_env" ]]; then
            for key in "${GUARDED_SECRETS[@]}"; do
                old_value="$(env_value "$key" "$previous_env")"
                [[ -n "$old_value" ]] && set_env_value "$key" "$old_value" "$ENV_FILE"
            done
            rm -f "$previous_env"
        fi
    elif [[ ! -f "$ENV_FILE" ]]; then
        install -m 0600 /dev/null "$ENV_FILE"
    fi

    ensure_secret POSTGRES_PASSWORD "openssl rand -hex 32"
    ensure_secret REDIS_PASSWORD "openssl rand -hex 32"
    ensure_secret ADMIN_PASSWORD "openssl rand -hex 20"
    ensure_secret CREDENTIAL_MASTER_KEY "openssl rand -base64 32"
    ensure_secret METRICS_TOKEN "openssl rand -hex 32"

    [[ -n "$(env_value ADMIN_EMAIL "$ENV_FILE")" ]] || set_env_value ADMIN_EMAIL admin@fluxmaker.local "$ENV_FILE"
    [[ -n "$(env_value LOG_MAX_SIZE "$ENV_FILE")" ]] || set_env_value LOG_MAX_SIZE 20m "$ENV_FILE"
    [[ -n "$(env_value LOG_MAX_FILES "$ENV_FILE")" ]] || set_env_value LOG_MAX_FILES 5 "$ENV_FILE"
    [[ -n "$(env_value LOG_LEVEL "$ENV_FILE")" ]] || set_env_value LOG_LEVEL info "$ENV_FILE"
    [[ -n "$(env_value FLUXMAKER_ENABLE_LIVE_TRADING "$ENV_FILE")" ]] || \
        set_env_value FLUXMAKER_ENABLE_LIVE_TRADING DISABLED "$ENV_FILE"
    set_env_value BACKEND_IMPL "$BACKEND_IMPL" "$ENV_FILE"
    set_env_value ADMIN_PORT "$ADMIN_PORT" "$ENV_FILE"
    chmod 0600 "$ENV_FILE"
}

open_firewall_port() {
    if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
        ufw allow "$ADMIN_PORT/tcp" >/dev/null
    fi
    if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
        firewall-cmd --permanent --add-port="$ADMIN_PORT/tcp" >/dev/null
        firewall-cmd --reload >/dev/null
    fi
}

compose() {
    local image_tag="$1"
    shift
    IMAGE_TAG="$image_tag" docker compose --env-file "$ENV_FILE" -f "$RELEASE_DIR/compose.yaml" "$@"
}

wait_until_ready() {
    local attempts=0
    while (( attempts < 100 )); do
        if curl -fsS --max-time 5 "http://127.0.0.1:$ADMIN_PORT/readyz" >/dev/null 2>&1; then
            return 0
        fi
        attempts=$((attempts + 1))
        sleep 3
    done
    return 1
}

rollback() {
    local previous="$1"
    [[ -n "$previous" && -d "$previous" ]] || return 0
    previous_tag="$(basename "$previous")"
    log "新版本启动失败，自动恢复上一版本 $previous_tag"
    RELEASE_DIR="$previous" compose "$previous_tag" up -d --no-build --remove-orphans || true
}

remove_release_images() {
    local release_id="$1" image
    for image in "fluxmaker-java:$release_id" "fluxmaker-go:$release_id"; do
        if docker image inspect "$image" >/dev/null 2>&1; then
            if docker image rm "$image" >/dev/null 2>&1; then
                printf '[server] 已清理旧镜像：%s\n' "$image"
            else
                printf '[server] 跳过仍被容器使用的镜像：%s\n' "$image"
            fi
        fi
    done
}

discard_failed_release() {
    local previous="$1" current
    if [[ -z "$previous" ]]; then
        compose "$RELEASE_ID" down --remove-orphans >/dev/null 2>&1 || true
    fi
    current="$(readlink -f "$CURRENT_LINK" 2>/dev/null || true)"
    if [[ "$RELEASE_DIR" != "$current" ]]; then
        rm -rf "$RELEASE_DIR"
        remove_release_images "$RELEASE_ID"
    fi
}

prune_releases() {
    local index=0 candidate candidate_id current
    current="$(readlink -f "$CURRENT_LINK" 2>/dev/null || true)"
    while IFS= read -r candidate_id; do
        if (( index >= KEEP_RELEASES )); then
            candidate="$DEPLOY_ROOT/releases/$candidate_id"
            if [[ "$candidate" != "$current" ]]; then
                rm -rf "$candidate"
                remove_release_images "$candidate_id"
                printf '[server] 已清理旧发布目录：%s\n' "$candidate"
            fi
        fi
        index=$((index + 1))
    done < <(find "$DEPLOY_ROOT/releases" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; | sort -r)
}

prune_build_cache() {
    # Build cache is never runtime data. Keep recent cache for fast rollouts and
    # clean old unused entries while retaining a useful cache budget.
    if docker builder prune --force --filter "until=168h" --keep-storage 2GB >/dev/null 2>&1; then
        printf '[server] 已清理 7 天前的未使用构建缓存，并保留近期构建缓存。\n'
    else
        printf '[server] 构建缓存清理失败，部署结果不受影响。\n' >&2
    fi
}

log "安装基础工具并校验发布包"
install_base_packages
actual_sha256="$(sha256_of "$ARCHIVE")"
[[ "$actual_sha256" == "$EXPECTED_SHA256" ]] || fail "发布包 SHA-256 校验失败"
ensure_docker

previous_release="$(readlink -f "$CURRENT_LINK" 2>/dev/null || true)"
mkdir -p "$RELEASE_DIR"
tar -xzf "$ARCHIVE" -C "$RELEASE_DIR"
[[ -f "$RELEASE_DIR/compose.yaml" ]] || fail "发布包缺少 compose.yaml"

log "准备生产环境变量"
prepare_environment
open_firewall_port

log "检查 Compose 配置"
compose "$RELEASE_ID" config -q

log "构建镜像并启动服务；首次 Java 构建可能需要数分钟"
if ! compose "$RELEASE_ID" up -d --build --remove-orphans; then
    compose "$RELEASE_ID" logs --tail=200 || true
    rollback "$previous_release"
    discard_failed_release "$previous_release"
    fail "Docker Compose 启动失败"
fi

log "等待 PostgreSQL、Redis 和 Admin API 就绪"
if ! wait_until_ready; then
    compose "$RELEASE_ID" ps || true
    compose "$RELEASE_ID" logs --tail=200 admin-api fluxmaker watchdog || true
    rollback "$previous_release"
    discard_failed_release "$previous_release"
    fail "服务在 5 分钟内没有就绪"
fi

ln -sfn "$RELEASE_DIR" "$CURRENT_LINK"
prune_releases
prune_build_cache

log "服务状态"
compose "$RELEASE_ID" ps
printf '\n[server] 健康检查通过：http://127.0.0.1:%s/readyz\n' "$ADMIN_PORT"
printf '[server] 后台账号：%s\n' "$(env_value ADMIN_EMAIL "$ENV_FILE")"
if [[ -n "$GENERATED_ADMIN_PASSWORD" ]]; then
    printf '[server] 首次生成的后台密码：%s\n' "$GENERATED_ADMIN_PASSWORD"
    printf '[server] 请立即保存密码；后续部署不会再次生成。\n'
fi
