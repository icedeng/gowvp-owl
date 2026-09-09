#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.yml"
ENV_FILE="${SCRIPT_DIR}/.env"
PROJECT_NAME="${COMPOSE_PROJECT_NAME:-owl-v1}"
COMMAND="${1:-release}"

log() {
  printf '\n[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

fail() {
  printf '错误: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "缺少命令: $1"
}

env_value() {
  local key="$1"
  local value="${!key-}"
  if [[ -n "$value" ]]; then
    printf '%s' "$value"
    return
  fi
  [[ -f "$ENV_FILE" ]] || return
  awk -v key="$key" 'index($0, key "=") == 1 { print substr($0, length(key) + 2); exit }' "$ENV_FILE"
}

generate_secret() {
  openssl rand -hex "$1"
}

detect_arch() {
  local arch="${OWL_TARGET_ARCH:-}"
  if [[ -z "$arch" ]]; then
    arch="$(docker info --format '{{.Architecture}}' 2>/dev/null || uname -m)"
  fi
  case "$arch" in
    amd64|x86_64) printf 'amd64' ;;
    arm64|aarch64) printf 'arm64' ;;
    *) fail "暂不支持目标架构: $arch（仅支持 amd64/arm64）" ;;
  esac
}

compose() {
  docker compose \
    --project-name "$PROJECT_NAME" \
    --project-directory "$SCRIPT_DIR" \
    --env-file "$ENV_FILE" \
    --file "$COMPOSE_FILE" \
    "$@"
}

init_env() {
  require_cmd openssl
  if [[ -e "$ENV_FILE" ]]; then
    fail "$ENV_FILE 已存在，不会覆盖"
  fi

  local media_ip="${2:-CHANGE_ME}"
  local image_tag="v1-$(git -C "$ROOT_DIR" rev-parse --short HEAD 2>/dev/null || printf 'local')"
  umask 077
  printf '%s\n' \
    "OWL_IMAGE_TAG=${image_tag}" \
    "OWL_MEDIA_SDP_IP=${media_ip}" \
    "OWL_MEDIA_SECRET=$(generate_secret 32)" \
    "OWL_ADMIN_USERNAME=admin" \
    "OWL_ADMIN_PASSWORD=$(generate_secret 16)" \
    "OWL_JWT_SECRET=$(generate_secret 32)" \
    "OWL_RTMP_SECRET=$(generate_secret 32)" \
    "OWL_WEBHOOK_SECRET=$(generate_secret 32)" \
    "OWL_SIP_PASSWORD=" \
    "OWL_HTTP_PORT=15123" \
    "OWL_ZLM_HTTP_PORT=8080" >"$ENV_FILE"

  prepare_data_dirs
  log "已生成 $ENV_FILE"
  if [[ "$media_ip" == "CHANGE_ME" ]]; then
    printf '发布前请把 OWL_MEDIA_SDP_IP 改为真机可访问的宿主机局域网地址。\n'
  fi
}

