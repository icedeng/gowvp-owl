#!/usr/bin/env bash
set -euo pipefail
repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$repo_dir"
# 使用实际编译依赖闭包，包含传递依赖；不靠目录名推断。
deps=$(go list -deps -f '{{.ImportPath}}' ./extensions/accessgateway/gateway/cmd/owl-gateway)
forbidden=$(awk '/^github.com\/gowvp\/owl\/(internal\/|pkg\/|extensions\/accessgateway\/(platform\/|integration\/))/ || /^gorm.io\// {print}' <<< "$deps")
if [[ -n "$forbidden" ]]; then
  printf 'B 依赖了禁止的 A 业务包：\n%s\n' "$forbidden" >&2
  exit 1
fi
printf '%s\n' 'B 依赖边界检查通过（没有 A 业务或 GORM）。'
