#!/usr/bin/env bash

set -Eeuo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE_DIR="/opt/fluxmaker"
BACKEND_IMPL="${BACKEND_IMPL:-java}"
ADMIN_PORT="${ADMIN_PORT:-8080}"
SSH_PORT="22"
IDENTITY_FILE=""
ENV_FILE=""
KEEP_RELEASES="3"
TARGET=""
LOCAL_ARCHIVE=""
CONTROL_PATH="/tmp/fluxmaker-ssh-$$"

usage() {
    cat <<'EOF'
用法：
  ./deploy.sh [选项] <SSH用户@服务器IP>

示例：
  ./deploy.sh root@1.2.3.4
  ./deploy.sh --identity ~/.ssh/server.pem ubuntu@1.2.3.4
  ./deploy.sh --env-file .env --port 8080 root@1.2.3.4

选项：
  --backend java|go     部署的后台实现，默认 java
  --env-file FILE       首次部署时上传指定环境文件；不传则自动生成安全配置
  --identity FILE       SSH 私钥
  --ssh-port PORT       SSH 端口，默认 22
  --port PORT           后台访问端口，默认 8080
  --remote-dir DIR      服务器部署目录，默认 /opt/fluxmaker
  --keep-releases N     保留的发布目录数量，默认 3
  -h, --help            显示帮助

服务器要求：Ubuntu、Debian、CentOS、RHEL、Rocky Linux 或 AlmaLinux，
登录账号必须是 root 或拥有 sudo 权限。脚本会自动安装 Docker Engine、
Docker Compose 和其他必要工具，然后打包、上传、构建、启动并检查服务。
EOF
}

fail() {
    printf '部署失败：%s\n' "$*" >&2
    exit 1
}

