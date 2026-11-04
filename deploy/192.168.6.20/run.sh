#!/usr/bin/env bash
set -Eeuo pipefail
cd /www2/gowvp-v1

compose() {
  docker compose --project-name owl-v1 --env-file .env --file docker-compose.yml "$@"
}

case "${1:-status}" in
  up)
    ./check-storage.sh
    compose config --quiet
    compose up -d --no-build --pull never --wait --wait-timeout 180
    compose ps
    ;;
  status) compose ps ;;
  logs) compose logs --tail=100 "${@:2}" ;;
  down) compose down ;;
  *) printf 'Usage: %s up|status|logs [service]|down\n' "$0" >&2; exit 1 ;;
esac