validate_build_env() {
  [[ -f "$ENV_FILE" ]] || fail "缺少 $ENV_FILE，请先执行: $0 init <宿主机局域网IP>"
  local media_secret
  media_secret="$(env_value OWL_MEDIA_SECRET)"
  [[ -n "$media_secret" && "$media_secret" != "CHANGE_ME" ]] || fail "请在 $ENV_FILE 设置 OWL_MEDIA_SECRET"
  [[ ${#media_secret} -ge 32 ]] || fail "OWL_MEDIA_SECRET 长度至少为 32 个字符"
}

validate_runtime_env() {
  validate_build_env
  local media_ip key value min_length
  media_ip="$(env_value OWL_MEDIA_SDP_IP)"
  [[ -n "$media_ip" && "$media_ip" != "CHANGE_ME" ]] || fail "请在 $ENV_FILE 设置真机可访问的 OWL_MEDIA_SDP_IP"
  case "$media_ip" in
    127.*|localhost|zlm|gowvp) fail "OWL_MEDIA_SDP_IP 必须是真机可访问的宿主机地址: $media_ip" ;;
  esac
  for key in OWL_ADMIN_PASSWORD OWL_JWT_SECRET OWL_RTMP_SECRET OWL_WEBHOOK_SECRET; do
    value="$(env_value "$key")"
    min_length=32
    [[ -n "$value" && "$value" != "CHANGE_ME" ]] || fail "请在 $ENV_FILE 设置 $key"
    [[ ${#value} -ge $min_length ]] || fail "$key 长度至少为 $min_length 个字符"
  done
}

prepare_data_dirs() {
  mkdir -p \
    "${SCRIPT_DIR}/data/configs" \
    "${SCRIPT_DIR}/data/logs" \
    "${SCRIPT_DIR}/data/recordings" \
    "${SCRIPT_DIR}/data/zlm-config"

  local config_file="${SCRIPT_DIR}/data/configs/config.toml"
  if [[ -f "$config_file" ]]; then
    chmod 600 "$config_file"
  fi
}

build_artifacts() {
  require_cmd go
  require_cmd pnpm

  local artifact_dir="${ROOT_DIR}/build/deploy-v1/${TARGETARCH}"
  local version branch hash build_time
  version="$(git -C "$ROOT_DIR" describe --tags --always --dirty 2>/dev/null || printf 'v1-local')"
  branch="$(git -C "$ROOT_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null || printf 'unknown')"
  hash="$(git -C "$ROOT_DIR" rev-parse --short HEAD 2>/dev/null || printf 'unknown')"
  build_time="$(date +%s)"

  case "$artifact_dir" in
    "$ROOT_DIR"/build/deploy-v1/*) ;;
    *) fail "非法构建目录: $artifact_dir" ;;
  esac
  rm -rf "$artifact_dir"
  mkdir -p "${artifact_dir}/service" "${artifact_dir}/frontend"

  log "编译 Owl 服务 (${TARGETARCH}, ${version})"
  (
    cd "$ROOT_DIR"
    CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" go build \
      -trimpath \
      -ldflags="-s -w -X main.buildVersion=${version} -X main.gitBranch=${branch} -X main.gitHash=${hash} -X main.buildTime=${build_time} -X main.release=true" \
      -o "${artifact_dir}/service/owl" \
      ./main.go
  )

  log "编译管理前端"
  pnpm --dir "${ROOT_DIR}/admin-ui" install --frozen-lockfile
  pnpm --dir "${ROOT_DIR}/admin-ui" build
  cp -R "${ROOT_DIR}/admin-ui/dist/." "${artifact_dir}/frontend/"
}

build_images() {
  require_cmd docker
  docker compose version >/dev/null
  validate_build_env
  export TARGETARCH
  TARGETARCH="$(detect_arch)"
  build_artifacts
  log "生成本地 Docker 镜像"
  compose build
  compose images
}

start_stack() {
  require_cmd docker
  docker compose version >/dev/null
  validate_runtime_env
  export TARGETARCH
  TARGETARCH="$(detect_arch)"
  prepare_data_dirs
  log "启动 Owl v1 真机测试环境"
  compose up -d --remove-orphans --wait --wait-timeout "${OWL_WAIT_TIMEOUT:-180}"
  compose ps
  log "发布完成，管理端地址: http://127.0.0.1:$(env_value OWL_HTTP_PORT)/"
}

case "$COMMAND" in
  init)
    init_env "$@"
    ;;
  build)
    build_images
    ;;
  up)
    start_stack
    ;;
  release)
    build_images
    start_stack
    ;;
  down)
    validate_build_env
    compose down
    ;;
  status)
    validate_build_env
    compose ps
    ;;
  logs)
    validate_build_env
    if [[ -n "${2:-}" ]]; then
      compose logs -f --tail=200 "$2"
    else
      compose logs -f --tail=200
    fi
    ;;
  *)
    fail "未知命令: ${COMMAND}。可用命令: init [LAN_IP] | build | up | release | down | status | logs [SERVICE]"
    ;;
esac
