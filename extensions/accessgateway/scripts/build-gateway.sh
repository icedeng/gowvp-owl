#!/usr/bin/env bash
set -euo pipefail
repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$repo_dir"
bash extensions/accessgateway/scripts/check-boundaries.sh
version=${GATEWAY_VERSION:-$(cat VERSION)}
commit=$(git rev-parse HEAD)
if [[ -n $(git status --porcelain) ]]; then commit="${commit}-dirty"; fi
# -ldflags 是 Go 链接器参数，只允许版本标识，不接受额外命令/参数。
if [[ ! "$version" =~ ^[A-Za-z0-9._+-]+$ ]]; then printf '%s\n' 'GATEWAY_VERSION 格式无效' >&2; exit 1; fi
output_dir=${GATEWAY_OUTPUT_DIR:-build/accessgateway}
mkdir -p "$output_dir"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.gatewayVersion=${version} -X main.sourceCommit=${commit}" \
  -o "$output_dir/owl-gateway" ./extensions/accessgateway/gateway/cmd/owl-gateway
printf 'gateway_version=%s\nsource_commit=%s\nprotocol_version=1\ncompatible=1\n' "$version" "$commit" > "$output_dir/build-info.txt"
printf '构建完成：%s/owl-gateway\n' "$output_dir"