log() {
    printf '\n==> %s\n' "$*"
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "本机缺少命令：$1"
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --backend)
            [[ $# -ge 2 ]] || fail "--backend 缺少参数"
            BACKEND_IMPL="$2"
            shift 2
            ;;
        --env-file)
            [[ $# -ge 2 ]] || fail "--env-file 缺少参数"
            ENV_FILE="$2"
            shift 2
            ;;
        --identity)
            [[ $# -ge 2 ]] || fail "--identity 缺少参数"
            IDENTITY_FILE="$2"
            shift 2
            ;;
        --ssh-port)
            [[ $# -ge 2 ]] || fail "--ssh-port 缺少参数"
            SSH_PORT="$2"
            shift 2
            ;;
        --port)
            [[ $# -ge 2 ]] || fail "--port 缺少参数"
            ADMIN_PORT="$2"
            shift 2
            ;;
        --remote-dir)
            [[ $# -ge 2 ]] || fail "--remote-dir 缺少参数"
            REMOTE_DIR="$2"
            shift 2
            ;;
        --keep-releases)
            [[ $# -ge 2 ]] || fail "--keep-releases 缺少参数"
            KEEP_RELEASES="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        --)
            shift
            break
            ;;
        -*)
            fail "未知选项：$1"
            ;;
        *)
            [[ -z "$TARGET" ]] || fail "只能指定一台服务器"
            TARGET="$1"
            shift
            ;;
    esac
done

[[ -n "$TARGET" ]] || { usage; exit 2; }
[[ "$BACKEND_IMPL" == "java" || "$BACKEND_IMPL" == "go" ]] || fail "--backend 只能是 java 或 go"
[[ "$ADMIN_PORT" =~ ^[0-9]+$ ]] && (( ADMIN_PORT >= 1 && ADMIN_PORT <= 65535 )) || fail "后台端口必须是 1-65535"
[[ "$SSH_PORT" =~ ^[0-9]+$ ]] && (( SSH_PORT >= 1 && SSH_PORT <= 65535 )) || fail "SSH 端口必须是 1-65535"
[[ "$KEEP_RELEASES" =~ ^[0-9]+$ ]] && (( KEEP_RELEASES >= 1 && KEEP_RELEASES <= 20 )) || fail "保留发布数必须是 1-20"
[[ "$REMOTE_DIR" =~ ^/[A-Za-z0-9._/-]+$ ]] || fail "远程目录只能使用绝对路径及字母、数字、点、下划线、横线"
[[ "$TARGET" =~ ^[A-Za-z0-9._-]+@([A-Za-z0-9._:-]+|\[[0-9A-Fa-f:]+\])$ ]] || fail "服务器格式应为 SSH用户@服务器IP"
[[ -f "$PROJECT_ROOT/Dockerfile-$BACKEND_IMPL" ]] || fail "找不到 Dockerfile-$BACKEND_IMPL"
[[ -f "$PROJECT_ROOT/scripts/deploy-remote.sh" ]] || fail "找不到 scripts/deploy-remote.sh"

if [[ -n "$ENV_FILE" ]]; then
    [[ -f "$ENV_FILE" ]] || fail "环境文件不存在：$ENV_FILE"
    ENV_FILE="$(cd "$(dirname "$ENV_FILE")" && pwd)/$(basename "$ENV_FILE")"
fi
if [[ -n "$IDENTITY_FILE" ]]; then
    [[ -f "$IDENTITY_FILE" ]] || fail "SSH 私钥不存在：$IDENTITY_FILE"
    IDENTITY_FILE="$(cd "$(dirname "$IDENTITY_FILE")" && pwd)/$(basename "$IDENTITY_FILE")"
fi

require_command ssh
require_command tar

SSH_ARGS=(
    -p "$SSH_PORT"
    -o ServerAliveInterval=30
    -o ServerAliveCountMax=10
    -o ControlMaster=auto
    -o ControlPersist=5m
    -o "ControlPath=$CONTROL_PATH"
)
if [[ -n "$IDENTITY_FILE" ]]; then
    SSH_ARGS+=(-i "$IDENTITY_FILE")
fi

cleanup() {
    if [[ -n "$LOCAL_ARCHIVE" && -f "$LOCAL_ARCHIVE" ]]; then
        rm -f "$LOCAL_ARCHIVE"
    fi
    ssh "${SSH_ARGS[@]}" -O exit "$TARGET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

RELEASE_ID="$(date -u +%Y%m%d%H%M%S)-$$"
REMOTE_TMP="/tmp/fluxmaker-deploy-$RELEASE_ID"
REMOTE_ARCHIVE="$REMOTE_TMP/source.tgz"
REMOTE_SCRIPT="$REMOTE_TMP/deploy-remote.sh"
REMOTE_ENV="$REMOTE_TMP/deploy.env"

LOCAL_ARCHIVE="$(mktemp "${TMPDIR:-/tmp}/fluxmaker-source.XXXXXX")"
log "打包当前代码（不包含 .git、.env、构建产物和运行数据）"
# COPYFILE_DISABLE=1 阻止 macOS tar 生成 ._ AppleDouble 伴随文件。
COPYFILE_DISABLE=1 tar -C "$PROJECT_ROOT" \
    --exclude='./.git' \
    --exclude='./.env' \
    --exclude='./.env.*' \
    --exclude='./java/target' \
    --exclude='./var' \
    --exclude='./backups' \
    --exclude='./.idea' \
    --exclude='./.claude' \
    --exclude='./.codex' \
    --exclude='./.tmp' \
    --exclude='./configs' \
    --exclude='.DS_Store' \
    --exclude='._*' \
    -czf "$LOCAL_ARCHIVE" .

if command -v shasum >/dev/null 2>&1; then
    ARCHIVE_SHA256="$(shasum -a 256 "$LOCAL_ARCHIVE" | awk '{print $1}')"
elif command -v sha256sum >/dev/null 2>&1; then
    ARCHIVE_SHA256="$(sha256sum "$LOCAL_ARCHIVE" | awk '{print $1}')"
else
    fail "本机缺少 shasum 或 sha256sum"
fi

log "连接服务器并上传发布包"
ssh "${SSH_ARGS[@]}" "$TARGET" "mkdir -p '$REMOTE_TMP' && chmod 700 '$REMOTE_TMP'"
ssh "${SSH_ARGS[@]}" "$TARGET" "cat > '$REMOTE_ARCHIVE'" < "$LOCAL_ARCHIVE"
ssh "${SSH_ARGS[@]}" "$TARGET" "cat > '$REMOTE_SCRIPT' && chmod 700 '$REMOTE_SCRIPT'" < "$PROJECT_ROOT/scripts/deploy-remote.sh"
if [[ -n "$ENV_FILE" ]]; then
    ssh "${SSH_ARGS[@]}" "$TARGET" "umask 077; cat > '$REMOTE_ENV'" < "$ENV_FILE"
else
    REMOTE_ENV="-"
fi

REMOTE_UID="$(ssh "${SSH_ARGS[@]}" "$TARGET" 'id -u' | tr -d '\r\n')"
REMOTE_ARGS=(
    "$REMOTE_ARCHIVE"
    "$ARCHIVE_SHA256"
    "$REMOTE_DIR"
    "$RELEASE_ID"
    "$BACKEND_IMPL"
    "$ADMIN_PORT"
    "$REMOTE_ENV"
    "$KEEP_RELEASES"
)
REMOTE_COMMAND="bash '$REMOTE_SCRIPT'"
for value in "${REMOTE_ARGS[@]}"; do
    REMOTE_COMMAND+=" '$value'"
done

log "自动准备服务器环境并部署 $BACKEND_IMPL 后台"
if [[ "$REMOTE_UID" == "0" ]]; then
    ssh "${SSH_ARGS[@]}" "$TARGET" "$REMOTE_COMMAND"
else
    ssh -tt "${SSH_ARGS[@]}" "$TARGET" "sudo $REMOTE_COMMAND"
fi

ACCESS_HOST="${TARGET#*@}"
if [[ "$ACCESS_HOST" == *:* && "$ACCESS_HOST" != \[*\] ]]; then
    ACCESS_HOST="[$ACCESS_HOST]"
fi

printf '\n部署成功。\n'
printf '访问地址：http://%s:%s\n' "$ACCESS_HOST" "$ADMIN_PORT"
printf '服务器目录：%s/current\n' "$REMOTE_DIR"
printf '查看日志：ssh -p %s %s "cd %s/current && docker compose --env-file %s/shared/.env logs -f --tail=200"\n' \
    "$SSH_PORT" "$TARGET" "$REMOTE_DIR" "$REMOTE_DIR"
printf '\n如果公网无法打开，请在云服务器安全组中放行 TCP %s。\n' "$ADMIN_PORT"
printf '当前是 HTTP 临时访问；配置 HTTPS 前请把安全组来源限制为你的固定 IP，并保持实盘开关关闭。\n'
